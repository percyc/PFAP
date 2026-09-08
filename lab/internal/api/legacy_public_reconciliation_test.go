package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/store"
)

func legacyPublicStartupFixture(lines int) model.Transaction {
	output := strings.Repeat(legacyConsoleStartupFatal+"\n", lines)
	return model.Transaction{
		ID: "legacy-public", ExperimentID: "experiment", Type: "public", FromNode: "node-1", ToNode: "node-2",
		Status: "unknown", ExecutionStage: "submit", SubmittedAt: time.Now().Add(-time.Minute),
		ProvingAt: time.Now().Add(-time.Minute), SubmissionAttemptedAt: time.Now().Add(-time.Minute),
		Error: "执行结果待核验；不会自动重发，相关节点继续保留交易占用。 RPC 返回失败：ssh: exit status 1: " + strings.TrimSpace(output) + ": " + output,
	}
}

func TestLegacyPublicStartupRequiresExactNonExecutionEvidence(t *testing.T) {
	for _, lines := range []int{1, 2} {
		if !legacyPublicConsoleNeverEvaluated(legacyPublicStartupFixture(lines)) {
			t.Fatalf("exact %d-line Fatalf output was not recognized", lines)
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*model.Transaction)
	}{
		{"transfer", func(tx *model.Transaction) { tx.Type = "transfer" }},
		{"create-account", func(tx *model.Transaction) { tx.Type = "createAccount" }},
		{"mint", func(tx *model.Transaction) { tx.Type = "mint" }},
		{"redeem", func(tx *model.Transaction) { tx.Type = "redeem" }},
		{"active-rpc", func(tx *model.Transaction) { tx.Status = "proving" }},
		{"payer-stage", func(tx *model.Transaction) { tx.ExecutionStage = "payer-proof" }},
		{"no-stage", func(tx *model.Transaction) { tx.ExecutionStage = "" }},
		{"no-node", func(tx *model.Transaction) { tx.FromNode = "" }},
		{"hash", func(tx *model.Transaction) { tx.Hash = "0x" + strings.Repeat("a", 64) }},
		{"receipt", func(tx *model.Transaction) { tx.Receipt = "{}" }},
		{"broadcast", func(tx *model.Transaction) { tx.BroadcastAt = time.Now() }},
		{"confirmed", func(tx *model.Transaction) { tx.ConfirmedAt = time.Now() }},
		{"ready", func(tx *model.Transaction) { tx.ReadyAt = time.Now() }},
		{"block", func(tx *model.Transaction) { tx.BlockNumber = "0x1" }},
		{"receipt-status", func(tx *model.Transaction) { tx.ReceiptStatus = "0x1" }},
		{"no-intent", func(tx *model.Transaction) { tx.SubmissionAttemptedAt = time.Time{} }},
		{"not-started", func(tx *model.Transaction) { tx.ProvingAt = time.Time{} }},
		{"extra-prefix", func(tx *model.Transaction) { tx.Error = "unexpected output\n" + tx.Error }},
		{"extra-suffix", func(tx *model.Transaction) { tx.Error += "unexpected output" }},
		{"extra-empty-line", func(tx *model.Transaction) { tx.Error += "\n" }},
		{"three-lines", func(tx *model.Transaction) { tx.Error = legacyPublicStartupFixture(3).Error }},
		{"different-fatal", func(tx *model.Transaction) {
			tx.Error = strings.Replace(tx.Error, "api modules", "transaction submit", 1)
		}},
		{"wrapper-mismatch", func(tx *model.Transaction) {
			tx.Error = strings.Replace(tx.Error, "exit status 1", "exit status 137", 1)
		}},
		{"local-wrapper", func(tx *model.Transaction) { tx.Error = strings.Replace(tx.Error, "ssh: ", "", 1) }},
		{"missing-newline", func(tx *model.Transaction) { tx.Error = strings.TrimSuffix(tx.Error, "\n") }},
		{"crlf", func(tx *model.Transaction) { tx.Error = strings.ReplaceAll(tx.Error, "\n", "\r\n") }},
		{"generic-timeout", func(tx *model.Transaction) { tx.Error = "RPC 返回失败：context deadline exceeded" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := legacyPublicStartupFixture(2)
			tc.mutate(&tx)
			if legacyPublicConsoleNeverEvaluated(tx) {
				t.Fatal("ambiguous execution evidence was accepted")
			}
		})
	}
}

func TestLegacyPublicStartupReconciliationIsDurableWithoutRPCOrReplay(t *testing.T) {
	a, path := newRecoveryTestAPI(t)
	original := legacyPublicStartupFixture(2)
	saveTestState(t, a, func(s *model.State) { s.Transactions = []model.Transaction{original} })
	updates := make(chan model.Event, 2)
	a.subscribers[updates] = struct{}{}
	rr := httptest.NewRecorder()
	a.transactionAction(rr, httptest.NewRequest("POST", "/api/transactions/"+original.ID+"/reconcile", nil))
	if rr.Code != 200 {
		t.Fatalf("reconcile failed: %d %s", rr.Code, rr.Body.String())
	}
	var tx model.Transaction
	if err := json.Unmarshal(rr.Body.Bytes(), &tx); err != nil {
		t.Fatal(err)
	}
	if tx.Status != "failed" || tx.Hash != "" || tx.Receipt != "" || !tx.ConfirmedAt.IsZero() || !tx.BroadcastAt.IsZero() || tx.LastCheckedAt.IsZero() || tx.ReconciliationError != "" {
		t.Fatalf("non-execution was misreported as on-chain confirmation: %+v", tx)
	}
	if tx.Error != original.Error+"\n"+legacyPublicStartupConclusion {
		t.Fatal("original error or reconciliation conclusion was lost")
	}
	state := recoveryTestState(a)
	if len(state.Transactions) != 1 || transactionNodesBusy(state.Transactions, original.FromNode, original.ToNode) || len(state.Events) != 1 || len(updates) != 1 {
		t.Fatal("wrong reservation, replay, or audit result")
	}
	if state.Events[0].Fields["result"] != "not-executed" || state.Events[0].Fields["id"] != original.ID {
		t.Fatal("audit event missing non-execution evidence")
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened.View(func(s model.State) {
		if s.Transactions[0].Status != "failed" || len(s.Events) != 1 {
			t.Fatal("result and audit were not committed together")
		}
	})
	_, _ = a.reconcileTransaction(context.Background(), original.ID)
	if len(recoveryTestState(a).Events) != 1 || len(recoveryTestState(a).Transactions) != 1 {
		t.Fatal("repeated reconciliation duplicated work")
	}
}

func TestLegacyPublicStartupReconciliationRetainsAnonymousReservation(t *testing.T) {
	for _, kind := range []string{"transfer", "mint", "redeem", "createAccount"} {
		t.Run(kind, func(t *testing.T) {
			a, _ := newRecoveryTestAPI(t)
			tx := legacyPublicStartupFixture(2)
			tx.Type = kind
			saveTestState(t, a, func(s *model.State) { s.Transactions = []model.Transaction{tx} })
			result, err := a.reconcileTransaction(context.Background(), tx.ID)
			state := recoveryTestState(a)
			if err == nil || result.Status != "unknown" || !transactionNodesBusy(state.Transactions, tx.FromNode, tx.ToNode) || len(state.Events) != 0 {
				t.Fatal("anonymous state reservation released without chain evidence")
			}
		})
	}
}

func TestLegacyPublicStartupReconciliationRequiresExecutionLockAndFreshEvidence(t *testing.T) {
	for _, scenario := range []string{"locked", "changed-node", "changed-error", "changed-hash", "pending-uncertainty", "pending-finish", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			a, _ := newRecoveryTestAPI(t)
			tx := legacyPublicStartupFixture(2)
			saveTestState(t, a, func(s *model.State) { s.Transactions = []model.Transaction{tx} })
			ctx := context.Background()
			switch scenario {
			case "locked":
				lock := &sync.Mutex{}
				lock.Lock()
				defer lock.Unlock()
				a.nodeLocks.Store(tx.FromNode, lock)
			case "changed-node":
				saveTestState(t, a, func(s *model.State) { s.Transactions[0].FromNode = "other-node" })
			case "changed-error":
				saveTestState(t, a, func(s *model.State) { s.Transactions[0].Error += "additional evidence" })
			case "changed-hash":
				saveTestState(t, a, func(s *model.State) { s.Transactions[0].Hash = "0x" + strings.Repeat("a", 64) })
			case "pending-uncertainty":
				a.pendingUncertainty.Store(tx.ID, uncertainResult{cause: errors.New("unpersisted outcome")})
			case "pending-finish":
				a.pendingTxFinishes.Store(tx.ID, transactionFinish{status: "unknown"})
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			before := recoveryTestState(a)
			_, handled, err := a.reconcileLegacyPublicStartup(ctx, tx)
			after := recoveryTestState(a)
			if !handled || err == nil || after.Transactions[0].Status != "unknown" || len(after.Events) != 0 || after.Transactions[0].Error != before.Transactions[0].Error {
				t.Fatal("busy, changed or cancelled evidence released reservation")
			}
		})
	}
}

func TestLegacyPublicStartupReconciliationStorageFailurePreservesReservation(t *testing.T) {
	a, path := newRecoveryTestAPI(t)
	tx := legacyPublicStartupFixture(2)
	saveTestState(t, a, func(s *model.State) { s.Transactions = []model.Transaction{tx} })
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	_, handled, err := a.reconcileLegacyPublicStartup(context.Background(), tx)
	state := recoveryTestState(a)
	if !handled || err == nil || state.Transactions[0].Status != "unknown" || state.Transactions[0].Error != tx.Error || len(state.Events) != 0 || !transactionNodesBusy(state.Transactions, tx.FromNode, tx.ToNode) {
		t.Fatal("failed persistence released reservation or partially recorded evidence")
	}
}
