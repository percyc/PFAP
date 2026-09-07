package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
)

func dagFixture(t *testing.T) (Orchestrator, model.Server, model.Experiment, string) {
	t.Helper()
	for _, command := range []string{"bash", "stat", "realpath", "sha256sum", "unlink"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("requires %s", command)
		}
	}
	server := model.Server{ID: "worker-new", Host: "local", WorkDir: t.TempDir()}
	exp := model.Experiment{
		ID: "exp-history", Status: "stopped",
		Placements: []model.Placement{{ServerID: "worker-old", Count: 1}},
		Nodes:      []model.Node{{ID: "node-old", ServerID: "worker-old", Status: "stopped"}},
	}
	cache := filepath.Join(server.WorkDir, "experiments", exp.ID, "worker-old", "ethash")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "full-R23-0000000000000000"), []byte("cached DAG"), 0600); err != nil {
		t.Fatal(err)
	}
	return Orchestrator{}, server, exp, cache
}

func scanOneDAG(t *testing.T, o Orchestrator, server model.Server, exp model.Experiment) CacheTarget {
	t.Helper()
	targets, err := o.ScanDAGCaches(context.Background(), server, []model.Experiment{exp})
	if err != nil && strings.Contains(err.Error(), "Cannot safely inspect worker processes") && strings.Contains(err.Error(), "unreadable") {
		t.Skipf("integration requires access to deployment-user /proc entries (run test binary as root): %v", err)
	}
	if err != nil || len(targets) != 1 {
		out, _ := o.Remote.Run(context.Background(), server, "set -euo pipefail\nexport LC_ALL=C\nworkdir="+shell(server.WorkDir)+"\n"+dagCacheScript+"dag_candidate scan "+shell(exp.ID)+" "+shell(exp.Placements[0].ServerID)+" ''\n")
		t.Fatalf("DAG scan targets=%+v error=%v output=%s", targets, err, out)
	}
	return targets[0]
}

func TestDAGCleanupHistoricalPlacementPreservesState(t *testing.T) {
	o, server, exp, cache := dagFixture(t)
	protected := map[string]string{
		"node1/geth/chaindata/00001.ldb": "chain",
		"node1/SN":                       "SN commitments",
		"node1/keystore/key.json":        "account key",
		"node1/address":                  "account address",
		"password.txt":                   "password",
		"prfKey/private.txt":             "proof key",
	}
	for name, contents := range protected {
		path := filepath.Join(filepath.Dir(cache), name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cache, "full-R23-aaaaaaaaaaaaaaaa.be.12938"), []byte("temporary DAG"), 0600); err != nil {
		t.Fatal(err)
	}
	target := scanOneDAG(t, o, server, exp)
	if target.Path != cache || target.ServerID != "worker-old" || target.Files != 2 || target.Bytes < 1 {
		t.Fatalf("unexpected target: %+v", target)
	}
	result, err := o.CleanupDAGCache(context.Background(), server, target)
	if err != nil || result.Status != "cleaned" || result.RemovedFiles != 2 || result.FreedBytes != target.Bytes {
		t.Fatalf("cleanup result=%+v error=%v", result, err)
	}
	entries, err := os.ReadDir(cache)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cache was not left empty: %v %v", entries, err)
	}
	for name, contents := range protected {
		if got := testRead(t, filepath.Join(filepath.Dir(cache), name)); got != contents {
			t.Fatalf("changed protected file %s: %q", name, got)
		}
	}
}

func TestDAGScanRejectsUnsafeEntriesAndAncestry(t *testing.T) {
	for _, kind := range []string{"symlink-file", "hardlink", "directory", "unknown-file", "symlink-cache", "symlink-workdir", "dangling-parent"} {
		t.Run(kind, func(t *testing.T) {
			o, server, exp, cache := dagFixture(t)
			file := filepath.Join(cache, "full-R23-0000000000000000")
			var err error
			switch kind {
			case "symlink-file":
				err = os.Symlink(file, filepath.Join(cache, "full-R23-1111111111111111"))
			case "hardlink":
				err = os.Link(file, filepath.Join(t.TempDir(), "protected"))
			case "directory":
				err = os.Mkdir(filepath.Join(cache, "full-R23-1111111111111111"), 0700)
			case "unknown-file":
				err = os.WriteFile(filepath.Join(cache, "SN"), []byte("private state"), 0600)
			case "symlink-cache":
				moved := filepath.Join(t.TempDir(), "outside")
				if err = os.Rename(cache, moved); err == nil {
					err = os.Symlink(moved, cache)
				}
			case "symlink-workdir":
				alias := filepath.Join(t.TempDir(), "alias")
				if err = os.Symlink(server.WorkDir, alias); err == nil {
					server.WorkDir = alias
				}
			case "dangling-parent":
				moved := filepath.Join(t.TempDir(), "preserved")
				parent := filepath.Dir(cache)
				if err = os.Rename(parent, moved); err == nil {
					err = os.Symlink(filepath.Join(t.TempDir(), "missing"), parent)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			targets, err := o.ScanDAGCaches(context.Background(), server, []model.Experiment{exp})
			if err != nil || len(targets) != 0 {
				t.Fatalf("unsafe cache was selected: %+v %v", targets, err)
			}
		})
	}
}

func TestDAGScanRequiresStoppedRegisteredManifest(t *testing.T) {
	o, server, exp, _ := dagFixture(t)
	for _, status := range []string{"running", "interrupted", "unknown", "starting", ""} {
		exp.Status = status
		got, err := o.ScanDAGCaches(context.Background(), server, []model.Experiment{exp})
		if err != nil || len(got) != 0 {
			t.Fatalf("status %q accepted: %+v %v", status, got, err)
		}
	}
	exp.Status = "failed"
	for _, status := range []string{"running", "unknown", "failed", ""} {
		exp.Nodes[0].Status = status
		got, err := o.ScanDAGCaches(context.Background(), server, []model.Experiment{exp})
		if err != nil || len(got) != 0 {
			t.Fatalf("node status %q accepted: %+v %v", status, got, err)
		}
	}
	exp.Nodes = nil
	got, err := o.ScanDAGCaches(context.Background(), server, []model.Experiment{exp})
	if err != nil || len(got) != 0 {
		t.Fatalf("unverified legacy manifest accepted: %+v %v", got, err)
	}
	got, err = o.ScanDAGCaches(context.Background(), server, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("unregistered on-disk cache accepted: %+v %v", got, err)
	}
}

func TestDAGCleanupRejectsStaleFingerprintAndMaliciousPaths(t *testing.T) {
	o, server, exp, cache := dagFixture(t)
	target := scanOneDAG(t, o, server, exp)
	for _, path := range []string{server.WorkDir, filepath.Dir(cache), cache + "/../node1", "/", "/tmp/other/ethash", cache + "/"} {
		changed := target
		changed.Path = path
		result, err := o.CleanupDAGCache(context.Background(), server, changed)
		if err == nil || result.RemovedFiles != 0 {
			t.Fatalf("malicious path %q accepted: %+v %v", path, result, err)
		}
	}
	file := filepath.Join(cache, "full-R23-0000000000000000")
	if err := os.WriteFile(file, []byte("replacement DAG"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := o.CleanupDAGCache(context.Background(), server, target)
	if err == nil || result.RemovedFiles != 0 || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("stale fingerprint accepted: %+v %v", result, err)
	}
	if got := testRead(t, file); got != "replacement DAG" {
		t.Fatalf("replacement changed: %q", got)
	}
}

func TestDAGCleanupRejectsLiveDatadirEvenWithStalePID(t *testing.T) {
	o, server, exp, cache := dagFixture(t)
	target := scanOneDAG(t, o, server, exp)
	dir := filepath.Join(filepath.Dir(cache), "node1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "geth.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", "while :; do sleep 0.1; done", "fake-runtime", "--datadir="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	result, err := o.CleanupDAGCache(context.Background(), server, target)
	if err == nil || result.RemovedFiles != 0 || !strings.Contains(err.Error(), "live process") {
		t.Fatalf("live datadir accepted: %+v %v", result, err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("cleanup signalled the process")
	}
	if _, err := os.Stat(filepath.Join(cache, "full-R23-0000000000000000")); err != nil {
		t.Fatal("cleanup removed the active DAG")
	}
}

func TestDAGCleanupRejectsOpenAndMappedDAG(t *testing.T) {
	for _, kind := range []string{"open", "mapped"} {
		t.Run(kind, func(t *testing.T) {
			o, server, exp, cache := dagFixture(t)
			target := scanOneDAG(t, o, server, exp)
			file, err := os.Open(filepath.Join(cache, "full-R23-0000000000000000"))
			if err != nil {
				t.Fatal(err)
			}
			if kind == "mapped" {
				data, err := syscall.Mmap(int(file.Fd()), 0, 10, syscall.PROT_READ, syscall.MAP_SHARED)
				if err != nil {
					_ = file.Close()
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = syscall.Munmap(data) })
				_ = file.Close()
			} else {
				t.Cleanup(func() { _ = file.Close() })
			}
			result, err := o.CleanupDAGCache(context.Background(), server, target)
			if err == nil || result.RemovedFiles != 0 || !strings.Contains(err.Error(), "live process") {
				t.Fatalf("%s DAG accepted: %+v %v", kind, result, err)
			}
		})
	}
}

func TestDAGCleanupIgnoresReusedRecordedPID(t *testing.T) {
	o, server, exp, cache := dagFixture(t)
	dir := filepath.Join(filepath.Dir(cache), "node1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "geth.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	target := scanOneDAG(t, o, server, exp)
	result, err := o.CleanupDAGCache(context.Background(), server, target)
	if err != nil || result.RemovedFiles != 1 {
		t.Fatalf("unrelated recorded PID prevented cleanup: %+v %v", result, err)
	}
}

func TestDAGCleanupContextCancellationDoesNotConfirmDeletion(t *testing.T) {
	o, server, exp, cache := dagFixture(t)
	target := scanOneDAG(t, o, server, exp)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	result, err := o.CleanupDAGCache(ctx, server, target)
	if err == nil || result.Status == "cleaned" || result.RemovedFiles != 0 {
		t.Fatalf("expired execution reported success: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(cache, "full-R23-0000000000000000")); err != nil {
		t.Fatal(err)
	}
}

func TestDAGCleanupReportsOnlySuccessfulUnlinksOnPartialFailure(t *testing.T) {
	o, server, exp, cache := dagFixture(t)
	remaining := filepath.Join(cache, "full-R23-1111111111111111")
	if err := os.WriteFile(remaining, []byte("keep on unlink failure"), 0600); err != nil {
		t.Fatal(err)
	}
	target := scanOneDAG(t, o, server, exp)
	realUnlink, err := exec.LookPath("unlink")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := t.TempDir()
	shim := "#!/bin/bash\nif [ \"$2\" = full-R23-1111111111111111 ]; then exit 1; fi\nexec " + shell(realUnlink) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "unlink"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+":"+os.Getenv("PATH"))
	result, err := o.CleanupDAGCache(context.Background(), server, target)
	if err == nil || result.Status != "partial" || result.RemovedFiles != 1 || result.FreedBytes <= 0 || result.FreedBytes >= target.Bytes {
		t.Fatalf("partial failure accounting: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(cache, "full-R23-0000000000000000")); !os.IsNotExist(err) {
		t.Fatalf("first DAG was not removed: %v", err)
	}
	if got := testRead(t, remaining); got != "keep on unlink failure" {
		t.Fatalf("failed unlink file changed: %q", got)
	}
}

func TestDAGCleanupRejectsDirectoryReplacedAfterScan(t *testing.T) {
	o, server, exp, cache := dagFixture(t)
	target := scanOneDAG(t, o, server, exp)
	moved := filepath.Join(t.TempDir(), "original-cache")
	if err := os.Rename(cache, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, cache); err != nil {
		t.Fatal(err)
	}
	result, err := o.CleanupDAGCache(context.Background(), server, target)
	if err == nil || result.RemovedFiles != 0 {
		t.Fatalf("replaced directory accepted: %+v %v", result, err)
	}
	if got := testRead(t, filepath.Join(moved, "full-R23-0000000000000000")); got != "cached DAG" {
		t.Fatalf("moved original cache changed: %q", got)
	}
}

func dagControlledProcScript(t *testing.T, script string) (string, string) {
	t.Helper()
	procRoot := t.TempDir()
	for _, dir := range []string{"self/fd", "123/fd"} {
		if err := os.MkdirAll(filepath.Join(procRoot, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, contents := range map[string]string{
		"self/stat":   "available",
		"123/stat":    "123 (helper) S " + strings.Repeat("0 ", 18) + "42\n",
		"123/status":  "Uid:\t" + strconv.Itoa(os.Geteuid()) + "\t" + strconv.Itoa(os.Geteuid()) + "\t0\t0\n",
		"123/cmdline": "unrelated\x00",
		"123/maps":    "",
	} {
		if err := os.WriteFile(filepath.Join(procRoot, path), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return strings.ReplaceAll(script, "/proc/", procRoot+"/"), procRoot
}

func TestDAGProcessInspectionRefusesPIDReuse(t *testing.T) {
	script, _ := dagControlledProcScript(t, dagCacheScript)
	// Each command substitution gets a different BASHPID, modeling process
	// identity reuse between the checks without racing the real PID allocator.
	script += "\ndag_proc_identity() { printf '%s' \"$BASHPID\"; }\nmode=cleanup\nroot=/unrelated/experiment\ndag_processes_inactive\n"
	o := Orchestrator{}
	out, err := o.Remote.Run(context.Background(), model.Server{Host: "local"}, "set -euo pipefail\n"+script)
	if err == nil || !strings.Contains(out, "PID was reused") {
		t.Fatalf("PID reuse was not refused: %q %v", out, err)
	}
}

func TestDAGProcessInspectionRefusesUnreadableDeploymentProcess(t *testing.T) {
	script, procRoot := dagControlledProcScript(t, dagCacheScript)
	// A directory satisfies -r, but reading it as a mapping file must fail.
	// This covers the permission/read-failure branch even when tests run as root.
	if err := os.Remove(filepath.Join(procRoot, "123/maps")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(procRoot, "123/maps"), 0700); err != nil {
		t.Fatal(err)
	}
	script += "\nmode=scan\nroot=/unrelated/experiment\ndag_processes_inactive\n"
	o := Orchestrator{}
	out, err := o.Remote.Run(context.Background(), model.Server{Host: "local"}, "set -euo pipefail\n"+script)
	if err == nil || !strings.Contains(out, "process mappings are unreadable") {
		t.Fatalf("unreadable process was not refused: %q %v", out, err)
	}
}

func TestDAGProcessInspectionAllowsUnrelatedStableProcess(t *testing.T) {
	script, _ := dagControlledProcScript(t, dagCacheScript)
	script += "\nmode=scan\nroot=/unrelated/experiment\ndag_processes_inactive\n"
	o := Orchestrator{}
	out, err := o.Remote.Run(context.Background(), model.Server{Host: "local"}, "set -euo pipefail\n"+script)
	if err != nil {
		t.Fatalf("unrelated process was refused: %q %v", out, err)
	}
}
