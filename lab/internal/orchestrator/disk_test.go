package orchestrator

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pfap/lab/internal/model"
)

// This df replacement lets startup tests exercise capacity failures without
// filling, mounting or changing any real filesystem.
func fakeDiskCapacity(t *testing.T, availableKB, availableInodes uint64) string {
	t.Helper()
	bin := t.TempDir()
	df := `#!/bin/bash
set -eu
if [ -n "${PFAP_TEST_DF_LOG:-}" ]; then printf '%s\n' "$*" >>"$PFAP_TEST_DF_LOG"; fi
if [ -n "${PFAP_TEST_DF_FAIL:-}" ]; then printf 'df: cannot inspect filesystem\n' >&2; exit 1; fi
printf 'Filesystem Total Used Available Use%% Mounted\n'
if [ "$1" = -Pi ]; then
    printf 'testfs 999999999 0 %s 0%% /\n' "$PFAP_TEST_DISK_INODES"
else
    printf 'testfs 999999999 0 %s 0%% /\n' "$PFAP_TEST_DISK_KB"
fi
`
	if err := os.WriteFile(filepath.Join(bin, "df"), []byte(df), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PFAP_TEST_DISK_KB", strconv.FormatUint(availableKB, 10))
	t.Setenv("PFAP_TEST_DISK_INODES", strconv.FormatUint(availableInodes, 10))
	return bin
}

func TestCheckDiskUsesWorkDirParentAndDoesNotCreateIt(t *testing.T) {
	fakeDiskCapacity(t, 4<<20, 5000)
	parent := t.TempDir()
	workDir := filepath.Join(parent, "space and 'quotes'", "future")
	log := filepath.Join(t.TempDir(), "df.log")
	t.Setenv("PFAP_TEST_DF_LOG", log)
	server := model.Server{ID: "local", Host: "local", WorkDir: workDir}
	if err := (Orchestrator{}).CheckDisk(context.Background(), server, MinimumDiskFreeBytes); err != nil {
		t.Fatal(err)
	}
	if got := testRead(t, log); got != "-Pk -- "+parent+"\n-Pi -- "+parent+"\n" {
		t.Fatalf("capacity was inspected outside the WorkDir filesystem: %q", got)
	}
	if _, err := os.Stat(filepath.Join(parent, "space and 'quotes'")); !os.IsNotExist(err) {
		t.Fatalf("preflight created a missing directory: %v", err)
	}
	usage, err := (Orchestrator{}).DiskUsage(context.Background(), server)
	if err != nil || usage.Path != parent || usage.AvailableBytes != 4<<30 || usage.InodesAvailable != 5000 {
		t.Fatalf("unexpected usage: %+v, %v", usage, err)
	}
}

func TestCheckDiskRejectsLowCapacityAndUnknownStats(t *testing.T) {
	for _, tc := range []struct {
		name, kb, inodes, want string
	}{
		{"bytes", "0", "5000", "insufficient free disk space"},
		{"baseline enforced", "1048575", "5000", "required 1048576 KiB"},
		{"inodes", "4194304", "0", "insufficient free disk inodes"},
		{"few inodes", "4194304", "1023", "insufficient free disk inodes"},
		{"unknown inodes", "4194304", "-", "invalid or unavailable"},
		{"invalid bytes", "unavailable", "5000", "invalid or unavailable"},
		{"overflow bytes", "18446744073709551615", "5000", "invalid or unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeDiskCapacity(t, 0, 0)
			t.Setenv("PFAP_TEST_DISK_KB", tc.kb)
			t.Setenv("PFAP_TEST_DISK_INODES", tc.inodes)
			server := model.Server{ID: "local", Host: "local", WorkDir: t.TempDir()}
			err := (Orchestrator{}).CheckDisk(context.Background(), server, 0)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCheckDiskRefusesUninspectablePaths(t *testing.T) {
	for _, name := range []string{"relative", "root", "file parent", "dangling symlink", "df failure"} {
		t.Run(name, func(t *testing.T) {
			fakeDiskCapacity(t, 4<<20, 5000)
			server := model.Server{ID: "local", Host: "local", WorkDir: t.TempDir()}
			switch name {
			case "relative":
				server.WorkDir = "relative/workdir"
			case "root":
				server.WorkDir = "/"
			case "file parent":
				path := filepath.Join(server.WorkDir, "file")
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
				server.WorkDir = path + "/nested"
			case "dangling symlink":
				path := filepath.Join(server.WorkDir, "link")
				if err := os.Symlink("missing", path); err != nil {
					t.Fatal(err)
				}
				server.WorkDir = path + "/nested"
			case "df failure":
				t.Setenv("PFAP_TEST_DF_FAIL", "1")
			}
			if err := (Orchestrator{}).CheckDisk(context.Background(), server, 0); err == nil {
				t.Fatal("uninspectable WorkDir accepted")
			}
		})
	}
}

func TestMiningDiskReservationGrowsWithEpochAndCannotOverflow(t *testing.T) {
	const genesis = 3<<30 + 8<<20
	if got := MiningDiskRequiredBytes(0); got != genesis {
		t.Fatalf("initial reserve %d, want %d", got, uint64(genesis))
	}
	if got := MiningDiskRequiredBytes(29999); got != genesis {
		t.Fatalf("epoch changed too early: %d", got)
	}
	if got := MiningDiskRequiredBytes(30000); got != genesis+16<<20 {
		t.Fatalf("reserve did not include current and next epoch growth: %d", got)
	}
	if got := MiningDiskRequiredBytes(math.MaxUint64); got != math.MaxUint64 {
		t.Fatalf("block overflow underestimated reserve: %d", got)
	}
}

func TestMinerServerDiskReservationCountsConcurrentProcesses(t *testing.T) {
	server := model.Server{ID: "worker"}
	node := model.Node{ID: "one", ServerID: server.ID, LocalIndex: 1, Index: 1, IsMiner: true}
	mining := true
	for _, tc := range []struct {
		name string
		exp  model.Experiment
		want uint64
	}{
		{name: "missing manifest", want: MiningDiskRequiredBytes(0)},
		{name: "one recorded miner", exp: model.Experiment{MinerCount: 1, Nodes: []model.Node{node}}, want: MiningDiskRequiredBytes(0)},
		{name: "two same-server cold miners", exp: model.Experiment{MinerCount: 2, Nodes: []model.Node{node, {ID: "two", ServerID: server.ID, LocalIndex: 2, IsMiner: true}}}, want: 5<<30 + 16<<20},
		{name: "other server excluded", exp: model.Experiment{MinerCount: 2, Nodes: []model.Node{node, {ID: "other", ServerID: "elsewhere", LocalIndex: 2, IsMiner: true}}}, want: MiningDiskRequiredBytes(0)},
		{name: "highest server epoch", exp: model.Experiment{MinerCount: 2, Nodes: []model.Node{node, {ID: "two", ServerID: server.ID, LocalIndex: 2, IsMiner: true, Block: 60000}}}, want: MinimumDiskFreeBytes + 2*(MiningDiskRequiredBytes(60000)-MinimumDiskFreeBytes)},
		{name: "unrecorded target included", exp: model.Experiment{MinerCount: 1, Nodes: []model.Node{{ID: "two", ServerID: server.ID, LocalIndex: 2, IsMiner: true}}}, want: 5<<30 + 16<<20},
		{name: "legacy configured miner", exp: model.Experiment{Nodes: []model.Node{{ID: "one", ServerID: server.ID, LocalIndex: 1, Index: 1}}}, want: MiningDiskRequiredBytes(0)},
		{name: "retiring miner still observed active", exp: model.Experiment{MinerCount: 1, Nodes: []model.Node{node, {ID: "retiring", ServerID: server.ID, LocalIndex: 2, Mining: &mining}}}, want: 5<<30 + 16<<20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := minerServerDiskRequiredBytes(tc.exp, node, server); got != tc.want {
				t.Fatalf("reserve=%d want=%d", got, tc.want)
			}
		})
	}
	if got := miningDiskRequiredBytesForMiners(0, math.MaxUint64); got != math.MaxUint64 {
		t.Fatalf("miner count overflow underestimated reserve: %d", got)
	}
	if got := miningDiskRequiredBytesForMiners(math.MaxUint64, 2); got != math.MaxUint64 {
		t.Fatalf("epoch overflow underestimated multi-miner reserve: %d", got)
	}
}

func TestCheckDiskRefusesUnreadableExistingParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can inspect mode-000 directories")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
	server := model.Server{ID: "local", Host: "local", WorkDir: filepath.Join(parent, "missing", "nested")}
	if err := (Orchestrator{}).CheckDisk(context.Background(), server, 0); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("unreadable parent was not rejected explicitly: %v", err)
	}
}
