package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
)

const mockMiningGeth = `#!/bin/bash
set -eu
dir="${2%/geth.ipc}"
expression="$4"
printf '%s\n' "$expression" >>"$dir/mock-mining.log"
if [ -f "$dir/mock-failure" ]; then echo 'RPC error' >&2; exit 1; fi
if [ -f "$dir/mock-output" ]; then cat "$dir/mock-output"; exit 0; fi
if [[ "$expression" == *miner.start* ]]; then
    printf 'true\n' >"$dir/mock-next"
    printf 'false\n'
elif [[ "$expression" == *miner.stop* ]]; then
    printf 'false\n' >"$dir/mock-next"
    printf 'true\n'
else
    if [ -f "$dir/mock-next" ]; then cat "$dir/mock-next"; else printf 'false\n'; fi
fi
`

func TestSetMiningWaitsForObservedState(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		o, exp, node, server, dir := recoveryFixture(t, 2)
		geth := filepath.Join(server.WorkDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "bin", "geth")
		if err := os.WriteFile(geth, []byte(mockMiningGeth), 0700); err != nil {
			t.Fatal(err)
		}
		if err := o.SetMining(context.Background(), exp, node, server, enabled); err != nil {
			t.Fatalf("enabled=%t: %v", enabled, err)
		}
		log := testRead(t, filepath.Join(dir, "mock-mining.log"))
		command := "miner.stop(); eth.mining"
		if enabled {
			command = "eth.mining\nminer.setEtherbase(eth.accounts[0]); miner.start(1); eth.mining"
		}
		if log != command+"\neth.mining\n" {
			t.Fatalf("should command once then confirm asynchronous state, got %q", log)
		}
	}
}

func TestSetMiningRejectsConsoleFailureAndUnconfirmedState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		fail   bool
		want   string
	}{
		{name: "exception", output: "Error: unknown method\n", want: "did not return eth.mining"},
		{name: "no state change", output: "false\n", want: "state was not confirmed"},
		{name: "RPC failure", fail: true, want: "RPC error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, exp, node, server, dir := recoveryFixture(t, 2)
			geth := filepath.Join(server.WorkDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "bin", "geth")
			if err := os.WriteFile(geth, []byte(mockMiningGeth), 0700); err != nil {
				t.Fatal(err)
			}
			file := "mock-output"
			if tc.fail {
				file = "mock-failure"
			}
			if err := os.WriteFile(filepath.Join(dir, file), []byte(tc.output), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := o.SetMining(ctx, exp, node, server, true); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("wanted %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDeployRejectsInvalidMinerCountBeforeRuntimeAccess(t *testing.T) {
	exp := model.Experiment{MinerCount: 2, Placements: []model.Placement{{ServerID: "one", Count: 1}}, ArtifactPath: "/must-not-open"}
	if _, err := (Orchestrator{}).Deploy(context.Background(), &exp, nil, nil); err == nil || !strings.Contains(err.Error(), "miner count") {
		t.Fatalf("invalid miner count was not rejected before deployment: %v", err)
	}
}

func TestSetMiningCancellationStopsHungAttach(t *testing.T) {
	o, exp, node, server, _ := recoveryFixture(t, 2)
	geth := filepath.Join(server.WorkDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "bin", "geth")
	if err := os.WriteFile(geth, []byte("#!/bin/bash\nwhile :; do :; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := o.SetMining(ctx, exp, node, server, true); err == nil {
		t.Fatal("hung attach ignored cancellation")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("cancellation left the attach process behind")
	}
}

func TestSetMiningPreflightDoesNotBlockStopOrAlreadyActiveMiner(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			fakeDiskCapacity(t, 0, 0)
			dfLog := filepath.Join(t.TempDir(), "df.log")
			t.Setenv("PFAP_TEST_DF_LOG", dfLog)
			o, exp, node, server, dir := recoveryFixture(t, 2)
			geth := filepath.Join(server.WorkDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "bin", "geth")
			if err := os.WriteFile(geth, []byte(mockMiningGeth), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "mock-next"), []byte("true\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := o.SetMining(context.Background(), exp, node, server, enabled); err != nil {
				t.Fatalf("disk-full should not block this action: %v", err)
			}
			if _, err := os.Stat(dfLog); !os.IsNotExist(err) {
				t.Fatal("capacity was checked for an existing miner or miner.stop")
			}
			if enabled && testRead(t, filepath.Join(dir, "mock-mining.log")) != "eth.mining\n" {
				t.Fatal("an already-active miner was restarted")
			}
		})
	}
}

func TestSetMiningRejectsInsufficientSpaceBeforeStart(t *testing.T) {
	fakeDiskCapacity(t, 2<<20, 5000)
	o, exp, node, server, dir := recoveryFixture(t, 2)
	geth := filepath.Join(server.WorkDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "bin", "geth")
	if err := os.WriteFile(geth, []byte(mockMiningGeth), 0700); err != nil {
		t.Fatal(err)
	}
	// Preallocated-looking cache files must not be credited as completed DAGs.
	cacheDir := filepath.Join(filepath.Dir(dir), "ethash")
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"full-R23-0000000000000000", "full-R23-290decd9548b62a8"} {
		path := filepath.Join(cacheDir, name)
		if err := os.WriteFile(path, []byte("malformed"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, 1<<30); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.SetMining(context.Background(), exp, node, server, true); err == nil || !strings.Contains(err.Error(), "insufficient free disk space") {
		t.Fatalf("start was not rejected: %v", err)
	}
	if log := testRead(t, filepath.Join(dir, "mock-mining.log")); log != "eth.mining\n" {
		t.Fatalf("miner.start reached geth without capacity: %q", log)
	}
}

func TestSetMiningReservesOtherConfiguredMinersTemporaryDAGs(t *testing.T) {
	fakeDiskCapacity(t, 4<<20, 5000)
	o, exp, node, server, dir := recoveryFixture(t, 2)
	geth := filepath.Join(server.WorkDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "bin", "geth")
	if err := os.WriteFile(geth, []byte(mockMiningGeth), 0700); err != nil {
		t.Fatal(err)
	}
	node.IsMiner, exp.MinerCount = true, 2
	exp.Nodes = []model.Node{node, {ID: "other-miner", ServerID: server.ID, LocalIndex: 4, IsMiner: true}}
	if err := o.SetMining(context.Background(), exp, node, server, true); err == nil || !strings.Contains(err.Error(), "insufficient free disk space") {
		t.Fatalf("concurrent DAG generation was not reserved: %v", err)
	}
	if log := testRead(t, filepath.Join(dir, "mock-mining.log")); log != "eth.mining\n" {
		t.Fatalf("miner.start reached geth without concurrent DAG capacity: %q", log)
	}
}

func TestDeployConnectsNodesBeforeStartingDistributedMiners(t *testing.T) {
	t.Run("auto", func(t *testing.T) { testDeployDistributedMiners(t, false) })
	t.Run("manual", func(t *testing.T) { testDeployDistributedMiners(t, true) })
}

func testDeployDistributedMiners(t *testing.T, manual bool) {
	if _, err := exec.LookPath("ss"); err != nil {
		t.Skip("requires ss for deployment port preflight")
	}
	root := t.TempDir()
	archive := filepath.Join(root, "runtime.tar.gz")
	writeDiskRuntimeArchive(t, archive, map[string]string{"pfap-runtime/fixture": "already-cached-test-runtime"})
	sha, err := fileSHA(archive)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "deploy.log")
	servers := map[string]model.Server{}
	for i := 1; i <= 2; i++ {
		id := "server-" + strconv.Itoa(i)
		server := model.Server{ID: id, Name: id, Host: "local", HostGroup: "physical-" + id, P2PHost: "192.0.2." + strconv.Itoa(i), WorkDir: filepath.Join(root, id)}
		servers[id] = server
		runtime := filepath.Join(server.WorkDir, "artifacts", sha, "pfap-runtime")
		geth := "#!/bin/bash\nset -eu\n[ \"$1\" != version ] || exit 0\nexpression=\"$4\"\nprintf '%s:%s\\n' " + shell(id) + " \"$expression\" >>" + shell(logPath) + `
case "$expression" in
    'eth.accounts[0]') printf '"0x1111111111111111111111111111111111111111"\n' ;;
    admin.nodeInfo.enode) printf '"enode://abcdef@0.0.0.0:30303"\n' ;;
    eth.mining) printf 'false\n' ;;
    *) printf 'true\n' ;;
esac
`
		network := "#!/bin/bash\nset -eu\nprintf '%s:MINE=%s MAX_PEERS=%s\\n' " + shell(id) + " \"$MINE\" \"$MAX_PEERS\" >>" + shell(logPath) + "\n[ \"$MINE\" = false ]\n"
		for path, content := range map[string]string{filepath.Join(runtime, "bin", "geth"): geth, filepath.Join(runtime, "pow", "network.sh"): network} {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0700); err != nil {
				t.Fatal(err)
			}
		}
	}
	exp := model.Experiment{ID: "exp-miners", ArtifactPath: archive, MinerCount: 2, NetworkID: 55661, P2PPortBase: 49171, RPCPortBase: 49181, Topology: "full-mesh", Placements: []model.Placement{{ServerID: "server-1", Count: 2}, {ServerID: "server-2", Count: 1}}}
	if manual {
		exp.MinerMode = "manual"
		exp.MinerSelections = []model.MinerSelection{{ServerID: "server-1", LocalIndex: 2}, {ServerID: "server-2", LocalIndex: 1}}
	}
	var deployedTargets []model.MinerTarget
	nodes, err := (Orchestrator{}).Deploy(context.Background(), &exp, servers, func(_, kind, _ string, fields map[string]any) {
		if kind == "deploy" {
			if targets, ok := fields["selectedTargets"].([]model.MinerTarget); ok {
				deployedTargets = targets
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 || nodes[0].IsMiner == manual || nodes[1].IsMiner != manual || !nodes[2].IsMiner {
		t.Fatalf("deployment lost desired auto/manual selection: %+v", nodes)
	}
	if len(deployedTargets) != 2 || deployedTargets[0].HostGroup != "physical-server-1" || deployedTargets[1].HostGroup != "physical-server-2" {
		t.Fatalf("deployment target snapshot missing: %+v", deployedTargets)
	}
	for _, node := range nodes {
		if node.Mining == nil || *node.Mining != node.IsMiner {
			t.Errorf("unexpected deployment mining result for %s", node.Name)
		}
	}
	log := testRead(t, logPath)
	if strings.Count(log, "MINE=false") != 2 || strings.Count(log, "miner.start(1)") != 2 {
		t.Fatalf("unexpected startup/mining calls: %s", log)
	}
	if strings.Count(log, "MAX_PEERS=2") != 2 {
		t.Fatalf("deployment did not pass the global peer limit to both placements: %s", log)
	}
	if strings.LastIndex(log, "admin.addPeer") > strings.Index(log, "miner.start(1)") || !strings.Contains(log, "admin.addPeer") {
		t.Fatalf("mining started before topology was connected: %s", log)
	}
}
