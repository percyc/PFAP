package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
)

func TestReceiverAdmissionReprobesWithoutReplayingSubmission(t *testing.T) {
	a, _, cmt := frozenPayerFixture(t)
	saveTestState(t, a, func(s *model.State) { s.Experiments[0].Nodes[1].Status = "unreachable" })
	probes, admissions := 0, 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := waitTransferReceiverAdmission(ctx, time.Millisecond, func() error {
		admissions++
		return a.beginTransferReceiverRPC("tx", cmt)
	}, func(context.Context) {
		probes++
		saveTestState(t, a, func(s *model.State) { s.Experiments[0].Nodes[1].Status = "running" })
	})
	if err != nil || probes != 1 || admissions != 2 {
		t.Fatalf("err=%v probes=%d attempts=%d", err, probes, admissions)
	}
	tx := recoveryTestState(a).Transactions[0]
	if tx.SubmissionAttemptedAt.IsZero() || tx.ExecutionStage != "submit" {
		t.Fatal("missing durable admission")
	}
	if !transactionNodesBusy(recoveryTestState(a).Transactions, "a", "b") {
		t.Fatal("account reservation lost")
	}
	if a.beginTransferReceiverRPC("tx", cmt) == nil {
		t.Fatal("duplicate submission permitted")
	}
}

func TestPayerAdmissionReprobesBeforeOneDurableProofIntent(t *testing.T) {
	a, _, _ := frozenPayerFixture(t)
	saveTestState(t, a, func(s *model.State) {
		tx := &s.Transactions[0]
		tx.Status, tx.ExecutionStage, tx.ProvingAt = "queued", "", time.Time{}
		s.Experiments[0].Nodes[0].PrivateStateError = ""
		s.Experiments[0].Nodes[0].LastTxBlock = "0x1"
		s.Experiments[0].Nodes[1].Status = "unreachable"
	})
	probes := 0
	err := waitTransferReceiverAdmission(context.Background(), time.Millisecond, func() error {
		return a.beginTransactionRPC("tx", "payer-proof")
	}, func(context.Context) {
		probes++
		s := recoveryTestState(a)
		if !s.Transactions[0].ProvingAt.IsZero() || !transactionNodesBusy(s.Transactions, "a", "b") {
			t.Fatal("proof intent issued early or account reservation lost")
		}
		saveTestState(t, a, func(s *model.State) { s.Experiments[0].Nodes[1].Status = "running" })
	})
	if err != nil || probes != 1 {
		t.Fatalf("err=%v probes=%d", err, probes)
	}
	tx := recoveryTestState(a).Transactions[0]
	if tx.ProvingAt.IsZero() || tx.ExecutionStage != "payer-proof" || !tx.SubmissionAttemptedAt.IsZero() {
		t.Fatal("incorrect durable proof intent")
	}
	if a.beginTransactionRPC("tx", "payer-proof") == nil {
		t.Fatal("duplicate payer proof admission allowed")
	}
}

func TestReceiverAdmissionDoesNotRetryOtherErrors(t *testing.T) {
	for _, want := range []error{errors.New("storage failed"), errors.New("transaction already submitted"), errors.New("private state mismatch")} {
		calls := 0
		err := waitTransferReceiverAdmission(context.Background(), time.Millisecond, func() error { calls++; return want }, func(context.Context) { t.Fatal("unexpected probe") })
		if err != want || calls != 1 {
			t.Fatal("retried non-liveness error")
		}
	}
}

func TestPublicAdmissionReprobesWithoutDuplicateSubmission(t *testing.T) {
	a, _, _ := frozenPayerFixture(t)
	saveTestState(t, a, func(s *model.State) {
		tx := &s.Transactions[0]
		tx.Type, tx.Status, tx.ExecutionStage, tx.ProvingAt = "public", "queued", "", time.Time{}
		s.Experiments[0].Nodes[0].Status = "unreachable"
	})
	probes := 0
	err := waitTransferReceiverAdmission(context.Background(), time.Millisecond, func() error {
		return a.beginTransactionRPC("tx", "submit")
	}, func(context.Context) {
		probes++
		if !recoveryTestState(a).Transactions[0].SubmissionAttemptedAt.IsZero() {
			t.Fatal("RPC admitted while unavailable")
		}
		saveTestState(t, a, func(s *model.State) { s.Experiments[0].Nodes[0].Status = "running" })
	})
	if err != nil || probes != 1 || recoveryTestState(a).Transactions[0].SubmissionAttemptedAt.IsZero() {
		t.Fatalf("err=%v probes=%d", err, probes)
	}
	if a.beginTransactionRPC("tx", "submit") == nil {
		t.Fatal("duplicate public submission admitted")
	}
}

func TestReceiverAdmissionCancellationNeverAdmitsAfterProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := waitTransferReceiverAdmission(ctx, time.Millisecond, func() error { calls++; return errNodeUnavailable }, func(context.Context) { cancel() })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestReceiverAdmissionTimeoutPreservesUnsubmittedState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := waitTransferReceiverAdmission(ctx, time.Millisecond, func() error { return errNodeUnavailable }, func(context.Context) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if !strings.Contains(err.Error(), errNodeUnavailable.Error()) {
		t.Fatal("timeout lost the admission rejection")
	}
}

func TestReceiverAdmissionBudgetRemainsBounded(t *testing.T) {
	if receiverAdmissionTimeout != 5*time.Minute || receiverAdmissionProbeTimeout != time.Minute || receiverAdmissionProbeTimeout >= receiverAdmissionTimeout {
		t.Fatal("unexpected post-proof admission budgets")
	}
}
