package orchestrator

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pfap/lab/internal/model"
)

var recoveryID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
var recoverySHA = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
var recoveryProcessResult = regexp.MustCompile(`(?m)^recovery=(started|existing) pid=[1-9][0-9]* runtimeSha=([a-fA-F0-9]{64})$`)
var recoveryAccountResult = regexp.MustCompile(`(?m)^recovery-account=([a-fA-F0-9]{40})$`)

func nodeRuntimeSHA(exp model.Experiment, node model.Node) string {
	if node.RuntimeSHA != "" {
		return node.RuntimeSHA
	}
	return exp.ArtifactSHA
}

func nodeRecoverySHA(exp model.Experiment, node model.Node) string {
	if exp.RecoveryArtifactSHA != "" {
		return exp.RecoveryArtifactSHA
	}
	return nodeRuntimeSHA(exp, node)
}

// RecoverNode resumes one existing node; it never invokes network.sh start,
// init, account new, or any privacy-state initialization command.
func (o Orchestrator) RecoverNode(ctx context.Context, exp model.Experiment, node model.Node, servers map[string]model.Server, emit EmitFunc) error {
	server, ok := servers[node.ServerID]
	if !ok {
		return fmt.Errorf("server %s not found", node.ServerID)
	}
	if emit == nil {
		emit = func(string, string, string, map[string]any) {}
	}
	node, err := o.recoverNodeProcess(ctx, exp, node, server, emit)
	if err != nil {
		return err
	}
	if err := o.reconnectRecoveredNode(ctx, exp, node, server, servers, emit); err != nil {
		return err
	}
	if model.NodeIsMiner(exp, node) {
		if err := o.SetMining(ctx, exp, node, server, true); err != nil {
			return fmt.Errorf("%s is running but mining could not be restored: %w", node.Name, err)
		}
		emit("info", "recovery", "mining restored", map[string]any{"nodeId": node.ID})
	}
	return nil
}

// recoverNodeProcess returns identity observed from the original datadir and
// the actual process runtime, including when a later readiness check fails.
func (o Orchestrator) recoverNodeProcess(ctx context.Context, exp model.Experiment, node model.Node, server model.Server, emit EmitFunc) (model.Node, error) {
	script, err := recoveryScript(exp, node, server)
	if err != nil {
		return node, err
	}
	startCtx, cancel := context.WithTimeout(ctx, 80*time.Second)
	out, err := o.Remote.Run(startCtx, server, script)
	cancel()
	if account := recoveryAccountResult.FindStringSubmatch(out); len(account) == 2 && node.Account == "" {
		node.Account = "0x" + strings.ToLower(account[1])
	}
	if result := recoveryProcessResult.FindStringSubmatch(out); len(result) == 3 {
		node.RuntimeSHA = result[2]
		if result[1] == "started" {
			emit("info", "node-recovery-started", "existing node process restarted", map[string]any{"nodeId": node.ID, "restarted": true, "runtimeSha": node.RuntimeSHA})
		} else {
			emit("info", "node-recovery-existing", "existing node process adopted", map[string]any{"nodeId": node.ID, "restarted": false, "runtimeSha": node.RuntimeSHA})
		}
	}
	if err != nil {
		return node, fmt.Errorf("recover %s: %w (%s)", node.Name, err, strings.TrimSpace(out))
	}
	emit("info", "recovery", "node process ready", map[string]any{"nodeId": node.ID, "detail": strings.TrimSpace(out)})
	return node, nil
}

func recoveryScript(exp model.Experiment, node model.Node, server model.Server) (string, error) {
	if !recoveryID.MatchString(exp.ID) || !recoveryID.MatchString(server.ID) || node.ServerID != server.ID || node.LocalIndex < 1 || node.Index < 1 {
		return "", fmt.Errorf("invalid experiment or node identity for recovery")
	}
	if !recoverySHA.MatchString(exp.ArtifactSHA) {
		return "", fmt.Errorf("experiment has no valid immutable runtime SHA; redeployment is not a recovery operation")
	}
	currentSHA, targetSHA := nodeRuntimeSHA(exp, node), nodeRecoverySHA(exp, node)
	if !recoverySHA.MatchString(currentSHA) || !recoverySHA.MatchString(targetSHA) {
		return "", fmt.Errorf("invalid current or recovery runtime SHA")
	}
	if !filepath.IsAbs(server.WorkDir) || filepath.Clean(server.WorkDir) == "/" || node.P2PPort < 1 || node.P2PPort > 65535 || exp.NetworkID < 1 {
		return "", fmt.Errorf("invalid server directory or network configuration for recovery")
	}
	base := strings.TrimRight(server.WorkDir, "/")
	root := base + "/experiments/" + exp.ID + "/" + server.ID
	requiredBytes := MinimumDiskFreeBytes
	if model.NodeIsMiner(exp, node) {
		requiredBytes = minerServerDiskRequiredBytes(exp, node, server)
	}
	diskCheck, err := diskPreflightScript(server, requiredBytes)
	if err != nil {
		return "", err
	}
	variables := "runtime=" + shell(base+"/artifacts/"+targetSHA+"/pfap-runtime") + "\n" +
		"current_runtime=" + shell(base+"/artifacts/"+currentSHA+"/pfap-runtime") + "\n" +
		"current_sha=" + shell(currentSHA) + "\n" +
		"target_sha=" + shell(targetSHA) + "\n" +
		"root=" + shell(root) + "\n" +
		"dir=" + shell(root+"/node"+strconv.Itoa(node.LocalIndex)) + "\n" +
		"network_id=" + strconv.Itoa(exp.NetworkID) + "\n" +
		"p2p_port=" + strconv.Itoa(node.P2PPort) + "\n" +
		"max_peers=" + strconv.Itoa(experimentMaxPeers(exp)) + "\n" +
		"expected_account=" + shell(strings.TrimPrefix(strings.ToLower(node.Account), "0x")) + "\n"
	return "set -euo pipefail\n" + variables + `
geth="$runtime/bin/geth"
current_geth="$current_runtime/bin/geth"
ipc_geth="$geth"
pidfile="$dir/geth.pid"
export PFAP_PRFKEY_DIR="$runtime/prfKey" LD_LIBRARY_PATH="$runtime/lib"
fail() {
    printf '[ERROR] %s\n' "$*" >&2
    if [ -f "$dir/geth.log" ]; then tail -n 60 "$dir/geth.log" | tail -c 12000 >&2; fi
    exit 1
}
[ -x "$geth" ] || fail "The node's selected recovery runtime is missing: $geth"
[ -d "$dir/geth/chaindata" ] || fail "Existing chain data is missing; refusing to initialize a new chain"
[ -f "$root/password.txt" ] || fail "Existing account password file is missing"
[ -f "$dir/address" ] || fail "Existing account address file is missing"
address=$(tr -d '\r\n' <"$dir/address")
[[ "$address" =~ ^[0-9a-fA-F]{40}$ ]] || fail "Invalid existing account address"
[ -z "$expected_account" ] || [ "${address,,}" = "$expected_account" ] || fail "Stored account does not match the experiment account"
key=$(find "$dir/keystore" -maxdepth 1 -type f -iname "*--$address" -print -quit 2>/dev/null) || fail "Existing keystore is missing"
[ -n "$key" ] || fail "Keystore for the existing account is missing"
printf 'recovery-account=%s\n' "$address"
for command in setsid timeout flock; do command -v "$command" >/dev/null || fail "Required recovery command is missing: $command"; done
# This lock only serializes Lab recovery. It does not replace geth's own
# fcntl/LevelDB locks, which flock cannot test.
exec 9>"$dir/.lab-recovery.lock" || fail "Cannot open recovery lock; check disk capacity and permissions"
flock -n 9 || fail "Another recovery operation is already running for this node"
` + processOwnershipScript + `
ipc_ready() {
    timeout 4 "$ipc_geth" attach "$dir/geth.ipc" --exec 'typeof eth.blockNumber === "number"' 2>/dev/null | grep -qx true
}
pid=''
for proc in /proc/[0-9]*/cmdline; do
    candidate="${proc#/proc/}"; candidate="${candidate%/cmdline}"
    if process_uses_datadir "$candidate"; then
        process_uses_runtime "$candidate" || fail "Another runtime is already using this datadir (PID $candidate); no process was stopped"
        [ -z "$pid" ] || fail "Multiple processes reference this datadir; manual inspection is required"
        pid="$candidate"
    fi
done
if [ -n "$pid" ]; then
    if [ "$matched_sha" = "$current_sha" ]; then
        ipc_geth="$current_geth"
        export PFAP_PRFKEY_DIR="$current_runtime/prfKey" LD_LIBRARY_PATH="$current_runtime/lib"
    fi
    ipc_ready || fail "Existing node process (PID $pid) is alive but IPC is unavailable; it was left running for inspection"
    printf '%s\n' "$pid" >"$pidfile" || fail "Cannot persist existing node PID; check disk capacity and permissions"
    printf 'recovery=existing pid=%s runtimeSha=%s\n' "$pid" "$matched_sha"
    exit 0
fi
# A runtime hotfix may change executable code, but cannot rotate proving
# parameters inside an existing experiment. Compare names and contents from
# relative paths so directory names do not affect the manifest comparison.
if [ "$target_sha" != "$current_sha" ]; then
    command -v sha256sum >/dev/null || fail "sha256sum is required to verify recovery proving keys"
    key_manifest() (
        cd "$1/prfKey" || exit 1
        export LC_ALL=C
        shopt -s nullglob
        keys=(*.txt)
        [ "${#keys[@]}" -gt 0 ] || exit 1
        sha256sum -- "${keys[@]}"
    )
    current_keys=$(key_manifest "$current_runtime") || fail "Cannot read current runtime proving-key manifest"
    target_keys=$(key_manifest "$runtime") || fail "Cannot read recovery runtime proving-key manifest"
    [ "$current_keys" = "$target_keys" ] || fail "Recovery runtime proving keys do not match the current runtime; refusing to start with different experiment keys"
fi
# Never signal a PID merely because it is recorded in a stale PID file.
# Detect a datadir opened using a different spelling/path before launching.
for lock in "$dir/geth/LOCK" "$dir/geth/chaindata/LOCK" "$dir/geth/lightchaindata/LOCK"; do
    [ -e "$lock" ] || continue
    if command -v fuser >/dev/null; then
        if fuser "$lock" >/dev/null 2>&1; then fail "Datadir lock has a live owner: $lock"; fi
    else
        # Compare file identities through /proc when psmisc is unavailable.
        # geth still acquires its own native locks to close any launch race.
        for fd in /proc/[0-9]*/fd/*; do
            if [ "$lock" -ef "$fd" ]; then fail "Datadir lock has a live owner: $lock ($fd)"; fi
        done
    fi
done
if command -v ss >/dev/null && ss -H -ltn "sport = :$p2p_port" | grep -q .; then fail "P2P port $p2p_port is already in use"; fi
` + diskCheck + `
mkdir -p "$root/ethash" || fail "Cannot create DAG directory; check disk capacity and permissions"
log_offset=0
if [ -f "$dir/geth.log" ]; then log_offset=$(wc -c <"$dir/geth.log"); fi
startup_state_failed() {
    tail -c "+$((log_offset + 1))" "$dir/geth.log" | grep -E 'Decode SNSbytes error|Decode string[[:space:]]+error|Restore private account state|decode SN (state|hex):' >/dev/null
}
startup_pidfile=$(mktemp "$dir/.lab-recovery-pid.XXXXXX") || fail "Cannot create PID handoff file; check disk capacity and permissions"
trap 'rm -f -- "$startup_pidfile"' EXIT
# The new session's child writes its own PID immediately before exec. $!
# can refer to the short-lived setsid parent and must not be persisted.
nohup setsid --wait bash -ec 'printf "%s\n" "$BASHPID" >"$1"; shift; exec "$@"' lab-recovery "$startup_pidfile" \
    "$geth" --datadir "$dir" --networkid "$network_id" --port "$p2p_port" --maxpeers "$max_peers" \
    --ipcpath "$dir/geth.ipc" --unlock "$address" --password "$root/password.txt" \
    --ethash.dagdir "$root/ethash" --nodiscover --nousb \
    9>&- </dev/null >>"$dir/geth.log" 2>&1 &
startup_launcher=$!
deadline=$((SECONDS + 60))
while [ "$SECONDS" -lt "$deadline" ]; do
    if ! kill -0 "$startup_launcher" 2>/dev/null; then
        wait "$startup_launcher" || fail "Node launcher failed; check disk capacity, permissions and startup log"
    fi
    candidate=$(head -c 64 "$startup_pidfile" 2>/dev/null || true)
    if process_uses_datadir "$candidate" && process_uses_runtime "$candidate"; then
        if [ -z "$pid" ]; then printf 'recovery=started pid=%s runtimeSha=%s\n' "$candidate" "$target_sha"; fi
        pid="$candidate"
        printf '%s\n' "$pid" >"$pidfile" || fail "Cannot persist restarted node PID; check disk capacity and permissions (process left running)"
        if ipc_ready; then
            if startup_state_failed; then fail "Private account SN restore failed during this startup; process left running for inspection, recovery is incomplete"; fi
            printf 'recovery=ready pid=%s runtimeSha=%s\n' "$pid" "$target_sha"
            exit 0
        fi
    elif [ -n "$pid" ]; then
        fail "Node exited while waiting for IPC"
    elif [[ "$candidate" =~ ^[1-9][0-9]*$ ]] && { ! kill -0 "$candidate" 2>/dev/null || [ "$(awk '{print $3}' "/proc/$candidate/stat" 2>/dev/null || true)" = Z ]; }; then
        fail "Node exited before IPC became available"
    fi
    sleep 0.25
done
fail "Timed out waiting for node IPC (60 seconds); inspect the process and log before retrying"
`, nil
}

func (o Orchestrator) recoveryAttach(ctx context.Context, exp model.Experiment, node model.Node, server model.Server, expression string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return o.Attach(ctx, exp, node, server, expression)
}

func consoleTrue(out string) bool {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "true"
}

// recoveryEnode picks an address from the destination's point of view. A
// controller server named "local" is not a routable peer address.
func recoveryEnode(raw string, node model.Node, owner, destination model.Server) (string, error) {
	raw = strings.Trim(strings.TrimSpace(raw), "\"")
	at := strings.LastIndex(raw, "@")
	if !strings.HasPrefix(raw, "enode://") || at < 0 {
		return "", fmt.Errorf("invalid enode returned by %s", node.Name)
	}
	host := owner.P2PHost
	if owner.ID == destination.ID {
		host = "127.0.0.1"
	} else if host == "" && owner.Host != "local" && owner.Host != "localhost-local" {
		host = owner.Host
	}
	if host == "" {
		endpoint := strings.SplitN(raw[at+1:], "?", 2)[0]
		var err error
		host, _, err = net.SplitHostPort(endpoint)
		if err != nil {
			return "", fmt.Errorf("configure a reachable P2P host for %s", owner.Name)
		}
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if host == "" || host == "local" || host == "localhost-local" || (owner.ID != destination.ID && (host == "localhost" || (ip != nil && (ip.IsUnspecified() || ip.IsLoopback())))) {
		return "", fmt.Errorf("configure a reachable P2P host for %s", owner.Name)
	}
	return raw[:at+1] + net.JoinHostPort(host, strconv.Itoa(node.P2PPort)), nil
}

func (o Orchestrator) reconnectRecoveredNode(ctx context.Context, exp model.Experiment, node model.Node, server model.Server, servers map[string]model.Server, emit EmitFunc) error {
	if len(exp.Nodes) < 2 {
		return nil
	}
	raw, err := o.recoveryAttach(ctx, exp, node, server, "admin.nodeInfo.enode")
	if err != nil {
		return fmt.Errorf("read recovered node enode: %w", err)
	}
	connected := 0
	for _, peer := range exp.Nodes {
		if peer.ID == node.ID || (exp.Topology != "full-mesh" && peer.ServerID != node.ServerID) {
			continue
		}
		peerServer, ok := servers[peer.ServerID]
		if !ok {
			continue
		}
		peerRaw, err := o.recoveryAttach(ctx, exp, peer, peerServer, "admin.nodeInfo.enode")
		if err != nil {
			emit("warn", "recovery", "peer unavailable; skipped during reconnect", map[string]any{"nodeId": node.ID, "peerId": peer.ID, "error": err.Error()})
			continue
		}
		peerEnode, err := recoveryEnode(peerRaw, peer, peerServer, server)
		if err != nil {
			return err
		}
		selfEnode, err := recoveryEnode(raw, node, server, peerServer)
		if err != nil {
			return err
		}
		out, err := o.recoveryAttach(ctx, exp, node, server, "admin.addPeer("+strconv.Quote(peerEnode)+")")
		if err != nil || !consoleTrue(out) {
			return fmt.Errorf("reconnect %s to %s failed: %s (%v)", node.Name, peer.Name, strings.TrimSpace(out), err)
		}
		out, err = o.recoveryAttach(ctx, exp, peer, peerServer, "admin.addPeer("+strconv.Quote(selfEnode)+")")
		if err != nil || !consoleTrue(out) {
			return fmt.Errorf("reconnect %s to %s failed: %s (%v)", peer.Name, node.Name, strings.TrimSpace(out), err)
		}
		connected++
	}
	emit("info", "recovery", "peer connections restored", map[string]any{"nodeId": node.ID, "connectedPeers": connected})
	return nil
}
