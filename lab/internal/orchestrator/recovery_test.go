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

const mockRecoveryGeth = `#!/bin/bash
set -eu
if [ "${1:-}" = attach ]; then
    dir="${2%/geth.ipc}"
    expression="$4"
    printf '%s\n' "$0" >>"$dir/mock-attach-runtime.log"
    printf '%s\n' "$expression" >>"$dir/mock-attach.log"
    if [[ "$expression" == *blockNumber* ]]; then
        [ ! -f "$dir/mock-ipc-unavailable" ] && [ -f "$dir/mock-ready" ] || exit 1
        printf 'true\n'
    elif [ "$expression" = admin.nodeInfo.enode ]; then
        printf '"enode://abcdef@0.0.0.0:30303"\n'
    elif [ -f "$dir/mock-console-false" ]; then
        printf 'false\n'
    elif [[ "$expression" == *miner.start* ]]; then
        printf 'true\n' >"$dir/mock-mining-enabled"
        printf 'true\n'
    elif [ "$expression" = eth.mining ]; then
        if [ -f "$dir/mock-mining-enabled" ]; then cat "$dir/mock-mining-enabled"; else printf 'false\n'; fi
    elif [[ "$expression" == miner.stop* ]]; then
        printf 'false\n' >"$dir/mock-mining-enabled"
        printf 'false\n'
    else
        printf 'true\n'
    fi
    exit 0
fi
dir="$2"
printf 'start\n' >>"$dir/mock-starts"
if [ -f "$dir/mock-exit" ]; then printf 'malformed SN: unexpected EOF\n' >&2; exit 1; fi
if [ -f "$dir/mock-sn-error" ]; then printf 'Decode SNSbytes error: rlp: expected input string or byte for common.Hash\n' >&2; fi
touch "$dir/mock-ready"
trap 'exit 0' TERM INT
while :; do sleep 0.1; done
`

func recoveryFixture(t *testing.T, index int) (Orchestrator, model.Experiment, model.Node, model.Server, string) {
	t.Helper()
	for _, command := range []string{"bash", "setsid", "timeout", "flock"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("requires %s: %v", command, err)
		}
	}
	server := model.Server{ID: "srv-recovery-" + strconv.Itoa(index), Host: "local", WorkDir: t.TempDir()}
	exp := model.Experiment{ID: "exp-recovery", ArtifactSHA: strings.Repeat("a", 64), NetworkID: 55661, Topology: "full-mesh"}
	node := model.Node{ID: "exp-recovery-n" + strconv.Itoa(index), Name: "node-" + strconv.Itoa(index), ServerID: server.ID, Index: index, LocalIndex: 3, P2PPort: 39781, RPCPort: 39782, Account: "0x" + strings.Repeat("1", 40)}
	dir := filepath.Join(server.WorkDir, "experiments", exp.ID, server.ID, "node3")
	files := map[string]string{
		filepath.Join(server.WorkDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "bin", "geth"):             mockRecoveryGeth,
		filepath.Join(server.WorkDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "prfKey", "create_pk.txt"): "original-pk",
		filepath.Join(server.WorkDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "prfKey", "create_vk.txt"): "original-vk",
		filepath.Join(filepath.Dir(dir), "password.txt"):                                                       "existing-password\n",
		filepath.Join(dir, "address"): strings.Repeat("1", 40) + "\n",
		filepath.Join(dir, "keystore", "UTC--existing--"+strings.Repeat("1", 40)): "existing-keystore",
		filepath.Join(dir, "geth", "chaindata", "existing-data"):                  "existing-chain",
		filepath.Join(dir, "SN"): "existing-privacy-state",
	}
	for path, contents := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		b, _ := os.ReadFile(filepath.Join(dir, "geth.pid"))
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		// Only clean up a helper whose exact test-owned datadir is present.
		cmdline, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		if pid > 0 && strings.Contains(string(cmdline), "\x00"+dir+"\x00") {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	})
	return Orchestrator{}, exp, node, server, dir
}

func testRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func stageRecoveryTarget(t *testing.T, server model.Server, sha string) string {
	t.Helper()
	runtime := filepath.Join(server.WorkDir, "artifacts", sha, "pfap-runtime")
	for name, contents := range map[string]string{"bin/geth": mockRecoveryGeth, "prfKey/create_pk.txt": "original-pk", "prfKey/create_vk.txt": "original-vk"} {
		path := filepath.Join(runtime, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return runtime
}

func TestRecoverNodePreservesStateAndRepairsStalePID(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	paths := []string{"SN", "address", "keystore/UTC--existing--" + strings.Repeat("1", 40), "geth/chaindata/existing-data", "../password.txt"}
	before := map[string]string{}
	for _, path := range paths {
		before[path] = testRead(t, filepath.Join(dir, path))
	}
	// A reused PID belonging to the test process must never be signalled.
	if err := os.WriteFile(filepath.Join(dir, "geth.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	startedEvents := 0
	emit := func(_, kind, _ string, fields map[string]any) {
		if kind == "node-recovery-started" && fields["restarted"] == true {
			startedEvents++
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := o.RecoverNode(ctx, exp, node, map[string]model.Server{server.ID: server}, emit); err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(testRead(t, filepath.Join(dir, "geth.pid")))
	args := testRead(t, filepath.Join("/proc", pid, "cmdline"))
	for _, want := range []string{"--datadir\x00" + dir + "\x00", "--port\x0039781\x00", "--networkid\x0055661\x00", "/artifacts/" + exp.ArtifactSHA + "/pfap-runtime/bin/geth"} {
		if !strings.Contains(args, want) {
			t.Errorf("process args %q do not contain %q", args, want)
		}
	}
	if err := os.Remove(filepath.Join(dir, "geth.pid")); err != nil {
		t.Fatal(err)
	}
	if err := o.RecoverNode(ctx, exp, node, map[string]model.Server{server.ID: server}, emit); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(testRead(t, filepath.Join(dir, "geth.pid"))); got != pid {
		t.Errorf("existing process replaced: %s -> %s", pid, got)
	}
	if startedEvents != 1 || testRead(t, filepath.Join(dir, "mock-starts")) != "start\n" {
		t.Error("recovery restarted an already-running node")
	}
	for _, path := range paths {
		if got := testRead(t, filepath.Join(dir, path)); got != before[path] {
			t.Errorf("recovery modified %s", path)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "mock-ipc-unavailable"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := o.RecoverNode(ctx, exp, node, map[string]model.Server{server.ID: server}, nil); err == nil || !strings.Contains(err.Error(), "alive but IPC is unavailable") {
		t.Fatalf("expected live unresponsive process protection, got %v", err)
	}
	if testRead(t, filepath.Join(dir, "mock-starts")) != "start\n" {
		t.Error("unresponsive process was restarted")
	}
}

func TestRecoverNodeRefusesMissingState(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	if err := os.Rename(filepath.Join(dir, "geth", "chaindata"), filepath.Join(dir, "retained-chain")); err != nil {
		t.Fatal(err)
	}
	err := o.RecoverNode(context.Background(), exp, node, map[string]model.Server{server.ID: server}, nil)
	if err == nil || !strings.Contains(err.Error(), "Existing chain data is missing") {
		t.Fatalf("missing data was not rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "geth", "chaindata")); !os.IsNotExist(err) {
		t.Fatal("recovery initialized missing chain data")
	}
}

func TestRecoverNodeStartupFailureIncludesLog(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	if err := os.WriteFile(filepath.Join(dir, "mock-exit"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := o.RecoverNode(ctx, exp, node, map[string]model.Server{server.ID: server}, nil)
	if err == nil || !strings.Contains(err.Error(), "malformed SN: unexpected EOF") {
		t.Fatalf("startup failure log missing: %v", err)
	}
}

func TestRecoverNodeRejectsLowCapacityBeforeSpawn(t *testing.T) {
	for _, index := range []int{1, 2} {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			fakeDiskCapacity(t, 0, 5000)
			o, exp, node, server, dir := recoveryFixture(t, index)
			err := o.RecoverNode(context.Background(), exp, node, map[string]model.Server{server.ID: server}, nil)
			if err == nil || !strings.Contains(err.Error(), "insufficient free disk space") {
				t.Fatalf("capacity failure was not explicit: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "mock-starts")); !os.IsNotExist(err) {
				t.Fatal("low capacity did not prevent spawning geth")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "ethash")); !os.IsNotExist(err) {
				t.Fatal("capacity preflight ran after DAG directory creation")
			}
		})
	}
}

func TestRecoverNodePIDHandoffDiskFullFailsImmediately(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("requires /dev/full to simulate ENOSPC")
	}
	bin := fakeDiskCapacity(t, 4<<20, 5000)
	o, exp, node, server, dir := recoveryFixture(t, 2)
	handoff := filepath.Join(dir, "test-pid-handoff")
	if err := os.Symlink("/dev/full", handoff); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "mktemp"), []byte("#!/bin/bash\nprintf '%s\\n' "+shell(handoff)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	started := time.Now()
	err := o.RecoverNode(ctx, exp, node, map[string]model.Server{server.ID: server}, nil)
	if err == nil || !strings.Contains(err.Error(), "Node launcher failed") || !strings.Contains(err.Error(), "No space left on device") {
		t.Fatalf("PID handoff failure was not explicit: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("PID write failure waited for IPC timeout")
	}
	if _, err := os.Stat(filepath.Join(dir, "mock-starts")); !os.IsNotExist(err) {
		t.Fatal("geth executed after its PID handoff failed")
	}
}

func TestRecoverNodeReservesConcurrentMinersBeforeSpawn(t *testing.T) {
	fakeDiskCapacity(t, 4<<20, 5000)
	o, exp, node, server, dir := recoveryFixture(t, 2)
	node.IsMiner, exp.MinerCount = true, 2
	exp.Nodes = []model.Node{node, {ID: "other-miner", ServerID: server.ID, LocalIndex: 4, IsMiner: true}}
	err := o.RecoverNode(context.Background(), exp, node, map[string]model.Server{server.ID: server}, nil)
	if err == nil || !strings.Contains(err.Error(), "insufficient free disk space") {
		t.Fatalf("concurrent DAG generation was not reserved before recovery: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mock-starts")); !os.IsNotExist(err) {
		t.Fatal("recovery spawned geth without concurrent DAG capacity")
	}
}

func TestRecoverNodeRestoresMiningAndChecksConsoleResult(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := o.RecoverNode(ctx, exp, node, map[string]model.Server{server.ID: server}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(testRead(t, filepath.Join(dir, "mock-attach.log")), "miner.start(1); eth.mining") {
		t.Error("mining was not restored")
	}
	if err := os.WriteFile(filepath.Join(dir, "mock-console-false"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := o.RecoverNode(ctx, exp, node, map[string]model.Server{server.ID: server}, nil); err == nil || !strings.Contains(err.Error(), "mining could not be restored") {
		t.Fatalf("false JS result was accepted: %v", err)
	}
}

func TestRecoverNodeUsesConfiguredMinerRole(t *testing.T) {
	for _, tc := range []struct {
		index int
		miner bool
	}{{index: 2, miner: true}, {index: 1, miner: false}} {
		o, exp, node, server, dir := recoveryFixture(t, tc.index)
		exp.MinerCount = 1
		node.IsMiner = tc.miner
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := o.RecoverNode(ctx, exp, node, map[string]model.Server{server.ID: server}, nil); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		startedMining := strings.Contains(testRead(t, filepath.Join(dir, "mock-attach.log")), "miner.start(1)")
		if startedMining != tc.miner {
			t.Errorf("node index=%d configured miner=%t: mining started=%t", tc.index, tc.miner, startedMining)
		}
	}
}

func TestRecoverNodeRejectsOwnedDatadirLock(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	f, err := os.OpenFile(filepath.Join(dir, "geth", "chaindata", "LOCK"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = o.RecoverNode(context.Background(), exp, node, map[string]model.Server{server.ID: server}, nil)
	if err == nil || !strings.Contains(err.Error(), "Datadir lock has a live owner") {
		t.Fatalf("live lock owner was not rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mock-starts")); !os.IsNotExist(err) {
		t.Fatal("recovery launched into an owned datadir")
	}
}

func TestRecoverNodeReconnectsBothDirections(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 1)
	_, _, peer, peerServer, peerDir := recoveryFixture(t, 2)
	server.P2PHost = "192.0.2.1"
	peerServer.P2PHost = "192.0.2.2"
	servers := map[string]model.Server{server.ID: server, peerServer.ID: peerServer}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := o.RecoverNode(ctx, exp, peer, servers, nil); err != nil {
		t.Fatal(err)
	}
	exp.Nodes = []model.Node{node, peer}
	if err := o.RecoverNode(ctx, exp, node, servers, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(testRead(t, filepath.Join(dir, "mock-attach.log")), `admin.addPeer("enode://abcdef@192.0.2.2:39781")`) {
		t.Error("recovered node was not connected to its peer")
	}
	if !strings.Contains(testRead(t, filepath.Join(peerDir, "mock-attach.log")), `admin.addPeer("enode://abcdef@192.0.2.1:39781")`) {
		t.Error("peer was not connected back to recovered node")
	}
	if err := os.WriteFile(filepath.Join(peerDir, "mock-console-false"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := o.RecoverNode(ctx, exp, node, servers, nil); err == nil || !strings.Contains(err.Error(), "reconnect node-2 to node-1 failed") {
		t.Fatalf("addPeer false result was accepted: %v", err)
	}
}

func TestRecoverNodeHotfixOnlyAppliesWhenProcessRestarts(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	servers := map[string]model.Server{server.ID: server}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := o.RecoverNode(ctx, exp, node, servers, nil); err != nil {
		t.Fatal(err)
	}
	oldPID := strings.TrimSpace(testRead(t, filepath.Join(dir, "geth.pid")))
	originalSHA := exp.ArtifactSHA
	targetSHA := strings.Repeat("b", 64)
	targetGeth := filepath.Join(stageRecoveryTarget(t, server, targetSHA), "bin", "geth")
	exp.RecoveryArtifactSHA = targetSHA
	startedSHAs := []string{}
	existingSHAs := []string{}
	emit := func(_, kind, _ string, fields map[string]any) {
		if kind == "node-recovery-started" {
			startedSHAs = append(startedSHAs, fields["runtimeSha"].(string))
		} else if kind == "node-recovery-existing" {
			if fields["restarted"] != false {
				t.Error("existing process was reported as restarted")
			}
			existingSHAs = append(existingSHAs, fields["runtimeSha"].(string))
		}
	}
	if err := o.RecoverNode(ctx, exp, node, servers, emit); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(testRead(t, filepath.Join(dir, "geth.pid"))); got != oldPID || len(startedSHAs) != 0 {
		t.Fatal("configuring a hotfix restarted a healthy original process")
	}
	if strings.Contains(testRead(t, filepath.Join(dir, "mock-attach-runtime.log")), targetSHA) {
		t.Fatal("healthy original node was attached with target runtime")
	}
	if len(existingSHAs) != 1 || existingSHAs[0] != originalSHA {
		t.Fatalf("existing original runtime was not reported: %v", existingSHAs)
	}
	pid, _ := strconv.Atoi(oldPID)
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 50; attempt++ {
		cmdline, _ := os.ReadFile(filepath.Join("/proc", oldPID, "cmdline"))
		if len(cmdline) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Historical failure lines must not poison the repaired runtime's startup.
	log, err := os.OpenFile(filepath.Join(dir, "geth.log"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = log.WriteString("Decode SNSbytes error: historic decoder failure\n")
	_ = log.Close()
	if err := o.RecoverNode(ctx, exp, node, servers, emit); err != nil {
		t.Fatal(err)
	}
	newPID := strings.TrimSpace(testRead(t, filepath.Join(dir, "geth.pid")))
	if newPID == oldPID || len(startedSHAs) != 1 || startedSHAs[0] != targetSHA {
		t.Fatalf("restart runtime was not reported: pid=%s, SHAs=%v", newPID, startedSHAs)
	}
	if args := testRead(t, filepath.Join("/proc", newPID, "cmdline")); !strings.Contains(args, targetGeth) {
		t.Fatalf("crashed node was not started using target runtime: %q", args)
	}
	if exp.ArtifactSHA != originalSHA {
		t.Fatal("original experiment runtime SHA was mutated")
	}
	// Simulate a controller crash after launch but before saving RuntimeSHA.
	// Retrying recovery must discover and report the already-running target.
	if err := o.RecoverNode(ctx, exp, node, servers, emit); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(testRead(t, filepath.Join(dir, "geth.pid"))); got != newPID {
		t.Fatal("adopting the hotfix process changed its PID")
	}
	if len(existingSHAs) != 2 || existingSHAs[1] != targetSHA || len(startedSHAs) != 1 {
		t.Fatalf("actual hotfix runtime was not reported on adoption: existing=%v started=%v", existingSHAs, startedSHAs)
	}
	node.RuntimeSHA = targetSHA
	exp.RecoveryArtifactSHA = ""
	if err := o.RecoverNode(ctx, exp, node, servers, emit); err != nil {
		t.Fatal(err)
	}
	if out, err := o.Attach(ctx, exp, node, server, "typeof eth.blockNumber === \"number\""); err != nil || !consoleTrue(out) {
		t.Fatalf("attach with persisted actual runtime: %q %v", out, err)
	}
	attachLines := strings.Split(strings.TrimSpace(testRead(t, filepath.Join(dir, "mock-attach-runtime.log"))), "\n")
	if got := attachLines[len(attachLines)-1]; got != targetGeth {
		t.Fatalf("Attach used %q, want actual runtime %q", got, targetGeth)
	}
}

func TestRecoverNodeRejectsInvalidHotfixSHA(t *testing.T) {
	_, exp, node, server, _ := recoveryFixture(t, 2)
	exp.RecoveryArtifactSHA = "../untrusted"
	if _, err := recoveryScript(exp, node, server); err == nil {
		t.Fatal("invalid target runtime SHA accepted")
	}
	exp.RecoveryArtifactSHA = ""
	node.RuntimeSHA = "../untrusted"
	if _, err := recoveryScript(exp, node, server); err == nil {
		t.Fatal("invalid actual runtime SHA accepted")
	}
}

func TestRecoverNodeRejectsDifferentHotfixKeys(t *testing.T) {
	for _, change := range []string{"contents", "names"} {
		t.Run(change, func(t *testing.T) {
			o, exp, node, server, dir := recoveryFixture(t, 2)
			exp.RecoveryArtifactSHA = strings.Repeat("b", 64)
			targetRuntime := stageRecoveryTarget(t, server, exp.RecoveryArtifactSHA)
			pk := filepath.Join(targetRuntime, "prfKey", "create_pk.txt")
			if change == "contents" {
				if err := os.WriteFile(pk, []byte("rotated-pk"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Rename(pk, filepath.Join(targetRuntime, "prfKey", "other_pk.txt")); err != nil {
				t.Fatal(err)
			}
			err := o.RecoverNode(context.Background(), exp, node, map[string]model.Server{server.ID: server}, nil)
			if err == nil || !strings.Contains(err.Error(), "Recovery runtime proving keys do not match") {
				t.Fatalf("mismatched target keys accepted: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "mock-starts")); !os.IsNotExist(err) {
				t.Fatal("recovery launched a process with changed proving keys")
			}
		})
	}
}

func TestRecoverNodeRejectsLiveProcessWithNewSNDecoderError(t *testing.T) {
	o, exp, node, server, dir := recoveryFixture(t, 2)
	if err := os.WriteFile(filepath.Join(dir, "mock-sn-error"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	reportedSHA := ""
	err := o.RecoverNode(context.Background(), exp, node, map[string]model.Server{server.ID: server}, func(_, kind, _ string, fields map[string]any) {
		if kind == "node-recovery-started" {
			reportedSHA = fields["runtimeSha"].(string)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "SN restore failed during this startup") || !strings.Contains(err.Error(), "Decode SNSbytes error") {
		t.Fatalf("live process with decoder failure reported ready: %v", err)
	}
	if reportedSHA != exp.ArtifactSHA {
		t.Errorf("running runtime was not reported on partial failure: %q", reportedSHA)
	}
	if got := testRead(t, filepath.Join(dir, "SN")); got != "existing-privacy-state" {
		t.Fatal("SN changed after decoder failure")
	}
}

func TestRecoveryEnodeAddresses(t *testing.T) {
	node := model.Node{Name: "node-1", P2PPort: 30001}
	for _, tc := range []struct {
		name, raw          string
		owner, destination model.Server
		want               string
	}{
		{"same host", "enode://abc@[::]:30303", model.Server{ID: "a", Host: "local"}, model.Server{ID: "a"}, "enode://abc@127.0.0.1:30001"},
		{"explicit p2p", "enode://abc@0.0.0.0:30303", model.Server{ID: "a", Host: "local", P2PHost: "192.0.2.10"}, model.Server{ID: "b"}, "enode://abc@192.0.2.10:30001"},
		{"advertised host", "enode://abc@192.0.2.11:30303", model.Server{ID: "a", Host: "local"}, model.Server{ID: "b"}, "enode://abc@192.0.2.11:30001"},
		{"ipv6", "enode://abc@[::]:30303", model.Server{ID: "a", Host: "2001:db8::2"}, model.Server{ID: "b"}, "enode://abc@[2001:db8::2]:30001"},
		{"unroutable local", "enode://abc@0.0.0.0:30303", model.Server{ID: "a", Host: "local"}, model.Server{ID: "b"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := recoveryEnode(tc.raw, node, tc.owner, tc.destination)
			if tc.want == "" {
				if err == nil {
					t.Fatal("unroutable peer accepted")
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
