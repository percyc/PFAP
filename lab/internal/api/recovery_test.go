package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/store"
)

func newRecoveryTestAPI(t *testing.T) (*API, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lab.json")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(state *model.State) error {
		state.Servers = []model.Server{{ID: "server", Name: "worker", Host: "local"}}
		state.Experiments = []model.Experiment{{
			ID: "experiment", Status: "running", ArtifactSHA: "original-runtime",
			Nodes: []model.Node{
				{ID: "node-1", ServerID: "server", Status: "unreachable", Index: 1, LocalIndex: 1, P2PPort: 30000, RPCPort: 40000, Account: "0xaccount", LastTxBlock: "0x10"},
				{ID: "node-2", ServerID: "server", Status: "running", Index: 2, LocalIndex: 2},
			},
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Do not start monitoring or recovery goroutines in these state-machine tests.
	return &API{store: s, subscribers: map[chan model.Event]struct{}{}}, path
}

func recoveryTestState(a *API) model.State {
	var state model.State
	a.store.View(func(s model.State) { state = s })
	return state
}

func TestNewDeploymentClearsPreviousRecoveryRuntime(t *testing.T) {
	a, _ := newRecoveryTestAPI(t)
	_ = a.store.Update(func(s *model.State) error {
		s.Experiments[0].RecoveryArtifactSHA = "compatible-hotfix"
		return nil
	})
	a.setExperiment("experiment", "deploying", "", nil, "original-runtime")
	if recoveryTestState(a).Experiments[0].RecoveryArtifactSHA != "compatible-hotfix" {
		t.Fatal("same deployed runtime must retain its recovery package")
	}
	a.setExperiment("experiment", "deploying", "", nil, "new-runtime-with-new-keys")
	if recoveryTestState(a).Experiments[0].RecoveryArtifactSHA != "" {
		t.Fatal("new deployment must not inherit the previous runtime's recovery package")
	}
}

func TestBeginNodeRecoveryPreservesIdentityAndTransactions(t *testing.T) {
	a, _ := newRecoveryTestAPI(t)
	if err := a.store.Update(func(s *model.State) error {
		for _, status := range []string{"queued", "proving", "submitted", "confirmed", "failed"} {
			s.Transactions = append(s.Transactions, model.Transaction{
				ID: "tx-" + status, ExperimentID: "experiment", FromNode: "node-1", ToNode: "node-2",
				Type: "transfer", Status: status, Hash: "0x" + status,
			})
		}
		s.Experiments[0].Nodes[0].RecoveryError = "previous recovery failed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := recoveryTestState(a)
	started := time.Now()
	exp, node, servers, err := a.beginNodeRecovery("experiment", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if exp.ID != "experiment" || exp.ArtifactSHA != "original-runtime" || node.Status != "recovering" || node.RecoveryError != "" || node.RecoveryStartedAt.Before(started) {
		t.Fatalf("unexpected admitted recovery: exp=%+v node=%+v", exp, node)
	}
	if node.Account != "0xaccount" || node.LastTxBlock != "0x10" || node.Index != 1 || node.LocalIndex != 1 || node.P2PPort != 30000 || node.RPCPort != 40000 || servers["server"].ID != "server" {
		t.Fatalf("recovery changed node identity or runtime placement: %+v", node)
	}
	// The returned placement snapshot must not alias the persisted node slice.
	exp.Nodes[0].Account = "changed outside store"
	a.completeNodeRecovery("experiment", "node-1", nil)
	after := recoveryTestState(a)
	if !reflect.DeepEqual(before.Transactions, after.Transactions) {
		t.Fatalf("recovery altered or replayed transactions: before=%+v after=%+v", before.Transactions, after.Transactions)
	}
	if after.Experiments[0].Nodes[0].Account != "0xaccount" || after.Experiments[0].Nodes[1].Status != "running" {
		t.Fatalf("recovery changed unrelated state: %+v", after.Experiments[0].Nodes)
	}
}

func TestBeginNodeRecoveryValidation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		experiment string
		node       string
		change     func(*model.State)
		missing    bool
		message    string
	}{
		{name: "missing experiment", experiment: "missing", node: "node-1", missing: true},
		{name: "missing node", experiment: "experiment", node: "missing", missing: true},
		{name: "missing server", experiment: "experiment", node: "node-1", change: func(s *model.State) { s.Servers = nil }, message: "服务器配置不存在"},
		{name: "duplicate recovery", experiment: "experiment", node: "node-1", change: func(s *model.State) { s.Experiments[0].Nodes[0].Status = "recovering" }, message: "正在恢复"},
		{name: "stopped experiment", experiment: "experiment", node: "node-1", change: func(s *model.State) { s.Experiments[0].Status = "stopped" }, message: "运行中实验"},
		{name: "deploying experiment", experiment: "experiment", node: "node-1", change: func(s *model.State) { s.Experiments[0].Status = "deploying" }, message: "运行中实验"},
		{name: "stopping experiment", experiment: "experiment", node: "node-1", change: func(s *model.State) { s.Experiments[0].Status = "stopping" }, message: "运行中实验"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newRecoveryTestAPI(t)
			if tc.change != nil {
				if err := a.store.Update(func(s *model.State) error { tc.change(s); return nil }); err != nil {
					t.Fatal(err)
				}
			}
			before := recoveryTestState(a)
			_, _, _, err := a.beginNodeRecovery(tc.experiment, tc.node)
			if err == nil || (tc.missing && !errors.Is(err, os.ErrNotExist)) || (!tc.missing && !strings.Contains(err.Error(), tc.message)) {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if after := recoveryTestState(a); !reflect.DeepEqual(before, after) {
				t.Fatal("rejected recovery modified state")
			}
		})
	}
}

func TestBeginNodeRecoveryAllowsConnectionRepairForRunningNode(t *testing.T) {
	a, _ := newRecoveryTestAPI(t)
	if _, node, _, err := a.beginNodeRecovery("experiment", "node-2"); err != nil || node.Status != "recovering" {
		t.Fatalf("repair of running node rejected: node=%+v err=%v", node, err)
	}
}

func TestBeginNodeRecoveryConcurrentAdmission(t *testing.T) {
	a, _ := newRecoveryTestAPI(t)
	const callers = 12
	start := make(chan struct{})
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, _, err := a.beginNodeRecovery("experiment", "node-1")
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if err != errLifecycleBusy && !strings.Contains(err.Error(), "正在恢复") {
			t.Errorf("unexpected duplicate error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("admitted %d concurrent recoveries, want 1", succeeded)
	}
}

func TestRecoveryBlocksTransactionAdmissionAndMonitorErrors(t *testing.T) {
	a, _ := newRecoveryTestAPI(t)
	if _, _, _, err := a.beginNodeRecovery("experiment", "node-1"); err != nil {
		t.Fatal(err)
	}
	for _, tx := range []model.Transaction{
		{ID: "mint", ExperimentID: "experiment", FromNode: "node-1", Type: "mint", Status: "queued"},
		{ID: "transfer", ExperimentID: "experiment", FromNode: "node-2", ToNode: "node-1", Type: "transfer", Status: "queued"},
	} {
		if err := a.enqueueTransaction(tx); !errors.Is(err, errNodeUnavailable) {
			t.Fatalf("%s admitted for recovering node: %v", tx.Type, err)
		}
	}
	a.setNodeError("experiment", "node-1", "unreachable", "stale monitor failure")
	state := recoveryTestState(a)
	if state.Experiments[0].Nodes[0].Status != "recovering" || state.Experiments[0].Nodes[0].StateError == "stale monitor failure" || len(state.Transactions) != 0 {
		t.Fatalf("recovery guard failed: %+v", state)
	}
}

func TestCompleteNodeRecoveryPersistsOutcomeAndWarning(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status string
	}{
		{name: "success", status: "running"},
		{name: "failure", err: errors.New("SSH unavailable"), status: "unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, path := newRecoveryTestAPI(t)
			if _, _, _, err := a.beginNodeRecovery("experiment", "node-1"); err != nil {
				t.Fatal(err)
			}
			const warning = "legacy privacy continuity not verified; do not repeat CreateAccount"
			if err := a.store.Update(func(s *model.State) error {
				s.Experiments[0].Nodes[0].RecoveryWarning = warning
				s.Experiments[0].Nodes[0].RecoveryError = "older failure"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			a.completeNodeRecovery("experiment", "node-1", tc.err)
			reopened, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			reopened.View(func(s model.State) {
				node := s.Experiments[0].Nodes[0]
				if node.Status != tc.status || node.RecoveryWarning != warning || node.RecoveryStartedAt.IsZero() {
					t.Fatalf("outcome was not persisted: %+v", node)
				}
				if tc.err == nil && node.RecoveryError != "" {
					t.Fatalf("successful recovery retained error: %s", node.RecoveryError)
				}
				if tc.err != nil && (node.RecoveryError != tc.err.Error() || node.StateError == "") {
					t.Fatalf("failed recovery lost error detail: %+v", node)
				}
				if len(s.Events) != 1 || s.Events[0].Kind != "node-recovery" {
					t.Fatalf("missing recovery audit event: %+v", s.Events)
				}
			})
		})
	}
}

func TestNodeRecoveryRouteRejectsInvalidRequestsAndStop(t *testing.T) {
	a, _ := newRecoveryTestAPI(t)
	if _, _, _, err := a.beginNodeRecovery("experiment", "node-1"); err != nil {
		t.Fatal(err)
	}
	h := a.Handler(http.NotFoundHandler())
	for _, tc := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, "/api/experiments/experiment/nodes/node-1/recover", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/experiments/experiment/nodes/node-1/recover", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/experiments/experiment/nodes/node-1/recover", http.StatusConflict},
		{http.MethodPost, "/api/experiments/experiment/nodes/missing/recover", http.StatusNotFound},
		{http.MethodPost, "/api/experiments/missing/nodes/node-1/recover", http.StatusNotFound},
		{http.MethodPost, "/api/experiments/experiment/stop", http.StatusConflict},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
	state := recoveryTestState(a)
	if state.Experiments[0].Status != "running" || state.Experiments[0].Nodes[0].Status != "recovering" {
		t.Fatalf("rejected lifecycle action changed experiment: %+v", state.Experiments[0])
	}
}

func TestTransactionNodesAvailable(t *testing.T) {
	for _, tc := range []struct {
		name          string
		txType        string
		from          string
		to            string
		fromStatus    string
		toStatus      string
		experiment    string
		expStatus     string
		wantError     string
		isUnavailable bool
	}{
		{name: "mint running", txType: "mint", from: "from", fromStatus: "running"},
		{name: "mint offline", txType: "mint", from: "from", fromStatus: "unreachable", isUnavailable: true},
		{name: "mint recovering", txType: "mint", from: "from", fromStatus: "recovering", isUnavailable: true},
		{name: "transfer running", txType: "transfer", from: "from", to: "to", fromStatus: "running", toStatus: "running"},
		{name: "transfer receiver offline", txType: "transfer", from: "from", to: "to", fromStatus: "running", toStatus: "unreachable", isUnavailable: true},
		{name: "transfer receiver recovering", txType: "transfer", from: "from", to: "to", fromStatus: "running", toStatus: "recovering", isUnavailable: true},
		{name: "public receiver offline", txType: "public", from: "from", to: "to", fromStatus: "running", toStatus: "unreachable"},
		{name: "public receiver recovering", txType: "public", from: "from", to: "to", fromStatus: "running", toStatus: "recovering"},
		{name: "transfer same node", txType: "transfer", from: "from", to: "from", fromStatus: "running", wantError: "different nodes"},
		{name: "missing sender", txType: "mint", from: "missing", wantError: "source node not found"},
		{name: "missing transfer receiver", txType: "transfer", from: "from", to: "missing", fromStatus: "running", wantError: "destination node not found"},
		{name: "missing public receiver", txType: "public", from: "from", to: "missing", fromStatus: "running", wantError: "destination node not found"},
		{name: "stopped experiment", txType: "mint", from: "from", fromStatus: "running", expStatus: "stopped", wantError: "not running"},
		{name: "missing experiment", txType: "mint", from: "from", fromStatus: "running", experiment: "missing", wantError: "experiment not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.expStatus == "" {
				tc.expStatus = "running"
			}
			if tc.experiment == "" {
				tc.experiment = "experiment"
			}
			state := model.State{Experiments: []model.Experiment{{ID: "experiment", Status: tc.expStatus, Nodes: []model.Node{{ID: "from", Status: tc.fromStatus}, {ID: "to", Status: tc.toStatus}}}}}
			err := transactionNodesAvailable(state, model.Transaction{ExperimentID: tc.experiment, Type: tc.txType, FromNode: tc.from, ToNode: tc.to})
			switch {
			case tc.isUnavailable:
				if !errors.Is(err, errNodeUnavailable) {
					t.Fatalf("want node unavailable, got %v", err)
				}
			case tc.wantError != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("want %q, got %v", tc.wantError, err)
				}
			case err != nil:
				t.Fatalf("available transaction rejected: %v", err)
			}
		})
	}
}

const recoverySampleGeth = `#!/bin/sh
set -eu
[ "$1" = attach ]
bin=${0%/*}
if [ -f "$bin/hold" ]; then
    touch "$bin/started"
    while [ ! -f "$bin/release" ]; do sleep 0.01; done
fi
cat "$bin/sample.json"
`

func configureRecoverySample(t *testing.T, a *API, lastTxBlock string) (model.Experiment, model.Node, model.Server, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("local attach requires bash: %v", err)
	}
	workDir := t.TempDir()
	if err := a.store.Update(func(s *model.State) error {
		s.Servers[0].WorkDir = workDir
		s.Experiments[0].Nodes[0].Status = "running"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state := recoveryTestState(a)
	exp, server := state.Experiments[0], state.Servers[0]
	bin := filepath.Join(workDir, "artifacts", exp.ArtifactSHA, "pfap-runtime", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "geth"), []byte(recoverySampleGeth), 0700); err != nil {
		t.Fatal(err)
	}
	writeRecoverySample(t, bin, lastTxBlock)
	return exp, exp.Nodes[0], server, bin
}

func writeRecoverySample(t *testing.T, bin, lastTxBlock string) {
	t.Helper()
	response := `{"block":"25","peers":"2","account":"0xaccount","publicBalance":"100","zk":{"balance":"0x7","commitment":"0xrestored","lastTxBlockNumber":"LAST_BLOCK"},"zkError":""}`
	response = strings.ReplaceAll(response, "LAST_BLOCK", lastTxBlock)
	if err := os.WriteFile(filepath.Join(bin, "sample.json"), []byte(response+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSampleNodePrivateStateGuardAndRecovery(t *testing.T) {
	a, path := newRecoveryTestAPI(t)
	if err := a.store.Update(func(s *model.State) error {
		// A previous bad sample may already have replaced the node's cached
		// last block with zero. Confirmed transaction history still proves
		// that this is an initialized account, not a new one.
		s.Experiments[0].Nodes[0].LastTxBlock = "0x0"
		s.Transactions = []model.Transaction{{ID: "created", ExperimentID: "experiment", FromNode: "node-1", Type: "createAccount", Status: "confirmed", Hash: "0xoriginal"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	exp, node, server, bin := configureRecoverySample(t, a, "0x0")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := a.sampleNode(ctx, exp, node, server, "monitor")
	if err == nil || !strings.Contains(err.Error(), "已初始化账户") {
		t.Fatalf("silent privacy-state reset was not detected: %v", err)
	}
	state := recoveryTestState(a)
	guardedNode := state.Experiments[0].Nodes[0]
	if guardedNode.Status != "running" || guardedNode.PrivateStateError == "" || guardedNode.LastTxBlock != "0x0" || guardedNode.Block != 25 || guardedNode.Peers != 2 {
		t.Fatalf("connection health and private-state health were not separated: %+v", guardedNode)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened.View(func(s model.State) {
		if s.Experiments[0].Nodes[0].PrivateStateError != guardedNode.PrivateStateError {
			t.Fatal("private-state guard was not persisted")
		}
	})
	for _, tx := range []model.Transaction{
		{ID: "mint", ExperimentID: exp.ID, Type: "mint", FromNode: "node-1", Status: "queued"},
		{ID: "redeem", ExperimentID: exp.ID, Type: "redeem", FromNode: "node-1", Status: "queued"},
		{ID: "transfer-sender", ExperimentID: exp.ID, Type: "transfer", FromNode: "node-1", ToNode: "node-2", Status: "queued"},
		{ID: "transfer-receiver", ExperimentID: exp.ID, Type: "transfer", FromNode: "node-2", ToNode: "node-1", Status: "queued"},
	} {
		if err := a.enqueueTransaction(tx); !errors.Is(err, errNodeUnavailable) {
			t.Fatalf("private transaction %s bypassed guard: %v", tx.ID, err)
		}
	}
	for _, tx := range []model.Transaction{
		{ExperimentID: exp.ID, Type: "public", FromNode: "node-1", ToNode: "node-2"},
		{ExperimentID: exp.ID, Type: "public", FromNode: "node-2", ToNode: "node-1"},
	} {
		if err := transactionNodesAvailable(state, tx); err != nil {
			t.Fatalf("private-state guard incorrectly disabled public transfer: %v", err)
		}
	}
	if transactions := recoveryTestState(a).Transactions; len(transactions) != 1 || transactions[0].Hash != "0xoriginal" || transactions[0].Status != "confirmed" {
		t.Fatalf("guard altered transaction history: %+v", transactions)
	}
	writeRecoverySample(t, bin, "0x10")
	exp = recoveryTestState(a).Experiments[0]
	if err := a.sampleNode(ctx, exp, exp.Nodes[0], server, "monitor"); err != nil {
		t.Fatalf("restored private state rejected: %v", err)
	}
	state = recoveryTestState(a)
	if node := state.Experiments[0].Nodes[0]; node.PrivateStateError != "" || node.LastTxBlock != "0x10" || node.Status != "running" {
		t.Fatalf("successful sample did not clear private-state guard: %+v", node)
	}
	if err := a.enqueueTransaction(model.Transaction{ID: "after-recovery", ExperimentID: exp.ID, Type: "mint", FromNode: "node-1", Status: "queued"}); err != nil {
		t.Fatalf("restored node still rejects private transactions: %v", err)
	}
}

func TestSampleNodeStaleMonitorCannotOverwriteRecovery(t *testing.T) {
	for _, completeBeforeSampleReturns := range []bool{false, true} {
		name := "during recovery"
		if completeBeforeSampleReturns {
			name = "after recovery completes"
		}
		t.Run(name, func(t *testing.T) {
			a, _ := newRecoveryTestAPI(t)
			exp, node, server, bin := configureRecoverySample(t, a, "0x10")
			t.Cleanup(func() { _ = os.WriteFile(filepath.Join(bin, "release"), nil, 0600) })
			if err := os.WriteFile(filepath.Join(bin, "hold"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- a.sampleNode(ctx, exp, node, server, "monitor") }()
			deadline := time.Now().Add(2 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(bin, "started")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("sample stub did not start")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if _, _, _, err := a.beginNodeRecovery(exp.ID, node.ID); err != nil {
				t.Fatal(err)
			}
			if err := a.store.Update(func(s *model.State) error {
				s.Experiments[0].Nodes[0].PrivateStateError = "newer privacy diagnosis"
				s.Experiments[0].Nodes[0].Commitment = "0xnewer-state"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if completeBeforeSampleReturns {
				a.completeNodeRecovery(exp.ID, node.ID, nil)
			}
			before := recoveryTestState(a)
			if err := os.WriteFile(filepath.Join(bin, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("monitor sample failed: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("monitor sample did not complete")
			}
			if after := recoveryTestState(a); !reflect.DeepEqual(before, after) {
				t.Fatal("stale monitor changed recovered node state or added an account snapshot")
			}
		})
	}
}

func TestRecoveredNodeRequiresCommitmentTreeReadiness(t *testing.T) {
	for _, readiness := range []string{"missing", "false", "true"} {
		t.Run(readiness, func(t *testing.T) {
			a, _ := newRecoveryTestAPI(t)
			exp, node, server, bin := configureRecoverySample(t, a, "0x10")
			_ = a.store.Update(func(s *model.State) error {
				s.Experiments[0].Nodes[0].RecoveryWarning = "restarted legacy account"
				return nil
			})
			if readiness != "missing" {
				path := filepath.Join(bin, "sample.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data = []byte(strings.Replace(string(data), `"balance":"0x7"`, `"commitmentReady":`+readiness+`,"balance":"0x7"`, 1))
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := a.sampleNode(context.Background(), exp, node, server, "recovery")
			guarded := recoveryTestState(a).Experiments[0].Nodes[0].PrivateStateError != ""
			if readiness == "true" {
				if err != nil || guarded {
					t.Fatalf("ready commitment tree rejected: %v", err)
				}
			} else if err == nil || !guarded {
				t.Fatal("restarted node with unverified or empty commitment tree was accepted")
			}
		})
	}
}
