package orchestrator

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pfap/lab/internal/model"
)

const (
	// MinimumDiskFreeBytes leaves space for chain state, logs and startup files.
	MinimumDiskFreeBytes  uint64 = 1 << 30
	MinimumDiskFreeInodes uint64 = 1024
)

type DiskUsage struct {
	Path            string `json:"path"`
	TotalBytes      uint64 `json:"totalBytes"`
	AvailableBytes  uint64 `json:"availableBytes"`
	InodesTotal     uint64 `json:"inodesTotal"`
	InodesAvailable uint64 `json:"inodesAvailable"`
}

func (o Orchestrator) DiskUsage(ctx context.Context, server model.Server) (DiskUsage, error) {
	script, err := diskStatsScript(server)
	if err != nil {
		return DiskUsage{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := o.Remote.Run(ctx, server, "set -euo pipefail\n"+script)
	if err != nil {
		return DiskUsage{}, fmt.Errorf("read disk capacity on %s: %w (%s)", server.ID, err, strings.TrimSpace(out))
	}
	values := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	usage := DiskUsage{Path: values["disk_path"]}
	for key, dest := range map[string]*uint64{
		"disk_total_kb": &usage.TotalBytes, "disk_available_kb": &usage.AvailableBytes,
		"disk_total_inodes": &usage.InodesTotal, "disk_available_inodes": &usage.InodesAvailable,
	} {
		value, err := strconv.ParseUint(values[key], 10, 64)
		if err != nil {
			return DiskUsage{}, fmt.Errorf("read disk capacity on %s: invalid %s", server.ID, key)
		}
		if strings.HasSuffix(key, "_kb") {
			if value > math.MaxUint64/1024 {
				return DiskUsage{}, fmt.Errorf("read disk capacity on %s: %s overflows bytes", server.ID, key)
			}
			value *= 1024
		}
		*dest = value
	}
	if usage.Path == "" {
		return DiskUsage{}, fmt.Errorf("read disk capacity on %s: missing filesystem path", server.ID)
	}
	return usage, nil
}

// MiningDiskRequiredBytes reserves both the current and next Ethash dataset,
// plus the baseline headroom. Dataset generation writes a temporary file then
// renames it, so the reservation also covers those temporary generation files.
// Existing cache files receive no credit: their names and apparent sizes cannot
// prove generation completed. This conservative check requires just over 3 GiB
// at epoch zero, grows by 16 MiB per epoch, and covers only the current/next
// epochs at the last observed block, not indefinite future chain growth.
func MiningDiskRequiredBytes(block uint64) uint64 {
	const growth = uint64(1 << 23)
	const initial = 3*MinimumDiskFreeBytes + growth
	epoch := block / 30000
	if epoch > (math.MaxUint64-initial)/(2*growth) {
		return math.MaxUint64
	}
	return initial + epoch*2*growth
}

func miningDiskRequiredBytesForMiners(block, miners uint64) uint64 {
	if miners < 1 {
		miners = 1
	}
	datasets := MiningDiskRequiredBytes(block) - MinimumDiskFreeBytes
	if datasets > (math.MaxUint64-MinimumDiskFreeBytes)/miners {
		return math.MaxUint64
	}
	return MinimumDiskFreeBytes + miners*datasets
}

// Each geth process can generate its own temporary current/next DAG before
// either is ready for reuse. A shared directory therefore does not make cold
// concurrent miners share one allocation. Reserve for all configured miners on
// this server and observed miners still running during a role transition,
// including the target being enabled, at their highest known block.
// This is a conservative snapshot; it cannot reserve filesystem space against
// unrelated writers or guarantee future epoch capacity.
func minerServerDiskRequiredBytes(exp model.Experiment, node model.Node, server model.Server) uint64 {
	var miners uint64
	block := node.Block
	targetCounted := false
	for _, candidate := range exp.Nodes {
		observedMining := candidate.Mining != nil && *candidate.Mining
		if candidate.ServerID != server.ID || (!model.NodeIsMiner(exp, candidate) && !observedMining) {
			continue
		}
		miners++
		if candidate.Block > block {
			block = candidate.Block
		}
		if (candidate.ID != "" && candidate.ID == node.ID) || (candidate.LocalIndex > 0 && candidate.LocalIndex == node.LocalIndex) {
			targetCounted = true
		}
	}
	if !targetCounted {
		miners++
	}
	return miningDiskRequiredBytesForMiners(block, miners)
}

// CheckDisk inspects the filesystem containing WorkDir, or its nearest existing
// parent before deployment creates it. It performs no write probe and uses df's
// available blocks, which exclude space reserved for root.
func (o Orchestrator) CheckDisk(ctx context.Context, server model.Server, requiredBytes uint64) error {
	script, err := diskPreflightScript(server, requiredBytes)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := o.Remote.Run(ctx, server, "set -euo pipefail\n"+script)
	if err != nil {
		return fmt.Errorf("disk preflight on %s (%s): %w (%s)", server.ID, server.WorkDir, err, strings.TrimSpace(out))
	}
	return nil
}

func diskStatsScript(server model.Server) (string, error) {
	if !filepath.IsAbs(server.WorkDir) || filepath.Clean(server.WorkDir) == "/" || strings.ContainsAny(server.WorkDir, "\x00\r\n") {
		return "", fmt.Errorf("disk preflight: server %s requires an absolute non-root work directory", server.ID)
	}
	return "disk_path=" + shell(strings.TrimRight(server.WorkDir, "/")) + "\n" + diskStatsShell, nil
}

func diskPreflightScript(server model.Server, requiredBytes uint64) (string, error) {
	script, err := diskStatsScript(server)
	if err != nil {
		return "", err
	}
	if requiredBytes < MinimumDiskFreeBytes {
		requiredBytes = MinimumDiskFreeBytes
	}
	requiredKB := requiredBytes / 1024
	if requiredBytes%1024 != 0 {
		requiredKB++
	}
	return script + "disk_required_kb=" + strconv.FormatUint(requiredKB, 10) + "\n" +
		"disk_required_inodes=" + strconv.FormatUint(MinimumDiskFreeInodes, 10) + "\n" + `
[ "$disk_available_kb" -ge "$disk_required_kb" ] || disk_fail "insufficient free disk space at $disk_path: available $disk_available_kb KiB, required $disk_required_kb KiB (includes DAG/headroom reservation when mining)"
[ "$disk_available_inodes" -ge "$disk_required_inodes" ] || disk_fail "insufficient free disk inodes at $disk_path: available $disk_available_inodes, required $disk_required_inodes"
`, nil
}

const diskStatsShell = `
disk_fail() { printf '[ERROR] disk preflight: %s\n' "$*" >&2; exit 1; }
while [ ! -e "$disk_path" ]; do
    [ ! -L "$disk_path" ] || disk_fail "work directory contains a dangling symlink: $disk_path"
    [ "$disk_path" != / ] || disk_fail "cannot inspect the filesystem for the work directory"
    disk_path="${disk_path%/*}"
    [ -n "$disk_path" ] || disk_path=/
done
[ -d "$disk_path" ] || disk_fail "work directory or its existing parent is not a directory: $disk_path"
[ -r "$disk_path" ] && [ -x "$disk_path" ] || disk_fail "work directory or its existing parent is unreadable: $disk_path"
disk_blocks=$(LC_ALL=C df -Pk -- "$disk_path") || disk_fail "cannot read free disk space at $disk_path"
disk_inodes=$(LC_ALL=C df -Pi -- "$disk_path") || disk_fail "cannot read free disk inodes at $disk_path"
read -r disk_total_kb disk_available_kb <<<"$(printf '%s\n' "$disk_blocks" | awk 'NR==2 {print $2, $4}')"
read -r disk_total_inodes disk_available_inodes <<<"$(printf '%s\n' "$disk_inodes" | awk 'NR==2 {print $2, $4}')"
for disk_value in "$disk_total_kb" "$disk_available_kb" "$disk_total_inodes" "$disk_available_inodes"; do
    [[ "$disk_value" =~ ^[0-9]{1,18}$ ]] || disk_fail "invalid or unavailable filesystem capacity at $disk_path"
done
printf 'disk_path=%s\ndisk_total_kb=%s\ndisk_available_kb=%s\ndisk_total_inodes=%s\ndisk_available_inodes=%s\n' "$disk_path" "$disk_total_kb" "$disk_available_kb" "$disk_total_inodes" "$disk_available_inodes"
`
