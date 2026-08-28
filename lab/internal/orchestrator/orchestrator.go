package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/remote"
)

type EmitFunc func(level, kind, message string, fields map[string]any)
type Orchestrator struct{ Remote remote.Runner }

func shell(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func (o Orchestrator) Check(ctx context.Context, s model.Server) (string, error) {
	return o.Remote.Run(ctx, s, `set -eu
printf 'host=%s\n' "$(hostname)"
printf 'kernel=%s\n' "$(uname -sr)"
printf 'cpus=%s\n' "$(getconf _NPROCESSORS_ONLN)"
printf 'memory_kb=%s\n' "$(awk '/MemTotal/{print $2}' /proc/meminfo)"
printf 'memory_available_kb=%s\n' "$(awk '/MemAvailable/{print $2}' /proc/meminfo)"
printf 'load1=%s\n' "$(awk '{print $1}' /proc/loadavg)"
printf 'disk_total_kb=%s\n' "$(df -Pk . | awk 'NR==2{print $2}')"
printf 'disk_available_kb=%s\n' "$(df -Pk . | awk 'NR==2{print $4}')"
command -v tar >/dev/null
command -v sha256sum >/dev/null
command -v setsid >/dev/null
command -v ss >/dev/null
`)
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (o Orchestrator) Deploy(ctx context.Context, exp *model.Experiment, servers map[string]model.Server, emit EmitFunc) ([]model.Node, error) {
	artifact, err := filepath.Abs(exp.ArtifactPath)
	if err != nil {
		return nil, err
	}
	sha, err := fileSHA(artifact)
	if err != nil {
		return nil, err
	}
	exp.ArtifactSHA = sha
	var nodes []model.Node
	global := 1
	for _, p := range exp.Placements {
		s, ok := servers[p.ServerID]
		if !ok {
			return nil, fmt.Errorf("server %s not found", p.ServerID)
		}
		if p.Count < 1 {
			return nil, fmt.Errorf("server %s node count must be positive", s.Name)
		}
		emit("info", "deploy", "preparing "+s.Name, map[string]any{"nodes": p.Count})
		base := strings.TrimRight(s.WorkDir, "/")
		artifactDir := base + "/artifacts/" + sha
		remoteArchive := base + "/uploads/pfap-runtime-" + sha + ".tar.gz"
		prepare := "set -eu\nmkdir -p " + shell(base+"/uploads") + " " + shell(base+"/artifacts") + " " + shell(base+"/experiments") + "\n"
		if _, err := o.Remote.Run(ctx, s, prepare); err != nil {
			return nil, err
		}
		firstPort := exp.P2PPortBase + global - 1
		lastPort := firstPort + p.Count - 1
		portCheck := fmt.Sprintf("set -eu\nfor port in $(seq %d %d); do if ss -H -ltn \"sport = :$port\" | grep -q .; then echo \"P2P port $port is already in use\" >&2; exit 42; fi; done\n", firstPort, lastPort)
		if out, err := o.Remote.Run(ctx, s, portCheck); err != nil {
			return nil, fmt.Errorf("port preflight on %s: %w (%s)", s.Name, err, strings.TrimSpace(out))
		}
		probe := "test -x " + shell(artifactDir+"/pfap-runtime/bin/geth")
		if _, err := o.Remote.Run(ctx, s, probe); err != nil {
			emit("info", "deploy", "uploading runtime to "+s.Name, map[string]any{"sha256": sha})
			if err := o.Remote.Copy(ctx, s, artifact, remoteArchive); err != nil {
				return nil, err
			}
			script := "set -eu\necho " + shell(sha+"  "+remoteArchive) + " | sha256sum -c -\nmkdir -p " + shell(artifactDir) + "\ntar -xzf " + shell(remoteArchive) + " -C " + shell(artifactDir) + "\n"
			if _, err := o.Remote.Run(ctx, s, script); err != nil {
				return nil, err
			}
		}
		runtime := artifactDir + "/pfap-runtime"
		preflight := "set -eu\nruntime=" + shell(runtime) + "\nLD_LIBRARY_PATH=\"$runtime/lib\" \"$runtime/bin/geth\" version >/dev/null\n"
		if out, err := o.Remote.Run(ctx, s, preflight); err != nil {
			return nil, fmt.Errorf("runtime incompatible on %s: %w (%s); rebuild the runtime on an OS/toolchain compatible with this worker", s.Name, err, strings.TrimSpace(out))
		}
		runtimeDir := base + "/experiments/" + exp.ID + "/" + s.ID
		portOffset := global - 1
		env := fmt.Sprintf("NODE_COUNT=%d NETWORK_ID=%d P2P_PORT_BASE=%d HTTP_PORT_BASE=%d RUNTIME_DIR=%s GETH_BIN=%s PFAP_PRFKEY_DIR=%s LD_LIBRARY_PATH=%s ENABLE_HTTP=false MINE=%t", p.Count, exp.NetworkID, exp.P2PPortBase+portOffset, exp.RPCPortBase+portOffset, shell(runtimeDir), shell(runtime+"/bin/geth"), shell(runtime+"/prfKey"), shell(runtime+"/lib"), global == 1)
		script := "set -eu\nmkdir -p " + shell(runtimeDir) + "\ncd " + shell(runtime+"/pow") + "\n" + env + " ./network.sh start\n"
		if out, err := o.Remote.Run(ctx, s, script); err != nil {
			return nil, fmt.Errorf("start %s: %w (%s)", s.Name, err, out)
		}
		for local := 1; local <= p.Count; local++ {
			nodes = append(nodes, model.Node{ID: fmt.Sprintf("%s-n%d", exp.ID, global), Name: fmt.Sprintf("node-%d", global), ServerID: s.ID, Index: global, LocalIndex: local, P2PPort: exp.P2PPortBase + global - 1, RPCPort: exp.RPCPortBase + global - 1, Status: "running"})
			global++
		}
	}
	for i := range nodes {
		n := nodes[i]
		s := servers[n.ServerID]
		accountOut, err := o.Attach(ctx, *exp, n, s, "eth.accounts[0]")
		if err != nil {
			return nil, fmt.Errorf("read account for %s: %w", n.Name, err)
		}
		nodes[i].Account = strings.Trim(strings.TrimSpace(accountOut), "\"")
	}
	if exp.Topology == "full-mesh" && len(nodes) > 1 {
		type endpoint struct {
			node   model.Node
			server model.Server
			enode  string
		}
		var endpoints []endpoint
		for _, n := range nodes {
			s := servers[n.ServerID]
			out, err := o.Attach(ctx, *exp, n, s, "admin.nodeInfo.enode")
			if err != nil {
				return nil, fmt.Errorf("read enode for %s: %w", n.Name, err)
			}
			enode := strings.Trim(strings.TrimSpace(out), "\"")
			if at := strings.LastIndex(enode, "@"); at >= 0 {
				port := n.P2PPort
				host := s.P2PHost
				if host == "" {
					host = s.Host
				}
				enode = enode[:at+1] + host + ":" + strconv.Itoa(port)
			}
			endpoints = append(endpoints, endpoint{node: n, server: s, enode: enode})
		}
		for i := range endpoints {
			for j := range endpoints {
				if i == j || endpoints[i].server.ID == endpoints[j].server.ID {
					continue
				}
				expr := "admin.addPeer(" + strconv.Quote(endpoints[j].enode) + ")"
				if _, err := o.Attach(ctx, *exp, endpoints[i].node, endpoints[i].server, expr); err != nil {
					return nil, fmt.Errorf("connect %s to %s: %w", endpoints[i].node.Name, endpoints[j].node.Name, err)
				}
			}
		}
		emit("info", "topology", "full-mesh topology ready", map[string]any{"nodes": len(nodes)})
	}
	emit("info", "deploy", "all nodes started", map[string]any{"count": len(nodes), "artifactSha256": sha})
	return nodes, nil
}

func (o Orchestrator) Stop(ctx context.Context, exp model.Experiment, servers map[string]model.Server, emit EmitFunc) error {
	for _, p := range exp.Placements {
		s := servers[p.ServerID]
		base := strings.TrimRight(s.WorkDir, "/")
		script := "set -eu\nroot=" + shell(base+"/experiments/"+exp.ID+"/"+s.ID) + "\nfor f in \"$root\"/node*/geth.pid; do [ -f \"$f\" ] || continue; pid=$(cat \"$f\"); kill \"$pid\" 2>/dev/null || true; done\n"
		if _, err := o.Remote.Run(ctx, s, script); err != nil {
			return err
		}
	}
	emit("info", "lifecycle", "experiment stopped", nil)
	return nil
}

func (o Orchestrator) Attach(ctx context.Context, exp model.Experiment, node model.Node, server model.Server, expression string) (string, error) {
	base := strings.TrimRight(server.WorkDir, "/")
	// Resolve the immutable runtime selected during deployment from its symlink-free artifact path.
	find := "runtime=" + shell(base+"/artifacts/"+exp.ArtifactSHA+"/pfap-runtime") + "\n"
	root := base + "/experiments/" + exp.ID + "/" + server.ID
	script := "set -eu\n" + find + "PFAP_PRFKEY_DIR=\"$runtime/prfKey\" LD_LIBRARY_PATH=\"$runtime/lib\" \"$runtime/bin/geth\" attach " + shell(root+"/node"+strconv.Itoa(node.LocalIndex)+"/geth.ipc") + " --exec " + shell(expression) + "\n"
	return o.Remote.Run(ctx, server, script)
}

func (o Orchestrator) TransactionTimings(ctx context.Context, exp model.Experiment, node model.Node, server model.Server, hash string) (proofUs, verifyUs int64, err error) {
	base := strings.TrimRight(server.WorkDir, "/")
	logPath := base + "/experiments/" + exp.ID + "/" + server.ID + "/node" + strconv.Itoa(node.LocalIndex) + "/geth.log"
	out, err := o.Remote.Run(ctx, server, "tail -n 2000 "+shell(logPath)+"\n")
	if err != nil {
		return 0, 0, err
	}
	proofUs, verifyUs, ok := ExtractTransactionTimings(out, hash)
	if !ok {
		return 0, 0, errors.New("transaction timing markers not found")
	}
	return proofUs, verifyUs, nil
}

func (o Orchestrator) RecentProofTimings(ctx context.Context, exp model.Experiment, node model.Node, server model.Server) (proofUs, verifyUs int64, err error) {
	base := strings.TrimRight(server.WorkDir, "/")
	logPath := base + "/experiments/" + exp.ID + "/" + server.ID + "/node" + strconv.Itoa(node.LocalIndex) + "/geth.log"
	out, err := o.Remote.Run(ctx, server, "tail -n 200 "+shell(logPath)+"\n")
	if err != nil {
		return 0, 0, err
	}
	proofUs, verifyUs, ok := ExtractRecentProofTimings(out)
	if !ok {
		return 0, 0, errors.New("recent proof timing markers not found")
	}
	return proofUs, verifyUs, nil
}

func ExtractRecentProofTimings(logText string) (proofUs, verifyUs int64, ok bool) {
	lines := strings.Split(logText, "\n")
	start := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "gen ") && strings.Contains(lines[i], " proof Use Time:") {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, 0, false
	}
	end := start + 8
	if end > len(lines) {
		end = len(lines)
	}
	proofUs, verifyUs = ParseProofTimesMicros(strings.Join(lines[start:end], "\n"))
	return proofUs, verifyUs, proofUs > 0
}

func ExtractTransactionTimings(logText, hash string) (proofUs, verifyUs int64, ok bool) {
	lines := strings.Split(logText, "\n")
	end := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "Submitted transaction") && strings.Contains(strings.ToLower(lines[i]), strings.ToLower(hash)) {
			end = i
			break
		}
	}
	if end < 0 {
		return 0, 0, false
	}
	start := 0
	for i := end - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "gen ") && strings.Contains(lines[i], " proof Use Time:") {
			start = i
			break
		}
	}
	if start == 0 {
		for i := end - 1; i >= 0; i-- {
			if strings.Contains(lines[i], "Submitted transaction") {
				start = i + 1
				break
			}
		}
	}
	proofUs, verifyUs = ParseProofTimesMicros(strings.Join(lines[start:end+1], "\n"))
	return proofUs, verifyUs, proofUs > 0 || verifyUs > 0
}

func ParseHash(out string) string {
	for _, f := range strings.Fields(strings.ReplaceAll(out, "\"", "")) {
		if strings.HasPrefix(f, "0x") && len(f) == 66 {
			return strings.TrimSpace(f)
		}
	}
	return ""
}

var proofTimeRE = regexp.MustCompile(`(?m)(gen|verify)\s+\w+\s+proof\s+Use Time:\s*([0-9.]+)s`)
var txCreateTimeRE = regexp.MustCompile(`Create .+ transaction Cost Time \(ms\):\s*([0-9]+)`)
var txVerifyTimeRE = regexp.MustCompile(`Verify .+ transaction Cost Time \(ms\):\s*([0-9]+)`)

func (o Orchestrator) TransactionPhaseTimings(ctx context.Context, exp model.Experiment, node model.Node, server model.Server, hash string) (generationUs, verificationUs int64, err error) {
	base := strings.TrimRight(server.WorkDir, "/")
	logPath := base + "/experiments/" + exp.ID + "/" + server.ID + "/node" + strconv.Itoa(node.LocalIndex) + "/geth.log"
	out, err := o.Remote.Run(ctx, server, "tail -n 4000 "+shell(logPath)+"\n")
	if err != nil {
		return 0, 0, err
	}
	generationUs, verificationUs, ok := ExtractTransactionPhaseTimings(out, hash)
	if !ok {
		return 0, 0, errors.New("transaction phase timing markers not found")
	}
	return generationUs, verificationUs, nil
}

func ExtractTransactionPhaseTimings(logText, hash string) (generationUs, verificationUs int64, ok bool) {
	lines := strings.Split(logText, "\n")
	for i, line := range lines {
		if strings.Contains(line, "Submitted transaction") && strings.Contains(strings.ToLower(line), strings.ToLower(hash)) {
			// The API prints the create marker immediately after the submission
			// log. Prefer that small forward window so an earlier transaction's
			// marker cannot be associated with this hash.
			end := i + 10
			if end >= len(lines) {
				end = len(lines) - 1
			}
			if match := txCreateTimeRE.FindStringSubmatch(strings.Join(lines[i:end+1], "\n")); len(match) == 2 {
				ms, _ := strconv.ParseInt(match[1], 10, 64)
				generationUs = ms * 1000
			}
		}
		if strings.Contains(strings.ToLower(line), strings.ToLower(hash)) {
			if match := txVerifyTimeRE.FindStringSubmatch(line); len(match) == 2 {
				ms, _ := strconv.ParseInt(match[1], 10, 64)
				verificationUs = ms * 1000
			}
		}
	}
	return generationUs, verificationUs, generationUs > 0 || verificationUs > 0
}

func ParseProofTimes(out string) (proofMs, verifyMs int64) {
	proofUs, verifyUs := ParseProofTimesMicros(out)
	return proofUs / 1000, verifyUs / 1000
}

func ParseProofTimesMicros(out string) (proofUs, verifyUs int64) {
	for _, m := range proofTimeRE.FindAllStringSubmatch(out, -1) {
		seconds, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		us := int64(seconds*1_000_000 + 0.5)
		if m[1] == "gen" {
			proofUs += us
		} else {
			verifyUs += us
		}
	}
	return
}

func ExtractJSONString(out string) (string, error) {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if value, err := strconv.Unquote(line); err == nil && (strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[")) {
			return value, nil
		}
		if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
			return line, nil
		}
	}
	return "", errors.New("JSON result not found")
}

func Deadline(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
