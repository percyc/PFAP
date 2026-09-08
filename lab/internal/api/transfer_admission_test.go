package api

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
)

func frozenPayerFixture(t *testing.T) (*API, time.Time, string) {
	t.Helper()
	a, now := runFixture(t)
	commitment := "0x" + strings.Repeat("a", 64)
	saveTestState(t, a, func(s *model.State) {
		s.Transactions = []model.Transaction{{ID: "tx", ExperimentID: "e", WorkloadID: "w", Type: "transfer", RunPhase: "warmup", Status: "proving", ExecutionStage: "payer-proof", FromNode: "a", ToNode: "b", Value: "0x1", ExpectedPayerBalance: "9", ExpectedReceiverBalance: "11", ProvingAt: now.Add(-time.Minute)}}
		n := &s.Experiments[0].Nodes[0]
		n.ZKBalance, n.Commitment, n.LastTxBlock = "0x9", commitment, "0x0"
		n.PrivateStateError = unconfirmedPrivateStateMessage
	})
	return a, now, commitment
}

func TestTransferReceiverAdmissionAllowsOnlyCurrentFrozenPayer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*model.State)
	}{
		{"other payer error", func(s *model.State) { s.Experiments[0].Nodes[0].PrivateStateError = "genuine state error" }},
		{"receiver error", func(s *model.State) { s.Experiments[0].Nodes[1].PrivateStateError = unconfirmedPrivateStateMessage }},
		{"different commitment", func(s *model.State) { s.Experiments[0].Nodes[0].Commitment = "0x" + strings.Repeat("b", 64) }},
		{"different balance", func(s *model.State) { s.Experiments[0].Nodes[0].ZKBalance = "0xa" }},
		{"unknown expected balance", func(s *model.State) { s.Transactions[0].ExpectedPayerBalance = "" }},
		{"already on chain", func(s *model.State) { s.Experiments[0].Nodes[0].LastTxBlock = "0x1" }},
		{"unknown state block", func(s *model.State) { s.Experiments[0].Nodes[0].LastTxBlock = "" }},
		{"recovery warning", func(s *model.State) { s.Experiments[0].Nodes[0].RecoveryWarning = "unresolved recovery" }},
		{"old observation", func(s *model.State) { s.Experiments[0].Nodes[0].LastSeen = time.Now().Add(-3 * time.Minute) }},
		{"before proof", func(s *model.State) {
			s.Experiments[0].Nodes[0].LastSeen = s.Transactions[0].ProvingAt.Add(-time.Second)
		}},
		{"future observation", func(s *model.State) { s.Experiments[0].Nodes[0].LastSeen = time.Now().Add(time.Minute) }},
		{"payer offline", func(s *model.State) { s.Experiments[0].Nodes[0].Status = "unreachable" }},
		{"receiver offline", func(s *model.State) { s.Experiments[0].Nodes[1].Status = "unreachable" }},
		{"payer miner", func(s *model.State) { s.Experiments[0].Nodes[0].IsMiner = true }},
		{"receiver miner", func(s *model.State) { s.Experiments[0].Nodes[1].IsMiner = true }},
		{"experiment stopped", func(s *model.State) { s.Experiments[0].Status = "stopped" }},
		{"manual transfer", func(s *model.State) { s.Transactions[0].WorkloadID = ""; s.Transactions[0].RunPhase = "" }},
		{"legacy workload", func(s *model.State) { s.Workloads[0].Strategy = "round-robin" }},
		{"different execution stage", func(s *model.State) { s.Transactions[0].ExecutionStage = "submit" }},
		{"already submitted", func(s *model.State) { s.Transactions[0].SubmissionAttemptedAt = time.Now() }},
		{"not transfer", func(s *model.State) { s.Transactions[0].Type = "mint" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, commitment := frozenPayerFixture(t)
			saveTestState(t, a, tc.mutate)
			before := recoveryTestState(a)
			if err := a.beginTransferReceiverRPC("tx", commitment); err == nil {
				t.Fatal("unsafe or unrelated state was allowed")
			}
			if after := recoveryTestState(a); !reflect.DeepEqual(before, after) {
				t.Fatal("rejected receiver submission changed durable state")
			}
		})
	}
}

func TestTransferReceiverAdmissionPreservesFrozenDiagnosticsAndReservation(t *testing.T) {
	a, _, commitment := frozenPayerFixture(t)
	before := recoveryTestState(a)
	if err := a.beginTransferReceiverRPC("tx", commitment); err != nil {
		t.Fatal(err)
	}
	after := recoveryTestState(a)
	if !reflect.DeepEqual(before.Experiments, after.Experiments) || len(after.Transactions) != 1 ||
		after.Transactions[0].Status != "proving" || after.Transactions[0].ExecutionStage != "submit" ||
		after.Transactions[0].SubmissionAttemptedAt.IsZero() || !transactionNodesBusy(after.Transactions, "a", "b") {
		t.Fatal("receiver continuation cleared diagnostics, released accounts, or created another transaction")
	}
	if err := a.beginTransferReceiverRPC("tx", commitment); err == nil {
		t.Fatal("frozen-state exception allowed a repeated receiver submission")
	}
	if err := a.enqueueTransaction(model.Transaction{ID: "second", ExperimentID: "e", Type: "transfer", FromNode: "a", ToNode: "b"}); err == nil {
		t.Fatal("continuing the current transfer admitted a second transaction")
	}
}

func TestFrozenPayerExceptionIsNotAvailableToOrdinaryRPCAdmission(t *testing.T) {
	a, _, _ := frozenPayerFixture(t)
	if err := a.beginTransactionRPC("tx", "submit"); err == nil {
		t.Fatal("ordinary RPC admission bypassed the private-state error without proof evidence")
	}
	if err := a.beginTransferReceiverRPC("tx", "not-a-commitment"); err == nil {
		t.Fatal("malformed payer proof commitment was accepted")
	}
}

func TestPeriodicFrozenPayerSampleCanContinueItsReceiverSubmission(t *testing.T) {
	a, _ := newRecoveryTestAPI(t)
	exp, node, server, bin := configureRecoverySample(t, a, "0x0")
	commitment := "0x" + strings.Repeat("a", 64)
	payload, err := os.ReadFile(filepath.Join(bin, "sample.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The runtime's GetPayerNextState freezes old value - transfer value and
	// returns that same new CMT. Model old=8, value=1 -> balance=0x7, not 0x8.
	payload = []byte(strings.Replace(string(payload), "0xrestored", commitment, 1))
	if err := os.WriteFile(filepath.Join(bin, "sample.json"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	saveTestState(t, a, func(s *model.State) {
		s.Workloads = []model.Workload{{ID: "w", ExperimentID: exp.ID, Strategy: "ready-pool"}}
		s.Transactions = []model.Transaction{{ID: "tx", ExperimentID: exp.ID, WorkloadID: "w", Type: "transfer", RunPhase: "warmup", Status: "proving", ExecutionStage: "payer-proof", FromNode: node.ID, ToNode: "node-2", Value: "0x1", ExpectedPayerBalance: "7", ExpectedReceiverBalance: "1", ProvingAt: time.Now().Add(-time.Minute)}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.sampleNode(ctx, exp, node, server, "monitor"); err == nil {
		t.Fatal("fixture did not reproduce the frozen-account diagnostic")
	}
	current := recoveryTestState(a).Experiments[0].Nodes[0]
	if current.PrivateStateError != unconfirmedPrivateStateMessage || current.ZKBalance != "0x7" || current.Commitment != commitment {
		t.Fatal("sampled payer does not match the actual freeze transition")
	}
	if err := a.beginTransferReceiverRPC("tx", commitment); err != nil {
		t.Fatalf("expected current-transfer freeze incorrectly blocked receiver submission: %v", err)
	}
}
