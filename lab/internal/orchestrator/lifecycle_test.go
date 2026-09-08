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

func TestStopNodesDiscoversMissingPIDAndPreservesChain(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	servers := map[string]model.Server{server.ID: server}
	if err := o.RecoverNode(context.Background(), exp, node, servers, nil); err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(testRead(t, filepath.Join(dir, "geth.pid")))
	if err := os.Remove(filepath.Join(dir, "geth.pid")); err != nil {
		t.Fatal(err)
	}
	exp.Nodes = []model.Node{node}
	results, err := o.StopNodes(context.Background(), exp, servers, nil)
	if err != nil || len(results) != 1 || results[0].Status != "stopped" {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	if cmdline, _ := os.ReadFile(filepath.Join("/proc", pid, "cmdline")); len(cmdline) != 0 {
		t.Fatalf("owned process survived stop: %q", cmdline)
	}
	for path, want := range map[string]string{"SN": "existing-privacy-state", "geth/chaindata/existing-data": "existing-chain", "address": strings.Repeat("1", 40) + "\n"} {
		if got := testRead(t, filepath.Join(dir, path)); got != want {
			t.Fatalf("stop changed %s: %q", path, got)
		}
	}
}

func TestStopNodesUntrustedPIDNeverSignalsAndStillTriesLaterNodes(t *testing.T) {
	for _, pid := range []string{"-1", "0", "1; touch should-not-exist", strconv.Itoa(os.Getpid()), ""} {
		t.Run(pid, func(t *testing.T) {
			o, exp, node, server, dir := recoveryFixture(t, 2)
			if pid != "" {
				if err := os.WriteFile(filepath.Join(dir, "geth.pid"), []byte(pid), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, _, later, laterServer, laterDir := recoveryFixture(t, 3)
			servers := map[string]model.Server{server.ID: server, laterServer.ID: laterServer}
			if err := o.RecoverNode(context.Background(), exp, later, servers, nil); err != nil {
				t.Fatal(err)
			}
			exp.Nodes = []model.Node{node, later}
			results, err := o.StopNodes(context.Background(), exp, servers, nil)
			if err == nil || len(results) != 2 || results[0].Status != "unknown" || results[0].Error == "" || results[1].Status != "stopped" {
				t.Fatalf("results=%+v err=%v", results, err)
			}
			laterPID := strings.TrimSpace(testRead(t, filepath.Join(laterDir, "geth.pid")))
			if cmdline, _ := os.ReadFile(filepath.Join("/proc", laterPID, "cmdline")); len(cmdline) != 0 {
				t.Fatal("later node was not stopped after first node failed")
			}
		})
	}
}

func TestStopNodesObservesExitBetweenIdentityAndOwnershipReads(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	geth := filepath.Join(server.WorkDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "bin", "geth")
	// Hold TERM until the stop loop has read the live process identity.
	helper := strings.Replace(mockRecoveryGeth, "trap 'exit 0' TERM INT", `trap 'while [ ! -f "$dir/mock-exit-release" ]; do sleep 0.01; done; exit 0' TERM INT`, 1)
	if err := os.WriteFile(geth, []byte(helper), 0700); err != nil {
		t.Fatal(err)
	}
	if err := o.RecoverNode(context.Background(), exp, node, map[string]model.Server{server.ID: server}, nil); err != nil {
		t.Fatal(err)
	}
	script, err := stopNodeScript(exp, node, server)
	if err != nil {
		t.Fatal(err)
	}
	boundary := `    [ "$current_identity" = "$identity" ] || break
`
	if strings.Count(script, boundary) != 1 {
		t.Fatal("stop identity boundary changed")
	}
	script = strings.Replace(script, boundary, boundary+`
    touch "$dir/mock-exit-release"
    exit_deadline=$((SECONDS + 3))
    while [ "$(process_identity "$pid" || true)" = "$identity" ]; do
        [ "$SECONDS" -lt "$exit_deadline" ] || fail "Test process did not exit at the ownership-read boundary"
        sleep 0.01
    done
`, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := o.Remote.Run(ctx, server, script)
	if err != nil || !strings.Contains(out, "stop=stopped") {
		t.Fatalf("normal exit was mistaken for changed ownership: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mock-exit-release")); err != nil {
		t.Fatal("test did not reach the identity/ownership boundary")
	}
}

func TestStopNodesStillRejectsLiveOwnershipChangeAfterTERM(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	geth := filepath.Join(server.WorkDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "bin", "geth")
	helper := strings.Replace(mockRecoveryGeth, "trap 'exit 0' TERM INT", `trap 'exec -a "test-ownership-$dir" sleep 30' TERM INT`, 1)
	if err := os.WriteFile(geth, []byte(helper), 0700); err != nil {
		t.Fatal(err)
	}
	if err := o.RecoverNode(context.Background(), exp, node, map[string]model.Server{server.ID: server}, nil); err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(testRead(t, filepath.Join(dir, "geth.pid")))
	t.Cleanup(func() {
		args, _ := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
		if strings.HasPrefix(string(args), "test-ownership-"+dir+"\x00") {
			number, _ := strconv.Atoi(pid)
			_ = syscall.Kill(number, syscall.SIGTERM)
		}
	})
	exp.Nodes = []model.Node{node}
	results, err := o.StopNodes(context.Background(), exp, map[string]model.Server{server.ID: server}, nil)
	if err == nil || len(results) != 1 || results[0].Status != "unknown" || !strings.Contains(err.Error(), "Process ownership changed while waiting for exit") {
		t.Fatalf("live changed process was mistaken for exit: %+v %v", results, err)
	}
	args := testRead(t, filepath.Join("/proc", pid, "cmdline"))
	if !strings.HasPrefix(args, "test-ownership-"+dir+"\x00") {
		t.Fatalf("replacement process did not remain alive: %q", args)
	}
}

func TestStopNodesRejectsDifferentRuntimeOwningDatadir(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	otherRuntime := stageRecoveryTarget(t, server, strings.Repeat("c", 64))
	cmd := exec.Command(filepath.Join(otherRuntime, "bin", "geth"), "--datadir", dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if err := os.WriteFile(filepath.Join(dir, "geth.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	exp.Nodes = []model.Node{node}
	results, err := o.StopNodes(context.Background(), exp, map[string]model.Server{server.ID: server}, nil)
	if err == nil || len(results) != 1 || results[0].Status != "unknown" || !strings.Contains(err.Error(), "Another runtime") {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("foreign runtime process was signalled")
	}
}

func TestStopNodesRejectsSpoofedRuntimeArgv(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	geth := filepath.Join(server.WorkDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "bin", "geth")
	cmd := exec.Command("bash", "-c", `exec -a "$1" bash -c 'while :; do sleep 0.1; done' spoof --datadir "$2"`, "test-spoof", geth, dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pid := strconv.Itoa(cmd.Process.Pid)
	deadline := time.Now().Add(3 * time.Second)
	for {
		cmdline, _ := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
		if strings.HasPrefix(string(cmdline), geth+"\x00") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("spoof process never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(dir, "geth.pid"), []byte(pid), 0600); err != nil {
		t.Fatal(err)
	}
	exp.Nodes = []model.Node{node}
	results, err := o.StopNodes(context.Background(), exp, map[string]model.Server{server.ID: server}, nil)
	if err == nil || len(results) != 1 || results[0].Status != "unknown" || !strings.Contains(err.Error(), "Another runtime") {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("spoofed runtime process was signalled")
	}
}

func TestStopNodesVerifiesNativeExecutableOwnership(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	geth := filepath.Join(server.WorkDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "bin", "geth")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	// A native executable under the immutable runtime path exercises the
	// same argv[0]/proc-exe ownership check used by the real geth binary.
	if err := os.Remove(geth); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bash, geth); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(geth, "-c", `trap 'exit 0' TERM; : >"$1/native-ready"; while :; do sleep 0.1; done`, "native-fixture", dir, "--datadir", dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "native-ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native executable fixture never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	exp.Nodes = []model.Node{node}
	results, err := o.StopNodes(context.Background(), exp, map[string]model.Server{server.ID: server}, nil)
	if err != nil || len(results) != 1 || results[0].Status != "stopped" {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("native process did not shut down gracefully: %v", err)
	}
}

func TestStopNodesMissingServerDoesNotSkipReachableNode(t *testing.T) {
	o, exp, node, server, _ := recoveryFixture(t, 2)
	servers := map[string]model.Server{server.ID: server}
	if err := o.RecoverNode(context.Background(), exp, node, servers, nil); err != nil {
		t.Fatal(err)
	}
	missing := node
	missing.ID, missing.ServerID = "missing-node", "missing-server"
	exp.Nodes = []model.Node{missing, node}
	results, err := o.StopNodes(context.Background(), exp, servers, nil)
	if err == nil || len(results) != 2 || results[0].Status != "unknown" || results[1].Status != "stopped" {
		t.Fatalf("results=%+v err=%v", results, err)
	}
}

func TestStopNodesVerifiesAbsentDatadirWithoutHidingAccessFailures(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		prepare      func(t *testing.T, workdir string)
	}{
		{name: "deployment never made experiment directory", status: "stopped"},
		{name: "interrupted account creation before initialization", status: "stopped", prepare: func(t *testing.T, workdir string) {
			if err := os.MkdirAll(filepath.Join(workdir, "experiments", "partial", "worker", "node1"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "initialized directory without PID stays uncertain", status: "unknown", prepare: func(t *testing.T, workdir string) {
			if err := os.MkdirAll(filepath.Join(workdir, "experiments", "partial", "worker", "node1", "geth"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "absent work directory with inspectable parent and no owned process", status: "stopped", prepare: func(t *testing.T, workdir string) {
			if err := os.Rename(workdir, workdir+"-retained"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Rename(workdir+"-retained", workdir) })
		}},
		{name: "dangling ancestry link", status: "unknown", prepare: func(t *testing.T, workdir string) {
			if err := os.Symlink(filepath.Join(workdir, "unavailable-mount"), filepath.Join(workdir, "experiments")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "inaccessible ancestry", status: "unknown", prepare: func(t *testing.T, workdir string) {
			if os.Geteuid() == 0 {
				t.Skip("root bypasses directory permissions")
			}
			path := filepath.Join(workdir, "experiments")
			if err := os.Mkdir(path, 0000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, 0700) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := model.Server{ID: "worker", Host: "local", WorkDir: t.TempDir()}
			if tc.prepare != nil {
				tc.prepare(t, server.WorkDir)
			}
			exp := model.Experiment{ID: "partial", ArtifactSHA: strings.Repeat("a", 64), Placements: []model.Placement{{ServerID: server.ID, Count: 1}}}
			results, err := (Orchestrator{}).StopNodes(context.Background(), exp, map[string]model.Server{server.ID: server}, nil)
			if len(results) != 1 || results[0].Status != tc.status || (err != nil) != (tc.status == "unknown") {
				t.Fatalf("results=%+v err=%v", results, err)
			}
		})
	}
}

func TestStopNodeWaitIsBoundedAndDoesNotEscalate(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	geth := filepath.Join(server.WorkDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "bin", "geth")
	if err := os.WriteFile(geth, []byte(strings.Replace(mockRecoveryGeth, "trap 'exit 0' TERM INT", "trap '' TERM INT", 1)), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(geth, "--datadir", dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "mock-ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake process never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	script, err := stopNodeScript(exp, node, server)
	if err != nil {
		t.Fatal(err)
	}
	// Shorten only the test's generated wait; production uses the same bounded
	// verification loop with a 20 second limit.
	script = strings.Replace(script, "SECONDS + 20", "SECONDS + 1", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := o.Remote.Run(ctx, server, script)
	if err == nil || !strings.Contains(out, "did not exit") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("stop escalated beyond TERM")
	}
}

func TestResumePreservesAccountStateAndUsesPinnedRuntime(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 1)
	exp.ArtifactPath = filepath.Join(t.TempDir(), "mutable-archive-does-not-exist.tar.gz")
	exp.RecoveryArtifactSHA = strings.Repeat("b", 64)
	stageRecoveryTarget(t, server, exp.RecoveryArtifactSHA)
	node.Account = "" // Controller died before saving the initial account.
	exp.Nodes = []model.Node{node}
	nodes, err := o.Resume(context.Background(), &exp, map[string]model.Server{server.ID: server}, nil)
	if err != nil || len(nodes) != 1 || nodes[0].Status != "running" {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	got := nodes[0]
	if got.ID != node.ID || got.LocalIndex != node.LocalIndex || got.Account != "0x"+strings.Repeat("1", 40) || got.RuntimeSHA != exp.RecoveryArtifactSHA || exp.ArtifactSHA != strings.Repeat("a", 64) || got.Mining == nil || !*got.Mining {
		t.Fatalf("identity, runtime or mining not restored: %+v", got)
	}
	if testRead(t, filepath.Join(dir, "SN")) != "existing-privacy-state" || testRead(t, filepath.Join(dir, "geth", "chaindata", "existing-data")) != "existing-chain" {
		t.Fatal("resume changed original state")
	}
	nodes, err = o.Resume(context.Background(), &exp, map[string]model.Server{server.ID: server}, nil)
	if err != nil || nodes[0].RuntimeSHA != exp.RecoveryArtifactSHA || testRead(t, filepath.Join(dir, "mock-starts")) != "start\n" {
		t.Fatalf("second resume changed actual runtime or relaunched process: nodes=%+v err=%v", nodes, err)
	}
}

func TestResumePreservesExplicitNonFirstMinerAfterHostMetadataChange(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	exp.MinerMode, exp.MinerCount = "manual", 1
	exp.MinerSelections = []model.MinerSelection{{ServerID: server.ID, LocalIndex: node.LocalIndex}}
	node.IsMiner = true
	exp.Nodes = []model.Node{node}
	server.HostGroup = "metadata-changed-after-selection"
	nodes, err := o.Resume(context.Background(), &exp, map[string]model.Server{server.ID: server}, nil)
	if err != nil || len(nodes) != 1 || !nodes[0].IsMiner || nodes[0].Mining == nil || !*nodes[0].Mining {
		t.Fatalf("manual role not restored: %+v %v", nodes, err)
	}
	if !strings.Contains(testRead(t, filepath.Join(dir, "mock-attach.log")), "miner.start(1)") {
		t.Fatal("selected non-first miner was not restarted")
	}
	if exp.MinerMode != "manual" || len(exp.MinerSelections) != 1 || exp.MinerSelections[0].LocalIndex != node.LocalIndex {
		t.Fatal("resume replaced explicit configuration")
	}
}

func TestResumeTriesAllNodesAndRestoresNonMinerRole(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	_, _, broken, brokenServer, brokenDir := recoveryFixture(t, 3)
	exp.MinerCount = 1
	node.IsMiner = false
	broken.IsMiner = true
	if err := os.Rename(filepath.Join(brokenDir, "geth", "chaindata"), filepath.Join(brokenDir, "retained-chain")); err != nil {
		t.Fatal(err)
	}
	exp.Nodes = []model.Node{broken, node}
	nodes, err := o.Resume(context.Background(), &exp, map[string]model.Server{server.ID: server, brokenServer.ID: brokenServer}, nil)
	if err == nil || len(nodes) != 2 || nodes[0].Status != "unreachable" || nodes[0].RecoveryError == "" || nodes[1].Status != "running" || nodes[1].Mining == nil || *nodes[1].Mining {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	if !strings.Contains(testRead(t, filepath.Join(dir, "mock-attach.log")), "miner.stop(); eth.mining") {
		t.Fatal("non-miner role was not explicitly restored")
	}
	if _, err := os.Stat(filepath.Join(brokenDir, "geth", "chaindata")); !os.IsNotExist(err) {
		t.Fatal("missing original chain was initialized")
	}
}

func TestResumeReportsActualRuntimeAfterPartialStartupFailure(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	exp.RecoveryArtifactSHA = strings.Repeat("b", 64)
	stageRecoveryTarget(t, server, exp.RecoveryArtifactSHA)
	if err := os.WriteFile(filepath.Join(dir, "mock-sn-error"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	exp.Nodes = []model.Node{node}
	nodes, err := o.Resume(context.Background(), &exp, map[string]model.Server{server.ID: server}, nil)
	if err == nil || len(nodes) != 1 || nodes[0].Status != "unreachable" || nodes[0].RuntimeSHA != exp.RecoveryArtifactSHA || !strings.Contains(nodes[0].RecoveryError, "SN restore failed") {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
}

func TestResumeReconnectsAfterAllProcessesStart(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 1)
	_, _, peer, peerServer, peerDir := recoveryFixture(t, 2)
	server.P2PHost, peerServer.P2PHost = "192.0.2.1", "192.0.2.2"
	exp.Nodes = []model.Node{node, peer}
	nodes, err := o.Resume(context.Background(), &exp, map[string]model.Server{server.ID: server, peerServer.ID: peerServer}, nil)
	if err != nil || nodes[0].Status != "running" || nodes[1].Status != "running" {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	for _, path := range []string{dir, peerDir} {
		if !strings.Contains(testRead(t, filepath.Join(path, "mock-attach.log")), "admin.addPeer(") {
			t.Fatal("topology was not restored for both nodes")
		}
	}
}

func TestLifecycleLegacyNodeManifest(t *testing.T) {
	exp := model.Experiment{ID: "legacy", ArtifactSHA: strings.Repeat("a", 64), P2PPortBase: 31000, RPCPortBase: 32000, Placements: []model.Placement{{ServerID: "a", Count: 2}, {ServerID: "b", Count: 1}}}
	nodes, err := lifecycleNodes(exp)
	if err != nil || len(nodes) != 3 || nodes[0].ID != "legacy-n1" || nodes[1].LocalIndex != 2 || nodes[2].LocalIndex != 1 || nodes[2].Index != 3 || nodes[2].P2PPort != 31002 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
}

func TestDeployRejectsChangedPinnedArchiveBeforeRemoteChanges(t *testing.T) {
	artifact := filepath.Join(t.TempDir(), "runtime.tar.gz")
	if err := os.WriteFile(artifact, []byte("changed archive"), 0600); err != nil {
		t.Fatal(err)
	}
	server := model.Server{ID: "local-test", Host: "local", WorkDir: filepath.Join(t.TempDir(), "untouched-worker")}
	exp := model.Experiment{ArtifactPath: artifact, ArtifactSHA: strings.Repeat("a", 64), Placements: []model.Placement{{ServerID: server.ID, Count: 1}}}
	_, err := (Orchestrator{}).Deploy(context.Background(), &exp, map[string]model.Server{server.ID: server}, nil)
	if err == nil || !strings.Contains(err.Error(), "runtime archive changed") {
		t.Fatalf("changed archive was not rejected: %v", err)
	}
	if _, err := os.Stat(server.WorkDir); !os.IsNotExist(err) {
		t.Fatal("remote directory was changed before validating pinned runtime")
	}
	if exp.ArtifactSHA != strings.Repeat("a", 64) {
		t.Fatal("pinned runtime SHA was changed")
	}
}
