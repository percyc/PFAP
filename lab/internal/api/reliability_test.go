package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/orchestrator"
	"github.com/pfap/lab/internal/store"
)

func saveTestState(t *testing.T, a *API, change func(*model.State)) {
	t.Helper()
	if err := a.store.Update(func(s *model.State) error { change(s); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestRestartReconciliationDoesNotReplay(t *testing.T) {
	s := model.State{
		Experiments: []model.Experiment{{ID: "exp", Status: "deploying", Nodes: []model.Node{{ID: "n", Status: "running"}}}},
		Transactions: []model.Transaction{
			{ID: "queued", Status: "queued"},
			{ID: "started", FromNode: "n", Status: "proving"},
			{ID: "submitted", Status: "submitted", Hash: "hash"},
			{ID: "timeout", Status: "timeout", Hash: "hash"},
			{ID: "reverted", Status: "failed", Hash: "hash", Receipt: `{"status":"0x0"}`},
			{ID: "preflight", Status: "failed"},
			{ID: "done", Status: "confirmed"},
		},
		Workloads: []model.Workload{{ID: "w", Status: "running", Submitted: 2}},
	}
	reconcileInterruptedTasks(&s)
	for i, expected := range []string{"cancelled", "unknown", "unknown", "unknown", "failed", "failed", "confirmed"} {
		if s.Transactions[i].Status != expected {
			t.Fatalf("transaction %d: got %s, want %s", i, s.Transactions[i].Status, expected)
		}
	}
	if s.Experiments[0].Status != "interrupted" || s.Experiments[0].Nodes[0].Status != "unknown" {
		t.Fatal("interrupted lifecycle was not made explicit")
	}
	w := s.Workloads[0]
	if w.Status != "interrupted" || !w.StopRequested || w.SubmissionStoppedAt.IsZero() || w.Submitted != 2 {
		t.Fatalf("workload restart state: %+v", w)
	}
	if !transactionNodesBusy(s.Transactions, "n", "") {
		t.Fatal("uncertain execution must keep the node reserved")
	}
	firstStopped := w.SubmissionStoppedAt
	reconcileInterruptedTasks(&s)
	if s.Workloads[0].SubmissionStoppedAt != firstStopped || s.Transactions[0].Status != "cancelled" {
		t.Fatal("restart reconciliation was not idempotent")
	}
}

func TestLifecyclePinsRuntimeBeforeDeploymentAndResume(t *testing.T) {
	a, _ := newRecoveryTestAPI(t)
	runtimePath := filepath.Join(t.TempDir(), "runtime.tar.gz")
	data := []byte("immutable test runtime")
	if err := os.WriteFile(runtimePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	saveTestState(t, a, func(s *model.State) {
		s.Experiments[0] = model.Experiment{ID: "experiment", Status: "draft", ArtifactPath: runtimePath, P2PPortBase: 30000, RPCPortBase: 40000, Placements: []model.Placement{{ServerID: "server", Count: 2}}}
	})
	exp, _, action, err := a.beginLifecycle("experiment", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	if action != "deploying" || exp.ArtifactSHA != hex.EncodeToString(hash[:]) || len(exp.Nodes) != 2 || exp.Nodes[1].ID != "experiment-n2" || exp.Nodes[1].P2PPort != 30001 {
		t.Fatalf("deployment manifest not pinned: %+v, %s", exp, action)
	}
	if _, _, _, err := a.beginLifecycle("experiment", "deploy"); err == nil {
		t.Fatal("duplicate deployment admitted")
	}
	started := time.Now().Add(-time.Hour)
	saveTestState(t, a, func(s *model.State) {
		s.Experiments[0].Status = "stopped"
		s.Experiments[0].StartedAt = started
		s.Experiments[0].Nodes[0].Account = "original-account"
		s.Experiments[0].ArtifactPath = filepath.Join(t.TempDir(), "missing-latest-runtime")
	})
	exp, _, action, err = a.beginLifecycle("experiment", "start")
	if err != nil || action != "resuming" || exp.ArtifactSHA != hex.EncodeToString(hash[:]) || exp.Nodes[0].Account != "original-account" || !exp.StartedAt.Equal(started) {
		t.Fatalf("resume read or reset mutable artifact/state: action=%s exp=%+v error=%v", action, exp, err)
	}
}

func TestStopAdmissionPreservesUncertaintyAndPartialResults(t *testing.T) {
	a, _ := newRecoveryTestAPI(t)
	saveTestState(t, a, func(s *model.State) {
		s.Transactions = []model.Transaction{{ID: "q", ExperimentID: "experiment", Status: "queued"}, {ID: "p", ExperimentID: "experiment", Status: "proving"}, {ID: "s", ExperimentID: "experiment", Status: "submitted", Hash: "hash"}}
		s.Workloads = []model.Workload{{ID: "w", ExperimentID: "experiment", Status: "running", Submitted: 3}}
	})
	_, _, action, err := a.beginLifecycle("experiment", "stop")
	if err != nil || action != "stopping" {
		t.Fatalf("stop admission: %s %v", action, err)
	}
	s := recoveryTestState(a)
	if s.Transactions[0].Status != "cancelled" || s.Transactions[1].Status != "unknown" || s.Transactions[2].Status != "unknown" || s.Transactions[2].Hash != "hash" || s.Workloads[0].Status != "interrupted" {
		t.Fatalf("unsafe stop transition: %+v", s)
	}
	// Late workers must not overwrite a stop decision or claim RPC failure.
	_ = a.finishTx("p", "failed", "", errors.New("late preflight"))
	_ = a.finishTx("q", "failed", "", errors.New("late preflight"))
	_ = a.finishWorkload("w", "completed", "")
	if err := a.completeExperimentStop("experiment", []orchestrator.StopResult{{NodeID: "node-1", Status: "stopped"}, {NodeID: "node-2", Status: "unknown", Error: "SSH unavailable"}}, errors.New("partial stop"), ""); err != nil {
		t.Fatal(err)
	}
	s = recoveryTestState(a)
	if s.Experiments[0].Status != "stop-failed" || s.Experiments[0].Nodes[0].Status != "stopped" || s.Experiments[0].Nodes[1].Status != "unknown" || !activeExperimentStatus("stop-failed") || s.Transactions[1].Status != "unknown" || s.Transactions[0].Status != "cancelled" || s.Workloads[0].Status != "interrupted" {
		t.Fatal("late worker or partial stop lost the unresolved state")
	}
	w := httptest.NewRecorder()
	a.deleteExperiment(w, "experiment")
	if w.Code != http.StatusConflict {
		t.Fatalf("partially stopped experiment can be deleted: %d", w.Code)
	}
	if err := a.completeExperimentStop("experiment", []orchestrator.StopResult{{NodeID: "node-1", Status: "stopped"}, {NodeID: "node-2", Status: "stopped"}}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if recoveryTestState(a).Experiments[0].Status != "stopped" {
		t.Fatal("verified stop should finish")
	}
}

func TestWorkloadStopAndAtomicAttemptCounts(t *testing.T) {
	a, _ := newRecoveryTestAPI(t)
	saveTestState(t, a, func(s *model.State) {
		s.Experiments[0].Nodes[0].Status = "running"
		s.Workloads = []model.Workload{{ID: "w", ExperimentID: "experiment", Status: "running"}}
	})
	tx := model.Transaction{ID: "one", ExperimentID: "experiment", WorkloadID: "w", FromNode: "node-1", Type: "mint", Status: "queued"}
	if queued, err := a.enqueueWorkloadTick("w", tx); err != nil || !queued {
		t.Fatalf("initial enqueue: %t %v", queued, err)
	}
	tx.ID = "busy"
	if queued, err := a.enqueueWorkloadTick("w", tx); err != nil || queued {
		t.Fatalf("busy tick: %t %v", queued, err)
	}
	saveTestState(t, a, func(s *model.State) { s.Experiments[0].Nodes[0].Status = "unreachable" })
	if queued, err := a.enqueueWorkloadTick("w", tx); err != nil || queued {
		t.Fatalf("offline tick: %t %v", queued, err)
	}
	w, err := a.stopWorkloadSubmission("w")
	if err != nil || w.Status != "draining" || !w.StopRequested || w.Submitted != 1 || w.Attempted != 3 || w.SkippedBusy != 1 || w.SkippedUnavailable != 1 || w.SubmissionStoppedAt.IsZero() {
		t.Fatalf("stop/count state: %+v %v", w, err)
	}
	if queued, err := a.enqueueWorkloadTick("w", tx); !errors.Is(err, errWorkloadStopped) || queued {
		t.Fatalf("enqueue after stop: %t %v", queued, err)
	}
	s := recoveryTestState(a)
	if len(s.Transactions) != 1 || s.Transactions[0].Status != "queued" || s.Workloads[0].Attempted != 3 {
		t.Fatal("stop must retain admitted work and exclude future ticks")
	}
	saveTestState(t, a, func(s *model.State) {
		s.Workloads = append(s.Workloads, model.Workload{ID: "never-started", Status: "queued"})
	})
	w, err = a.stopWorkloadSubmission("never-started")
	if err != nil || w.Status != "cancelled" {
		t.Fatalf("queued workload stop: %+v %v", w, err)
	}
}

func TestServerFailurePreservesLastGoodMetrics(t *testing.T) {
	a, _ := newRecoveryTestAPI(t)
	if err := a.recordServerCheck("server", "host=test load=1 memory=42", nil); err != nil {
		t.Fatal(err)
	}
	before := recoveryTestState(a).Servers[0]
	if err := a.recordServerCheck("server", "", errors.New("SSH timeout")); err != nil {
		t.Fatal(err)
	}
	after := recoveryTestState(a).Servers[0]
	if after.Status != "error" || after.LastError != "SSH timeout" || after.SystemInfo != before.SystemInfo || !after.LastSuccessAt.Equal(before.LastSuccessAt) {
		t.Fatalf("last known metrics lost: %+v", after)
	}
	_ = a.recordServerCheck("server", "host=test load=2", nil)
	if recoveryTestState(a).Servers[0].LastError != "" {
		t.Fatal("successful check did not clear current error")
	}
}

func TestPersistenceFailurePreventsExecutionIntentAndFinalization(t *testing.T) {
	a, path := newRecoveryTestAPI(t)
	saveTestState(t, a, func(s *model.State) {
		s.Experiments[0].Nodes[0].Status = "running"
		s.Transactions = []model.Transaction{{ID: "tx", ExperimentID: "experiment", FromNode: "node-1", Type: "mint", Status: "queued"}}
	})
	// A directory at the primary path forces persistence to fail even as root.
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := a.beginTransactionRPC("tx", "submit"); err == nil {
		t.Fatal("execution intent was accepted without a durable save")
	}
	if err := a.confirmTx("tx", "hash", `{"status":"0x1"}`, "1", "0x1", 10, 20); err == nil {
		t.Fatal("receipt was finalized without a durable save")
	}
	tx := recoveryTestState(a).Transactions[0]
	if tx.Status != "queued" || !tx.SubmissionAttemptedAt.IsZero() || tx.Receipt != "" || tx.Hash != "" || !a.store.Health().Degraded {
		t.Fatalf("failed save leaked evidence/status: %+v", tx)
	}
	blocked := New(a.store)
	if blocked.startupError == nil {
		t.Fatal("startup should fail closed on persistence error")
	}
	w := httptest.NewRecorder()
	blocked.Handler(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/transactions", strings.NewReader(`{}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("mutation after startup failure: %d %s", w.Code, w.Body.String())
	}
}

func TestStorageRecoverySavesPendingTransactionResults(t *testing.T) {
	a, path := newRecoveryTestAPI(t)
	saveTestState(t, a, func(s *model.State) {
		s.Transactions = []model.Transaction{{ID: "unsent", Status: "queued"}, {ID: "sent", Status: "proving"}}
	})
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := a.finishTx("unsent", "failed", "", errors.New("command could not be saved; no RPC executed")); err == nil {
		t.Fatal("expected finalization persistence error")
	}
	if err := a.markTransactionUnknown("sent", "0x"+strings.Repeat("c", 64), errors.New("receipt timeout")); err == nil {
		t.Fatal("expected evidence persistence error")
	}
	// Remove only the empty fault-injection directory created by this test.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	a.flushPendingTransactionResults()
	s := recoveryTestState(a)
	if s.Transactions[0].Status != "failed" || s.Transactions[1].Status != "unknown" || s.Transactions[1].Hash == "" || a.store.Health().Degraded {
		t.Fatalf("transient storage failure left orphaned work: %+v", s.Transactions)
	}
}

func TestResumeWithNoReadyNodesRemainsRetryable(t *testing.T) {
	a, _ := newRecoveryTestAPI(t)
	saveTestState(t, a, func(s *model.State) {
		s.Experiments[0].Status = "resuming"
		s.Experiments[0].ArtifactSHA = strings.Repeat("a", 64)
	})
	a.resumeExperiment(recoveryTestState(a).Experiments[0], map[string]model.Server{})
	exp := recoveryTestState(a).Experiments[0]
	if exp.Status != "interrupted" || exp.Nodes[0].RecoveryError == "" {
		t.Fatalf("failed resume was presented as running: %+v", exp)
	}
	if _, _, action, err := a.beginLifecycle(exp.ID, "resume"); err != nil || action != "resuming" {
		t.Fatalf("failed full resume cannot be retried: %s %v", action, err)
	}
}

func TestReadOnlyTransactionReconciliation(t *testing.T) {
	hash, blockHash := "0x"+strings.Repeat("a", 64), "0x"+strings.Repeat("b", 64)
	receipt := func(status, txHash, canonical string) string {
		return fmt.Sprintf(`{"receipt":{"transactionHash":%q,"blockHash":%q,"blockNumber":"0x10","status":%q},"canonicalHash":%q}`, txHash, blockHash, status, canonical)
	}
	for _, tt := range []struct {
		name, hash, response, expected string
		wantErr                        bool
	}{
		{"confirmed", hash, receipt("0x1", hash, blockHash), "confirmed", false},
		{"reverted", hash, receipt("0x0", hash, blockHash), "failed", false},
		{"missing", hash, `{"receipt":null,"canonicalHash":null}`, "unknown", true},
		{"noncanonical", hash, receipt("0x1", hash, "different-block"), "unknown", true},
		{"wrong-hash", hash, receipt("0x1", "different-hash", blockHash), "unknown", true},
		{"no-hash", "", "", "unknown", true},
		{"malformed", hash, `{"receipt":{},"canonicalHash":null}`, "unknown", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, path := newRecoveryTestAPI(t)
			_, _, _, bin := configureRecoverySample(t, a, "0x10")
			// The fake node records every expression. It supports only receipt
			// lookup and state sampling: no transaction or account initialization.
			script := "#!/bin/sh\nset -eu\nbin=${0%/*}\nprintf '%s\\n' \"$*\" >> \"$bin/calls\"\ncase \"$*\" in\n*eth.getTransactionReceipt*) cat \"$bin/receipt.json\";;\n*) cat \"$bin/sample.json\";;\nesac\n"
			if err := os.WriteFile(filepath.Join(bin, "geth"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "receipt.json"), []byte(tt.response), 0600); err != nil {
				t.Fatal(err)
			}
			saveTestState(t, a, func(s *model.State) {
				s.Transactions = []model.Transaction{{ID: "tx", ExperimentID: "experiment", FromNode: "node-1", Type: "mint", Status: "unknown", Hash: tt.hash}}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			tx, err := a.reconcileTransaction(ctx, "tx")
			if (err != nil) != tt.wantErr || tx.Status != tt.expected || tx.LastCheckedAt.IsZero() {
				t.Fatalf("reconcile result: status=%s error=%v checked=%v", tx.Status, err, tx.LastCheckedAt)
			}
			if tt.wantErr && !transactionNodesBusy(recoveryTestState(a).Transactions, "node-1", "") {
				t.Fatal("unresolved node reservation released")
			}
			if !tt.wantErr && (tx.Receipt == "" || tx.ConfirmedAt.IsZero() || tx.ReconciliationError != "") {
				t.Fatal("receipt finalization incomplete")
			}
			calls, readErr := os.ReadFile(filepath.Join(bin, "calls"))
			if tt.hash == "" && !errors.Is(readErr, os.ErrNotExist) {
				t.Fatal("no-hash reconciliation must not query or mutate the node")
			}
			for _, forbidden := range []string{"eth.send", "eth.revert", " init ", "account new"} {
				if strings.Contains(string(calls), forbidden) {
					t.Fatalf("unsafe reconciliation command: %s", calls)
				}
			}
			reopened, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			reopened.View(func(s model.State) {
				if s.Transactions[0].Status != tt.expected {
					t.Fatal("reconciliation state not durable")
				}
			})
		})
	}
}
