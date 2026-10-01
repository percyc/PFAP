package api

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
)

func TestPairProbeDoesNotSpendReceiversDeadlineOnPayer(t *testing.T) {
	started := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	probeTransferPair(ctx, 100*time.Millisecond, func(c context.Context) {
		select {
		case <-started:
		case <-c.Done():
			t.Error("receiver waited behind payer")
		}
	}, func(c context.Context) {
		if c.Err() != nil {
			t.Error("expired receiver context")
		}
		close(started)
	})
}

func TestAlreadyCanceledSampleNeverMarksNodeUnavailable(t *testing.T) {
	for _, reason := range []string{"transfer-recheck:tx", "transaction-recheck:tx", "monitor", "transaction:tx", "run-readiness:tx", "manual-query", "recovery"} {
		t.Run(reason, func(t *testing.T) {
			a, _ := runFixture(t)
			before := recoveryTestState(a)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if a.sampleNode(ctx, model.Experiment{}, model.Node{}, model.Server{}, reason) == nil {
				t.Fatal("missing cancellation")
			}
			if !reflect.DeepEqual(before, recoveryTestState(a)) {
				t.Fatal("canceled probe modified node state")
			}
		})
	}
}

func TestTransferProofJournalIsPrivateAndNeverOverwritten(t *testing.T) {
	a, _ := runFixture(t)
	tx := model.Transaction{ID: "tx-abcdef", Type: "transfer", ExperimentID: "e", FromNode: "a", ToNode: "b", Value: "1"}
	raw := `{"proofA":"0x1234"}`
	if err := a.saveTransferProof(tx, "sha", raw); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(a.store.DataDir(), "transfer-proofs", tx.ID+".json")
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0600 {
		t.Fatal("public journal permissions")
	}
	if a.saveTransferProof(tx, "sha", `{"different":true}`) == nil {
		t.Fatal("overwrote proof")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("proof changed")
	}
	tx.ID = "../escape"
	if a.saveTransferProof(tx, "sha", raw) == nil {
		t.Fatal("path traversal")
	}
}
