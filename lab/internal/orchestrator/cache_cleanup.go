package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pfap/lab/internal/model"
)

// CacheTarget identifies a complete, unchanged DAG directory. ServerID is the
// original deployment placement, which may predate worker re-registration.
// Bytes counts allocated file blocks, so it is an estimate of reclaimable space.
type CacheTarget struct {
	ExperimentID string `json:"experimentId"`
	ServerID     string `json:"serverId"`
	Path         string `json:"path"`
	Bytes        int64  `json:"bytes"`
	Files        int    `json:"files"`
	Fingerprint  string `json:"fingerprint"`
}

type CleanupResult struct {
	Target       CacheTarget `json:"target"`
	FreedBytes   int64       `json:"freedBytes"`
	RemovedFiles int         `json:"removedFiles"`
	Status       string      `json:"status"`
	Error        string      `json:"error,omitempty"`
}

func cacheWorkDir(server model.Server) (string, error) {
	base := strings.TrimRight(server.WorkDir, "/")
	if !filepath.IsAbs(base) || filepath.Clean(base) != base || base == "/" || strings.ContainsAny(base, "\x00\r\n\t") {
		return "", errors.New("DAG cleanup requires a clean absolute worker directory other than root")
	}
	return base, nil
}

// ScanDAGCaches only inspects placement paths explicitly recorded in positively
// stopped experiments. Unknown directories, uncertain node states, and unsafe
// caches are omitted. The caller supplies the registered worker whose WorkDir
// is being inspected; historical placement IDs are preserved in each target.
func (o Orchestrator) ScanDAGCaches(ctx context.Context, server model.Server, experiments []model.Experiment) ([]CacheTarget, error) {
	base, err := cacheWorkDir(server)
	if err != nil {
		return nil, err
	}
	var script strings.Builder
	script.WriteString("set -euo pipefail\nexport LC_ALL=C\nworkdir=" + shell(base) + "\n" + dagCacheScript)
	seen := make(map[string]bool)
	for _, exp := range experiments {
		if !recoveryID.MatchString(exp.ID) || (exp.Status != "stopped" && exp.Status != "failed") || len(exp.Nodes) == 0 {
			continue
		}
		stopped := true
		placements := make(map[string]bool)
		for _, node := range exp.Nodes {
			if node.Status != "stopped" {
				stopped = false
			}
			placements[node.ServerID] = true
		}
		if !stopped {
			continue
		}
		for _, placement := range exp.Placements {
			key := exp.ID + "/" + placement.ServerID
			if placement.Count < 1 || !placements[placement.ServerID] || !recoveryID.MatchString(placement.ServerID) || seen[key] {
				continue
			}
			seen[key] = true
			script.WriteString("dag_candidate scan " + shell(exp.ID) + " " + shell(placement.ServerID) + " ''\n")
		}
	}
	targets := make([]CacheTarget, 0)
	if len(seen) == 0 {
		return targets, nil
	}
	out, runErr := o.Remote.Run(ctx, server, script.String())
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 6 || parts[0] != "dag-cache" {
			continue
		}
		bytes, bytesErr := strconv.ParseInt(parts[3], 10, 64)
		files, filesErr := strconv.Atoi(parts[4])
		key := parts[1] + "/" + parts[2]
		if !seen[key] || bytesErr != nil || filesErr != nil || bytes < 0 || files < 1 || !recoverySHA.MatchString(parts[5]) {
			return targets, errors.New("invalid DAG cache inspection response")
		}
		targets = append(targets, CacheTarget{
			ExperimentID: parts[1], ServerID: parts[2],
			Path: base + "/experiments/" + key + "/ethash", Bytes: bytes, Files: files, Fingerprint: parts[5],
		})
	}
	if runErr != nil {
		return targets, fmt.Errorf("inspect DAG caches: %w (%s)", runErr, strings.TrimSpace(out))
	}
	return targets, nil
}

// CleanupDAGCache unlinks only the exact ordinary DAG files in the reviewed
// fingerprint. The caller must revalidate the experiment manifest, scan expiry,
// and worker configuration, and exclude concurrent deployment/resume operations.
// FreedBytes reports only successful unlinks, including on a partial failure.
func (o Orchestrator) CleanupDAGCache(ctx context.Context, server model.Server, target CacheTarget) (CleanupResult, error) {
	result := CleanupResult{Target: target, Status: "failed"}
	base, err := cacheWorkDir(server)
	if err == nil && (!recoveryID.MatchString(target.ExperimentID) || !recoveryID.MatchString(target.ServerID) || !recoverySHA.MatchString(target.Fingerprint)) {
		err = errors.New("invalid DAG cache target identity or fingerprint")
	}
	if err == nil && target.Path != base+"/experiments/"+target.ExperimentID+"/"+target.ServerID+"/ethash" {
		err = errors.New("DAG cache path does not match its recorded experiment placement")
	}
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	script := "set -euo pipefail\nexport LC_ALL=C\nworkdir=" + shell(base) + "\n" + dagCacheScript +
		"dag_candidate cleanup " + shell(target.ExperimentID) + " " + shell(target.ServerID) + " " + shell(target.Fingerprint) + "\n"
	out, runErr := o.Remote.Run(ctx, server, script)
	completed := false
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 || (parts[0] != "dag-progress" && parts[0] != "dag-done") {
			continue
		}
		bytes, bytesErr := strconv.ParseInt(parts[1], 10, 64)
		files, filesErr := strconv.Atoi(parts[2])
		if bytesErr != nil || filesErr != nil || bytes < result.FreedBytes || files < result.RemovedFiles {
			runErr = errors.New("invalid DAG cleanup progress response")
			continue
		}
		result.FreedBytes, result.RemovedFiles = bytes, files
		completed = parts[0] == "dag-done"
	}
	if runErr == nil && !completed {
		runErr = errors.New("worker did not confirm DAG cleanup completion")
	}
	if runErr != nil {
		if result.RemovedFiles > 0 {
			result.Status = "partial"
		}
		err = fmt.Errorf("clean DAG cache: %w (%s)", runErr, strings.TrimSpace(out))
		result.Error = err.Error()
		return result, err
	}
	result.Status = "cleaned"
	return result, nil
}

// Workers already require Linux /proc and Bash. Process checks cover all
// readable processes and require complete inspection of the deployment UID.
// Other users' inaccessible descriptors/maps cannot be inspected: deployments
// must use dedicated worker storage, not DAGs shared with other Unix users.
// Directory pinning and exact unlink avoid recursive traversal, even if an
// ancestor is changed concurrently by an administrator outside Lab's lock.
const dagCacheScript = `
dag_fail() {
    if [ "$mode" = scan ]; then
        printf 'dag-skip\t%s\t%s\n' "$experiment_id" "$*"
        exit 0
    fi
    printf '[ERROR] %s\n' "$*" >&2
    exit 1
}
dag_process_fail() {
    printf '[ERROR] Cannot safely inspect worker processes (PID %s): %s; use a worker account with access to its process descriptors and mappings\n' "${pid:-self}" "$*" >&2
    exit 1
}
dag_ancestry() {
    local part checked='' actual
    local -a components
    IFS=/ read -r -a components <<<"$cache"
    ancestry=()
    for part in "${components[@]}"; do
        [ -n "$part" ] || continue
        checked="$checked/$part"
        [ ! -L "$checked" ] && [ -d "$checked" ] && [ -r "$checked" ] && [ -x "$checked" ] || dag_fail "DAG ancestry is missing, unreadable, or symbolic"
        actual=$(stat -c '%d:%i:%f' -- "$checked") || dag_fail "Cannot inspect DAG ancestry"
        ancestry+=("$checked:$actual")
    done
    actual=$(realpath -e -- "$cache") || dag_fail "Cannot resolve DAG directory"
    [ "$actual" = "$cache" ] || dag_fail "DAG path is not canonical"
    if [ -n "${pinned_identity:-}" ]; then
        actual=$(stat -c '%d:%i' -- "$cache") || dag_fail "DAG directory disappeared"
        [ "$actual" = "$pinned_identity" ] && [ "$(stat -c '%d:%i' .)" = "$pinned_identity" ] || dag_fail "DAG directory changed after inspection"
    fi
}
dag_snapshot() {
    local file meta dev inode size blocks block_size mtime ctime links type
    dag_ancestry
    names=(); metadata=(); allocations=(); total_bytes=0
    for file in *; do
        [[ "$file" =~ ^full-R[0-9]+-[a-fA-F0-9]{16}(\.be)?(\.[0-9]+)?$ ]] || dag_fail "DAG directory contains an unrecognized entry"
        [ ! -L "$file" ] && [ -f "$file" ] || dag_fail "DAG entry is not an ordinary file"
        meta=$(stat -c '%d|%i|%s|%b|%B|%y|%z|%h|%F' -- "$file") || dag_fail "Cannot inspect DAG file"
        IFS='|' read -r dev inode size blocks block_size mtime ctime links type <<<"$meta"
        [ "$links" = 1 ] && { [ "$type" = 'regular file' ] || [ "$type" = 'regular empty file' ]; } || dag_fail "DAG file is hardlinked or changed type"
        names+=("$file"); metadata+=("$file:$meta"); allocations+=("$((blocks * block_size))")
        total_bytes=$((total_bytes + blocks * block_size))
        [ "${#names[@]}" -le 10000 ] && [ "$total_bytes" -ge 0 ] || dag_fail "DAG manifest exceeds inspection limits"
    done
    [ "${#names[@]}" -gt 0 ] || dag_fail "DAG directory is empty"
    fingerprint=$(printf '%s\0' "$cache" "${ancestry[@]}" "${metadata[@]}" | sha256sum) || dag_fail "Cannot fingerprint DAG files"
    fingerprint="${fingerprint%% *}"
}
dag_proc_identity() {
    local text suffix
    local -a fields
    text=$(cat "/proc/$1/stat" 2>/dev/null) || return 1
    suffix="${text##*) }"
    read -r -a fields <<<"$suffix"
    [ "${#fields[@]}" -ge 20 ] || return 1
    case "${fields[0]}" in Z|X) return 2 ;; esac
    printf '%s' "${fields[19]}"
}
dag_under_root() {
    case "$1" in "$root"|"$root"/*) return 0 ;; esac
    return 1
}
dag_processes_inactive() {
    local proc pid before after uid arg previous referenced resolved cwd link key rest links match_status
    local -a argv
    [ -r /proc/self/stat ] && [ -d /proc/self/fd ] || dag_process_fail "Linux process inspection is unavailable"
    for proc in /proc/[0-9]*; do
        pid="${proc#/proc/}"
        [ "$pid" != "$BASHPID" ] && [ "$pid" != "$$" ] || continue
        before=$(dag_proc_identity "$pid") || {
            case "$?" in 2) continue ;; esac
            [ ! -d "$proc" ] && continue
            dag_process_fail "a live process identity is unreadable"
        }
        uid=''
        if [ -r "$proc/status" ]; then
            while read -r key rest; do
                if [ "$key" = Uid: ]; then read -r uid rest <<<"$rest"; break; fi
            done <"$proc/status"
        fi
        [ -n "$uid" ] || { [ ! -d "$proc" ] && continue; dag_process_fail "process owner is unreadable"; }
        argv=()
        if [ -r "$proc/cmdline" ]; then
            mapfile -d '' -t argv <"$proc/cmdline" 2>/dev/null || { [ ! -d "$proc" ] && continue; dag_process_fail "process arguments are unreadable"; }
        elif [ "$uid" = "$EUID" ]; then
            [ ! -d "$proc" ] && continue
            dag_process_fail "deployment-user process arguments are unreadable"
        fi
        previous=''
        for arg in "${argv[@]}"; do
            referenced=''
            case "$arg" in --datadir=*|--ethash.dagdir=*) referenced="${arg#*=}" ;; esac
            case "$previous" in --datadir|--ethash.dagdir) referenced="$arg" ;; esac
            if [ -n "$referenced" ]; then
                dag_under_root "$referenced" && dag_fail "A live process references this experiment directory"
                if [[ "$referenced" != /* ]]; then
                    cwd=$(readlink -- "$proc/cwd" 2>/dev/null) || cwd=''
                    [ -z "$cwd" ] || referenced="$cwd/$referenced"
                fi
                resolved=$(realpath -e -- "$referenced" 2>/dev/null) || resolved=''
                if [ -n "$resolved" ]; then dag_under_root "$resolved" && dag_fail "A live process references this experiment directory"; fi
            fi
            previous="$arg"
        done
        if [ -r "$proc/fd" ] && [ -x "$proc/fd" ]; then
            links=$(readlink -- "$proc"/fd/* "$proc/cwd" 2>/dev/null || true)
            while IFS= read -r link; do
                dag_under_root "$link" && dag_fail "A live process has this experiment directory open"
            done <<<"$links"
        elif [ "$uid" = "$EUID" ]; then
            [ ! -d "$proc" ] && continue
            dag_process_fail "deployment-user process descriptors are unreadable"
        fi
        if [ -r "$proc/maps" ]; then
            # A fixed-string search avoids copying potentially huge maps into
            # the shell. Exit 1 is no match; errors are never an empty list.
            if grep -F -- " $root/" "$proc/maps" >/dev/null 2>&1; then
                dag_fail "A live process maps files in this experiment directory"
            else
                match_status=$?
            fi
            if [ "$match_status" -ne 1 ]; then
                [ ! -d "$proc" ] && continue
                if [ "$uid" = "$EUID" ]; then dag_process_fail "deployment-user process mappings are unreadable"; fi
            fi
        elif [ "$uid" = "$EUID" ]; then
            [ ! -d "$proc" ] && continue
            dag_process_fail "deployment-user process mappings are unreadable"
        fi
        after=$(dag_proc_identity "$pid") || {
            case "$?" in 2) continue ;; esac
            [ ! -d "$proc" ] && continue
            dag_process_fail "process identity changed during inspection"
        }
        [ "$before" = "$after" ] || dag_process_fail "PID was reused during DAG inspection"
    done
}
dag_candidate() (
    mode="$1"; experiment_id="$2"; server_id="$3"; expected="$4"
    root="$workdir/experiments/$experiment_id/$server_id"
    cache="$root/ethash"; pinned_identity=''
    shopt -s nullglob dotglob
    dag_ancestry
    cd -P -- "$cache" || dag_fail "Cannot enter DAG cache"
    pinned_identity=$(stat -c '%d:%i' .) || dag_fail "Cannot pin DAG directory"
    dag_snapshot
    before="$fingerprint"
    dag_processes_inactive
    # Do not approve a manifest that changed during process inspection.
    dag_snapshot
    [ "$before" = "$fingerprint" ] || dag_fail "DAG files changed during inspection"
    if [ "$mode" = scan ]; then
        printf 'dag-cache\t%s\t%s\t%s\t%s\t%s\n' "$experiment_id" "$server_id" "$total_bytes" "${#names[@]}" "$fingerprint"
        exit 0
    fi
    [ "$fingerprint" = "$expected" ] || dag_fail "DAG cache changed since the reviewed scan; scan again"
    removed=0; freed=0
    for ((i=0; i<${#names[@]}; i++)); do
        dag_processes_inactive
        dag_ancestry
        remaining=(*)
        [ "${#remaining[@]}" -eq "$((${#names[@]} - i))" ] || dag_fail "DAG directory contents changed before deletion"
        for ((j=0; j<${#remaining[@]}; j++)); do
            [ "${remaining[j]}" = "${names[i+j]}" ] || dag_fail "DAG directory contents changed before deletion"
        done
        file="${names[i]}"
        [ ! -L "$file" ] && [ -f "$file" ] || dag_fail "DAG file changed before deletion"
        current=$(stat -c '%d|%i|%s|%b|%B|%y|%z|%h|%F' -- "$file") || dag_fail "Cannot recheck DAG file"
        [ "$file:$current" = "${metadata[i]}" ] || dag_fail "DAG file changed before deletion"
        unlink -- "$file" || dag_fail "DAG unlink failed"
        removed=$((removed + 1)); freed=$((freed + allocations[i]))
        printf 'dag-progress\t%s\t%s\n' "$freed" "$removed"
    done
    printf 'dag-done\t%s\t%s\n' "$freed" "$removed"
)
`
