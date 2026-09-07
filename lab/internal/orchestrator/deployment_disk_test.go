package orchestrator

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pfap/lab/internal/model"
)

func writeDiskRuntimeArchive(t *testing.T, path string, contents map[string]string) uint64 {
	t.Helper()
	var data bytes.Buffer
	compressed := gzip.NewWriter(&data)
	archive := tar.NewWriter(compressed)
	names := make([]string, 0, len(contents))
	for name := range contents {
		names = append(names, name)
	}
	sort.Strings(names)
	var expanded uint64
	for _, name := range names {
		content := contents[name]
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0700, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		expanded += uint64(len(content))
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return uint64(data.Len()) + expanded
}

func limitTestDiskPath(t *testing.T, bin, path string, availableKB uint64) {
	t.Helper()
	originalDF := filepath.Join(bin, "df-original")
	if err := os.Rename(filepath.Join(bin, "df"), originalDF); err != nil {
		t.Fatal(err)
	}
	df := "#!/bin/bash\nset -eu\nif [ \"$3\" = " + shell(path) + " ]; then export PFAP_TEST_DISK_KB=" + strconv.FormatUint(availableKB, 10) + "; fi\nexec " + shell(originalDF) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "df"), []byte(df), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeArchiveDiskBytesCountsExpandedAndUploadedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.tar.gz")
	want := writeDiskRuntimeArchive(t, path, map[string]string{
		"pfap-runtime/bin/geth":             strings.Repeat("runtime-data", 4096),
		"pfap-runtime/prfKey/create_pk.txt": "proof-key",
	})
	got, err := runtimeArchiveDiskBytes(context.Background(), path)
	if err != nil || got != want {
		t.Fatalf("archive reservation=%d want=%d err=%v", got, want, err)
	}
}

func TestRuntimeArchiveDiskBytesRejectsInvalidArchiveAndCancellation(t *testing.T) {
	for _, name := range []string{"raw bytes", "truncated gzip", "invalid tar", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runtime.tar.gz")
			writeDiskRuntimeArchive(t, path, map[string]string{"pfap-runtime/bin/geth": "runtime"})
			ctx := context.Background()
			switch name {
			case "raw bytes":
				if err := os.WriteFile(path, []byte("not a tar archive"), 0600); err != nil {
					t.Fatal(err)
				}
			case "truncated gzip":
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(path, info.Size()-5); err != nil {
					t.Fatal(err)
				}
			case "invalid tar":
				var data bytes.Buffer
				compressed := gzip.NewWriter(&data)
				_, _ = compressed.Write([]byte("not a tar archive"))
				if err := compressed.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := runtimeArchiveDiskBytes(ctx, path); err == nil {
				t.Fatal("invalid or cancelled archive inspection was accepted")
			}
		})
	}
}

func TestDeploymentDiskPreflightReservesDAGsForAssignedServers(t *testing.T) {
	for _, miners := range []int{1, 2} {
		t.Run(strconv.Itoa(miners), func(t *testing.T) {
			bin := fakeDiskCapacity(t, 4<<20, 5000)
			servers := map[string]model.Server{
				"first":  {ID: "first", Host: "local", WorkDir: t.TempDir()},
				"second": {ID: "second", Host: "local", WorkDir: t.TempDir()},
			}
			// The second server has enough for a nonminer and the runtime, but
			// cannot reserve both DAGs. Two miners must spread to that server.
			limitTestDiskPath(t, bin, servers["second"].WorkDir, 2<<20)
			path := filepath.Join(t.TempDir(), "runtime.tar.gz")
			writeDiskRuntimeArchive(t, path, map[string]string{"pfap-runtime/bin/geth": "runtime"})
			exp := model.Experiment{MinerCount: miners, Placements: []model.Placement{{ServerID: "first", Count: 2}, {ServerID: "second", Count: 1}}}
			err := (Orchestrator{}).deploymentDiskPreflight(context.Background(), exp, servers, path)
			if miners == 1 && err != nil {
				t.Fatalf("nonminer server was required to reserve DAGs: %v", err)
			}
			if miners == 2 && (err == nil || !strings.Contains(err.Error(), "second") || !strings.Contains(err.Error(), "insufficient free disk space")) {
				t.Fatalf("distributed miner reservation missing: %v", err)
			}
			for _, server := range servers {
				entries, err := os.ReadDir(server.WorkDir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("preflight wrote to worker %s: %v %v", server.ID, entries, err)
				}
			}
		})
	}
}

func TestDeploymentDiskPreflightIncludesRuntimeAndConcurrentMinerDAGs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.tar.gz")
	runtimeBytes := writeDiskRuntimeArchive(t, path, map[string]string{"pfap-runtime/bin/geth": strings.Repeat("binary", 4096)})
	availableKB := (MinimumDiskFreeBytes + 2*(MiningDiskRequiredBytes(0)-MinimumDiskFreeBytes) + runtimeBytes + 1023) / 1024
	fakeDiskCapacity(t, availableKB, 5000)
	server := model.Server{ID: "worker", Host: "local", WorkDir: t.TempDir()}
	exp := model.Experiment{MinerCount: 2, Placements: []model.Placement{{ServerID: server.ID, Count: 2}}}
	servers := map[string]model.Server{server.ID: server}
	if err := (Orchestrator{}).deploymentDiskPreflight(context.Background(), exp, servers, path); err != nil {
		t.Fatalf("sufficient runtime and concurrent DAG space was rejected: %v", err)
	}
	t.Setenv("PFAP_TEST_DISK_KB", strconv.FormatUint(availableKB-1, 10))
	if err := (Orchestrator{}).deploymentDiskPreflight(context.Background(), exp, servers, path); err == nil || !strings.Contains(err.Error(), "includes runtime archive and extracted files") {
		t.Fatalf("runtime storage was omitted from reservation: %v", err)
	}
	t.Setenv("PFAP_TEST_DISK_KB", strconv.FormatUint((MiningDiskRequiredBytes(0)+runtimeBytes+1023)/1024, 10))
	if err := (Orchestrator{}).deploymentDiskPreflight(context.Background(), exp, servers, path); err == nil {
		t.Fatal("one DAG pair reservation was incorrectly shared by concurrent miners")
	}
}

func TestDeployChecksAllDisksBeforeAnyWorkerWrites(t *testing.T) {
	bin := fakeDiskCapacity(t, 4<<20, 5000)
	servers := map[string]model.Server{
		"first":  {ID: "first", Host: "local", WorkDir: t.TempDir()},
		"second": {ID: "second", Host: "local", WorkDir: t.TempDir()},
	}
	limitTestDiskPath(t, bin, servers["second"].WorkDir, 0)
	path := filepath.Join(t.TempDir(), "runtime.tar.gz")
	writeDiskRuntimeArchive(t, path, map[string]string{"pfap-runtime/bin/geth": "runtime"})
	exp := model.Experiment{ID: "preflight", ArtifactPath: path, MinerCount: 1, Placements: []model.Placement{{ServerID: "first", Count: 1}, {ServerID: "second", Count: 1}}}
	_, err := (Orchestrator{}).Deploy(context.Background(), &exp, servers, nil)
	if err == nil || !strings.Contains(err.Error(), "second") || !strings.Contains(err.Error(), "insufficient free disk space") {
		t.Fatalf("later target was not checked before deployment: %v", err)
	}
	for _, server := range servers {
		entries, err := os.ReadDir(server.WorkDir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("deployment wrote to %s before checking all disks: %v %v", server.ID, entries, err)
		}
	}
}

func TestManualDeploymentDiskPreflightUsesSelectedWorker(t *testing.T) {
	bin := fakeDiskCapacity(t, 4<<20, 5000)
	servers := map[string]model.Server{"first": {ID: "first", Host: "local", WorkDir: t.TempDir()}, "second": {ID: "second", Host: "local", WorkDir: t.TempDir()}}
	limitTestDiskPath(t, bin, servers["second"].WorkDir, 2<<20)
	archive := filepath.Join(t.TempDir(), "runtime.tar.gz")
	writeDiskRuntimeArchive(t, archive, map[string]string{"pfap-runtime/bin/geth": "runtime"})
	exp := model.Experiment{MinerMode: "manual", MinerSelections: []model.MinerSelection{{ServerID: "second", LocalIndex: 1}}, Placements: []model.Placement{{ServerID: "first", Count: 1}, {ServerID: "second", Count: 1}}}
	if err := (Orchestrator{}).deploymentDiskPreflight(context.Background(), exp, servers, archive); err == nil || !strings.Contains(err.Error(), "second") {
		t.Fatalf("manual worker's DAG reserve omitted: %v", err)
	}
	exp.MinerSelections = []model.MinerSelection{{ServerID: "first", LocalIndex: 1}}
	if err := (Orchestrator{}).deploymentDiskPreflight(context.Background(), exp, servers, archive); err != nil {
		t.Fatalf("unselected worker incorrectly required DAG space: %v", err)
	}
}
