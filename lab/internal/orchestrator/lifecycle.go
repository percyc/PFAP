package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pfap/lab/internal/model"
)

// These ownership checks consume NUL-separated argv entries. Matching a PID
// file or a substring of a process's command line is never sufficient.
const processOwnershipScript = `
process_uses_datadir() {
    local pid="$1" previous='' arg
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 1
    [ -r "/proc/$pid/cmdline" ] || return 1
    while IFS= read -r -d '' arg; do
        if { [ "$previous" = --datadir ] && [ "$arg" = "$dir" ]; } || [ "$arg" = "--datadir=$dir" ]; then return 0; fi
        previous="$arg"
    done <"/proc/$pid/cmdline"
    return 1
}
process_uses_runtime() {
    local pid="$1" arg first='' header interpreter remainder index=0 candidate_sha
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 1
    [ -r "/proc/$pid/cmdline" ] || return 1
    while IFS= read -r -d '' arg; do
        candidate_sha=''
        if [ "$arg" = "$geth" ]; then candidate_sha="$target_sha"; fi
        if [ "$arg" = "$current_geth" ]; then candidate_sha="$current_sha"; fi
        if [ -n "$candidate_sha" ]; then
            if [ "$index" -eq 0 ]; then
                [ "$arg" -ef "/proc/$pid/exe" ] || return 1
            else
                # Interpreted test or wrapper runtimes must name the actual
                # interpreter in argv[0] and in their existing shebang.
                [ "$first" -ef "/proc/$pid/exe" ] || return 1
                [ -r "$arg" ] && IFS= read -r header <"$arg" || return 1
                [[ "$header" == '#!'* ]] || return 1
                read -r interpreter remainder <<<"${header#\#!}"
                [ "$interpreter" -ef "/proc/$pid/exe" ] || return 1
            fi
            matched_sha="$candidate_sha"
            return 0
        fi
        first="$arg"
        index=$((index + 1))
        [ "$index" -lt 2 ] || break
    done <"/proc/$pid/cmdline"
    return 1
}
`

type StopResult struct {
	NodeID string `json:"nodeId"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// lifecycleNodes reconstructs only the deterministic deployment manifest for
// legacy experiments. It never creates a directory, account, or chain.
func lifecycleNodes(exp model.Experiment) ([]model.Node, error) {
	if len(exp.Nodes) > 0 {
		return append([]model.Node(nil), exp.Nodes...), nil
	}
	var nodes []model.Node
	index := 1
	for _, placement := range exp.Placements {
		if placement.Count < 1 || placement.Count > 100 || len(nodes)+placement.Count > 100 {
			return nil, fmt.Errorf("server %s node count is invalid or total exceeds 100", placement.ServerID)
		}
		for local := 1; local <= placement.Count; local++ {
			nodes = append(nodes, model.Node{
				ID: fmt.Sprintf("%s-n%d", exp.ID, index), Name: fmt.Sprintf("node-%d", index),
				ServerID: placement.ServerID, Index: index, LocalIndex: local,
				P2PPort: exp.P2PPortBase + index - 1, RPCPort: exp.RPCPortBase + index - 1,
				RuntimeSHA: exp.ArtifactSHA,
			})
			index++
		}
	}
	if len(nodes) == 0 {
		return nil, errors.New("experiment has no nodes or placements to inspect")
	}
	return nodes, nil
}

// StopNodes attempts every node, retaining uncertainty on any failure. Only a
// verified process belonging to this datadir and immutable runtime gets TERM.
func (o Orchestrator) StopNodes(ctx context.Context, exp model.Experiment, servers map[string]model.Server, emit EmitFunc) ([]StopResult, error) {
	if emit == nil {
		emit = func(string, string, string, map[string]any) {}
	}
	nodes, err := lifecycleNodes(exp)
	if err != nil {
		return nil, err
	}
	results := make([]StopResult, 0, len(nodes))
	var failures []error
	for _, node := range nodes {
		result := StopResult{NodeID: node.ID, Status: "unknown"}
		server, ok := servers[node.ServerID]
		var stopErr error
		if !ok {
			stopErr = fmt.Errorf("server %s not found", node.ServerID)
		} else {
			var script string
			script, stopErr = stopNodeScript(exp, node, server)
			if stopErr == nil {
				stopCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
				out, runErr := o.Remote.Run(stopCtx, server, script)
				cancel()
				if runErr != nil {
					stopErr = fmt.Errorf("%w (%s)", runErr, strings.TrimSpace(out))
				} else if !strings.Contains("\n"+strings.TrimSpace(out)+"\n", "\nstop=stopped\n") {
					stopErr = errors.New("remote stop did not confirm process exit")
				}
			}
		}
		if stopErr != nil {
			failure := fmt.Errorf("stop %s: %w", node.Name, stopErr)
			result.Error = failure.Error()
			failures = append(failures, failure)
			emit("error", "node-stop", result.Error, map[string]any{"nodeId": node.ID, "status": result.Status})
		} else {
			result.Status = "stopped"
			emit("info", "node-stop", "node exit confirmed", map[string]any{"nodeId": node.ID, "status": result.Status})
		}
		results = append(results, result)
	}
	return results, errors.Join(failures...)
}

func stopNodeScript(exp model.Experiment, node model.Node, server model.Server) (string, error) {
	if !recoveryID.MatchString(exp.ID) || !recoveryID.MatchString(server.ID) || node.ServerID != server.ID || node.LocalIndex < 1 || node.Index < 1 {
		return "", errors.New("invalid experiment or node identity for stop")
	}
	if !filepath.IsAbs(server.WorkDir) || filepath.Clean(server.WorkDir) == "/" {
		return "", errors.New("invalid server directory for stop")
	}
	currentSHA, targetSHA := nodeRuntimeSHA(exp, node), nodeRecoverySHA(exp, node)
	if !recoverySHA.MatchString(exp.ArtifactSHA) || !recoverySHA.MatchString(currentSHA) || !recoverySHA.MatchString(targetSHA) {
		return "", errors.New("cannot verify process ownership without valid immutable runtime SHAs")
	}
	base := strings.TrimRight(server.WorkDir, "/")
	dir := base + "/experiments/" + exp.ID + "/" + server.ID + "/node" + strconv.Itoa(node.LocalIndex)
	return "set -euo pipefail\n" +
		"workdir=" + shell(base) + "\n" +
		"experiment_id=" + shell(exp.ID) + "\n" +
		"server_id=" + shell(server.ID) + "\n" +
		"node_dir=" + shell("node"+strconv.Itoa(node.LocalIndex)) + "\n" +
		"dir=" + shell(dir) + "\n" +
		"geth=" + shell(base+"/artifacts/"+targetSHA+"/pfap-runtime/bin/geth") + "\n" +
		"current_geth=" + shell(base+"/artifacts/"+currentSHA+"/pfap-runtime/bin/geth") + "\n" +
		"current_sha=" + shell(currentSHA) + "\n" +
		"target_sha=" + shell(targetSHA) + "\n" + processOwnershipScript + `
fail() { printf '[ERROR] %s\n' "$*" >&2; exit 1; }
[ -r /proc/self/stat ] || fail "Process ownership cannot be checked on this server"
process_identity() {
    local stat suffix
    [ -r "/proc/$1/stat" ] || return 1
    stat=$(cat "/proc/$1/stat" 2>/dev/null) || return 1
    suffix="${stat##*) }"
    read -r -a fields <<<"$suffix"
    [ "${#fields[@]}" -ge 20 ] || return 1
    [ "${fields[0]}" != Z ] && [ "${fields[0]}" != X ] || return 1
    printf '%s\n' "${fields[19]}"
}
process_exited() {
    local stat suffix
    [ ! -e "/proc/$1" ] && return 0
    stat=$(cat "/proc/$1/stat" 2>/dev/null) || { [ ! -e "/proc/$1" ]; return; }
    suffix="${stat##*) }"
    case "$suffix" in Z\ *|X\ *) return 0 ;; esac
    return 1
}
recorded=''
if [ -f "$dir/geth.pid" ]; then recorded=$(cat "$dir/geth.pid") || fail "Cannot read node PID file"; fi
pid=''
for proc in /proc/[0-9]*/cmdline; do
    candidate="${proc#/proc/}"; candidate="${candidate%/cmdline}"
    if process_uses_datadir "$candidate"; then
        process_uses_runtime "$candidate" || fail "Another runtime references this datadir (PID $candidate); no process was stopped"
        [ -z "$pid" ] || fail "Multiple processes reference this datadir; no process was stopped"
        pid="$candidate"
    fi
done
if [ -z "$pid" ]; then
    # An interrupted fresh deployment may never have created this datadir.
    # Walk from the configured work directory with access checks so EACCES
    # and dangling symlinks cannot be mistaken for a missing directory.
    checked="$workdir"
    for part in experiments "$experiment_id" "$server_id" "$node_dir"; do
        [ -d "$checked" ] && [ -r "$checked" ] && [ -x "$checked" ] || fail "Cannot inspect node directory ancestry: $checked"
        next="$checked/$part"
        if [ ! -e "$next" ]; then
            [ ! -L "$next" ] || fail "Node directory ancestry contains a dangling symlink: $next"
            [ -r "$checked" ] && [ -x "$checked" ] && [ ! -e "$dir" ] && [ ! -L "$dir" ] || fail "Node directory changed during inspection"
            printf 'stop=stopped\n'
            exit 0
        fi
        checked="$next"
    done
    [ -d "$checked" ] && [ -r "$checked" ] && [ -x "$checked" ] || fail "Cannot inspect original node directory: $checked"
    [[ "$recorded" =~ ^[1-9][0-9]*$ ]] || fail "No owned process found and no valid recorded PID; stopped state is unknown"
    if [ -e "/proc/$recorded" ]; then
        stat=$(cat "/proc/$recorded/stat" 2>/dev/null) || fail "Recorded PID cannot be inspected"
        suffix="${stat##*) }"
        case "$suffix" in Z\ *|X\ *) ;; *) fail "Recorded PID belongs to an unverified process; it was not signalled" ;; esac
    fi
    printf 'stop=stopped\n'
    exit 0
fi
identity=$(process_identity "$pid") || fail "Owned process changed before its identity could be verified; retry stop"
process_uses_datadir "$pid" && process_uses_runtime "$pid" || fail "Process ownership changed before stop; no signal sent"
[ "$(process_identity "$pid" || true)" = "$identity" ] || fail "PID was reused before stop; no signal sent"
kill -TERM -- "$pid" || fail "TERM could not be delivered to the owned node process"
deadline=$((SECONDS + 20))
while [ "$SECONDS" -lt "$deadline" ]; do
    if [ ! -e "/proc/$pid" ]; then break; fi
    current_identity=$(process_identity "$pid" || true)
    if [ -z "$current_identity" ]; then
        process_exited "$pid" && break
        fail "Process state became unreadable while waiting for exit"
    fi
    [ "$current_identity" = "$identity" ] || break
    if ! process_uses_datadir "$pid" || ! process_uses_runtime "$pid"; then
        # /proc identity, argv and exe are separate reads. TERM can make the
        # original process disappear or become a zombie between those reads.
        process_exited "$pid" && break
        current_identity=$(process_identity "$pid" || true)
        if [ -n "$current_identity" ] && [ "$current_identity" != "$identity" ]; then break; fi
        process_exited "$pid" && break
        fail "Process ownership changed while waiting for exit"
    fi
    sleep 0.1
done
[ "$(process_identity "$pid" || true)" != "$identity" ] || fail "Owned node did not exit within 20 seconds after TERM"
# Check that another process did not acquire the same datadir during shutdown.
for proc in /proc/[0-9]*/cmdline; do
    candidate="${proc#/proc/}"; candidate="${candidate%/cmdline}"
    if process_uses_datadir "$candidate"; then fail "A process still references this datadir after TERM (PID $candidate)"; fi
done
printf 'stop=stopped\n'
`, nil
}

// Resume uses only cached immutable runtimes and original on-disk identity.
// Every process is attempted before peer connections and mining are restored.
func (o Orchestrator) Resume(ctx context.Context, exp *model.Experiment, servers map[string]model.Server, emit EmitFunc) ([]model.Node, error) {
	if emit == nil {
		emit = func(string, string, string, map[string]any) {}
	}
	nodes, err := lifecycleNodes(*exp)
	if err != nil {
		return nil, err
	}
	if len(exp.Nodes) == 0 {
		if err := model.ResolveMiners(nodes, *exp, servers); err != nil {
			return nil, err
		}
		exp.Nodes = append([]model.Node(nil), nodes...)
	}
	var failures []error
	ready := make([]bool, len(nodes))
	for i := range nodes {
		nodes[i].Status, nodes[i].Mining, nodes[i].Peers = "unreachable", nil, 0
		server, ok := servers[nodes[i].ServerID]
		var recoverErr error
		if !ok {
			recoverErr = fmt.Errorf("server %s not found", nodes[i].ServerID)
		} else {
			nodes[i], recoverErr = o.recoverNodeProcess(ctx, *exp, nodes[i], server, emit)
		}
		if recoverErr != nil {
			nodes[i].RecoveryError = recoverErr.Error()
			nodes[i].StateError = recoverErr.Error()
			failures = append(failures, fmt.Errorf("resume %s: %w", nodes[i].Name, recoverErr))
			continue
		}
		ready[i] = true
		nodes[i].Status, nodes[i].RecoveryError, nodes[i].StateError = "running", "", ""
	}
	active := *exp
	active.Nodes = nil
	for i := range nodes {
		if ready[i] {
			active.Nodes = append(active.Nodes, nodes[i])
		}
	}
	for i := range nodes {
		if !ready[i] {
			continue
		}
		node, server := nodes[i], servers[nodes[i].ServerID]
		connectErr := o.reconnectRecoveredNode(ctx, active, node, server, servers, emit)
		mining := model.NodeIsMiner(*exp, node)
		miningErr := o.SetMining(ctx, *exp, node, server, mining)
		if miningErr == nil {
			nodes[i].Mining = &mining
		}
		if restoreErr := errors.Join(connectErr, miningErr); restoreErr != nil {
			nodes[i].Status, nodes[i].RecoveryError, nodes[i].StateError = "unreachable", restoreErr.Error(), restoreErr.Error()
			failures = append(failures, fmt.Errorf("restore %s: %w", node.Name, restoreErr))
		}
	}
	exp.Nodes = nodes
	return nodes, errors.Join(failures...)
}
