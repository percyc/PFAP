package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pfap/lab/internal/model"
)

func TestExperimentMaxPeersUsesWholeDeployment(t *testing.T) {
	for _, tc := range []struct {
		name     string
		topology string
		planned  int
		saved    int
		want     int
	}{
		{"hundred", "full-mesh", 100, 0, 99},
		{"three-hundred", "full-mesh", 300, 0, 299},
		{"partial-recovery", "full-mesh", 100, 88, 99},
		{"legacy-inventory", "full-mesh", 0, 100, 99},
		{"single-node", "full-mesh", 1, 1, 1},
		{"missing-legacy-inventory", "full-mesh", 0, 0, 25},
		{"other-topology", "manual", 300, 300, 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exp := model.Experiment{Topology: tc.topology, Placements: []model.Placement{{Count: tc.planned}}, Nodes: make([]model.Node, tc.saved)}
			if got := experimentMaxPeers(exp); got != tc.want {
				t.Fatalf("peer limit=%d want=%d", got, tc.want)
			}
		})
	}
}

func TestRecoveredProcessReceivesDeploymentPeerLimit(t *testing.T) {
	for _, tc := range []struct {
		topology string
		count    int
		want     int
	}{{"full-mesh", 100, 99}, {"full-mesh", 300, 299}, {"manual", 300, 25}} {
		t.Run(tc.topology+strconv.Itoa(tc.count), func(t *testing.T) {
			o, exp, node, server, dir := recoveryFixture(t, 2)
			exp.Topology = tc.topology
			exp.Placements = []model.Placement{{ServerID: server.ID, Count: tc.count}}
			if _, err := o.recoverNodeProcess(context.Background(), exp, node, server, func(string, string, string, map[string]any) {}); err != nil {
				t.Fatal(err)
			}
			pid := strings.TrimSpace(testRead(t, filepath.Join(dir, "geth.pid")))
			args := testRead(t, filepath.Join("/proc", pid, "cmdline"))
			if !strings.Contains(args, "--maxpeers\x00"+strconv.Itoa(tc.want)+"\x00") {
				t.Fatalf("restarted process has wrong peer capacity: %q", args)
			}
			exp.Nodes = []model.Node{node}
			if _, err := o.StopNodes(context.Background(), exp, map[string]model.Server{server.ID: server}, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Exercise the real launcher's validation and launch argument construction,
// replacing only process/IPC effects with test-local shell functions.
func TestNetworkLauncherPeerLimitValidationAndArguments(t *testing.T) {
	path, err := filepath.Abs("../../../test/pow/network.sh")
	if err != nil {
		t.Fatal(err)
	}
	source := testRead(t, path)
	definitions, _, found := strings.Cut(source, "\ncommand=\"${1:-status}\"")
	if !found {
		t.Fatal("network launcher dispatcher changed")
	}
	harness := `
init_network() {
    validate_config
    mkdir -p "$NETWORK_ROOT/node1"
    printf '%s\n' 1111111111111111111111111111111111111111 >"$NETWORK_ROOT/node1/address"
}
is_running() { return 1; }
datadir_locked() { return 1; }
wait_for_process() { return 0; }
wait_for_ipc() { return 0; }
connect_network() { return 0; }
status_network() { wait; }
nohup() { printf '%s\n' "$@" >"$NETWORK_ROOT/launch.args"; }
start_network
`
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"", "25"}, {"1", "1"}, {"99", "99"}, {"299", "299"}, {"65535", "65535"},
		{"0", ""}, {"-1", ""}, {"08", ""}, {"65536", ""}, {"999999999999999999999", ""}, {"1;touch injected", ""},
	} {
		t.Run("max-"+tc.value, func(t *testing.T) {
			root := t.TempDir()
			command := exec.Command("bash", "-c", definitions+harness, path)
			command.Env = append(os.Environ(), "MAX_PEERS="+tc.value, "NODE_COUNT=1", "RUNTIME_DIR="+root, "GETH_BIN=true", "MINE=false", "OFFLINE=false")
			out, err := command.CombinedOutput()
			if tc.want == "" {
				if err == nil || !strings.Contains(string(out), "MAX_PEERS must be an integer between 1 and 65535") {
					t.Fatalf("invalid capacity accepted: %q %v", out, err)
				}
				if _, err := os.Stat(filepath.Join(root, "launch.args")); !os.IsNotExist(err) {
					t.Fatal("invalid configuration reached process launch")
				}
				return
			}
			if err != nil {
				t.Fatalf("launcher failed: %q %v", out, err)
			}
			args := testRead(t, filepath.Join(root, "launch.args"))
			if !strings.Contains(args, "\n--maxpeers\n"+tc.want+"\n") {
				t.Fatalf("launcher omitted peer capacity: %q", args)
			}
		})
	}
}
