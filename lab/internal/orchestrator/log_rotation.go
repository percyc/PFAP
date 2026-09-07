package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pfap/lab/internal/model"
)

const LogMaxBytes int64 = 64 << 20
const LogRetainFiles = 3

const logRotationDeadline = 12 * time.Second

// RotateNodeLog preserves the open log inode used by geth. The API coordinates
// this operation with transaction execution to preserve active timing records.
// Like logrotate copytruncate, concurrent log lines in the copy/truncate window
// may be lost; chain data and persisted transaction metrics are unaffected.
func (o Orchestrator) RotateNodeLog(ctx context.Context, exp model.Experiment, node model.Node, server model.Server) (bool, error) {
	script, err := logRotationScript(exp, node, server, LogMaxBytes)
	if err != nil {
		return false, err
	}
	out, err := o.Remote.Run(ctx, server, script)
	if err != nil {
		// SSH cancellation does not guarantee remote command cancellation. Keep
		// the caller's transaction/lifecycle locks through the worker timeout,
		// even if the transport died before that deadline was observed locally.
		var exitError *exec.ExitError
		if ctx.Err() != nil || (errors.As(err, &exitError) && exitError.ExitCode() == 255) {
			time.Sleep(logRotationDeadline + 2*time.Second)
		}
		return false, fmt.Errorf("rotate %s log: %w (%s)", node.Name, err, strings.TrimSpace(out))
	}
	return strings.Contains(out, "log_rotation=rotated"), nil
}

func logRotationScript(exp model.Experiment, node model.Node, server model.Server, maxBytes int64) (string, error) {
	if !recoveryID.MatchString(exp.ID) || !recoveryID.MatchString(server.ID) || node.ServerID != server.ID || node.LocalIndex < 1 || maxBytes < 1 || !filepath.IsAbs(server.WorkDir) || filepath.Clean(server.WorkDir) == "/" {
		return "", errors.New("invalid log rotation target")
	}
	base := filepath.Clean(server.WorkDir)
	dir := filepath.Join(base, "experiments", exp.ID, server.ID, "node"+strconv.Itoa(node.LocalIndex))
	body := "set -euo pipefail\nbase=" + shell(base) + "\ndir=" + shell(dir) + "\nmax=" + strconv.FormatInt(maxBytes, 10) + `
fail() { printf '[ERROR] %s\n' "$*" >&2; exit 1; }
[ -d "$dir" ] || { printf 'log_rotation=missing\n'; exit 0; }
[ "$(readlink -f -- "$dir")" = "$dir" ] || fail "Log directory contains a symlink"
log="$dir/geth.log"
[ ! -L "$log" ] || fail "Refusing symlink log"
[ -f "$log" ] || { printf 'log_rotation=missing\n'; exit 0; }
[ "$(stat -c %h -- "$log")" = 1 ] || fail "Refusing hardlinked log"
size=$(stat -c %s -- "$log")
[ "$size" -ge "$max" ] || { printf 'log_rotation=below-limit\n'; exit 0; }
lock="$dir/.lab-log-maintenance.lock"
[ ! -L "$lock" ] || fail "Refusing symlink rotation lock"
if [ -e "$lock" ]; then [ -f "$lock" ] && [ "$(stat -c %h -- "$lock")" = 1 ] || fail "Refusing unsafe rotation lock"; fi
exec 8>"$lock"
flock -n 8 || { printf 'log_rotation=busy\n'; exit 0; }
for archive in "$log.1" "$log.2" "$log.3"; do
    [ ! -L "$archive" ] || fail "Refusing symlink log archive"
    if [ -e "$archive" ]; then
        [ -f "$archive" ] && [ "$(stat -c %h -- "$archive")" = 1 ] || fail "Refusing nonregular or hardlinked log archive"
    fi
done
available=$(df -Pk -- "$dir" | awk 'END {print $4}')
[[ "$available" =~ ^[0-9]+$ ]] || fail "Cannot inspect log archive capacity"
[ "$((available * 1024))" -ge "$((size + 16777216))" ] || fail "磁盘空间不足以安全归档日志，请先释放缓存空间"
identity=$(stat -c '%d:%i' -- "$log")
tmp=$(mktemp "$dir/.lab-log-rotation.XXXXXX")
trap 'rm -f -- "$tmp"' EXIT
cp -- "$log" "$tmp"
sync -f "$tmp"
[ "$(stat -c '%d:%i' -- "$log")" = "$identity" ] && [ ! -L "$log" ] || fail "Log file changed during rotation"
if [ -f "$log.2" ]; then mv -f -- "$log.2" "$log.3"; fi
if [ -f "$log.1" ]; then mv -f -- "$log.1" "$log.2"; fi
mv -f -- "$tmp" "$log.1"
truncate -s 0 -- "$log"
printf 'log_rotation=rotated\n'
`
	// A worker-enforced process-group deadline also kills a stalled cp/sync.
	// The controller context is longer, and on transport errors it waits through
	// this bounded lease before admitting a transaction or recovery.
	return "set -euo pipefail\ncommand -v timeout >/dev/null\ncommand -v flock >/dev/null\ntimeout -k 1s " + strconv.Itoa(int(logRotationDeadline/time.Second)) + "s bash -c " + shell(body) + "\n", nil
}

// Read retained logs oldest to newest so a timing boundary spanning rotation
// remains discoverable. Each file is bounded independently.
func retainedLogTail(logPath string, lines int) string {
	return "log=" + shell(logPath) + "\nfor f in \"$log.3\" \"$log.2\" \"$log.1\" \"$log\"; do if [ -f \"$f\" ] && [ ! -L \"$f\" ]; then tail -n " + strconv.Itoa(lines) + " -- \"$f\"; fi; done\n"
}
