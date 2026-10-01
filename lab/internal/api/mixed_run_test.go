package api

import (
	"github.com/pfap/lab/internal/model"
	"strings"
	"testing"
	"time"
)

func TestMixedWarmupRequiresOwnBroadcastsForEachPresentType(t *testing.T) {
	now := time.Now()
	w := model.Workload{ID: "w", Type: "mixed", TransferPercent: 40, NodeIDs: []string{"a", "b"}}
	txs := []model.Transaction{{WorkloadID: "w", Type: "public", FromNode: "a", ToNode: "b", Status: "confirmed", ReadyAt: now}}
	if mixedWarmupReady(txs, w, now) {
		t.Fatal("incoming funds cannot warm b or Transfer")
	}
	txs = append(txs, model.Transaction{WorkloadID: "w", Type: "public", FromNode: "b", ToNode: "a", Status: "confirmed", ReadyAt: now})
	w.TransferPercent = 0
	if !mixedWarmupReady(txs, w, now) {
		t.Fatal("alpha zero must not require Transfer")
	}
	w.TransferPercent = 40
	if mixedWarmupReady(txs, w, now) {
		t.Fatal("missing Transfer warmup")
	}
	for _, owner := range w.NodeIDs {
		txs = append(txs, model.Transaction{WorkloadID: "w", Type: "transfer", ToNode: owner, Status: "confirmed", ReadyAt: now})
	}
	if !mixedWarmupReady(txs, w, now) {
		t.Fatal("completed mixed warmup rejected")
	}
	w.TransferPercent = 100
	if !mixedWarmupReady(txs[2:], w, now) {
		t.Fatal("alpha100 must not require Public")
	}
	if mixedWarmupReady(txs, w, now.Add(time.Second)) {
		t.Fatal("stale warmup counted")
	}
}

func TestPublicExpressionHasExplicitNonzeroFee(t *testing.T) {
	x := publicTransactionExpression("0x123", "1")
	if !strings.Contains(x, `gasPrice:"20000000000"`) || !strings.Contains(x, `gas:21000`) {
		t.Fatal("public transaction depends on zero-fee anonymous gas oracle", x)
	}
}

func TestMixedQuotaEveryFiveBroadcasts(t *testing.T) {
	for percent := 0; percent <= 100; percent += 20 {
		for index := 1; index <= 100; index++ {
			count := 0
			for i := 0; i < 50; i++ {
				if mixedNextType(percent, index, i) == "transfer" {
					count++
				}
			}
			if count != percent/2 {
				t.Fatalf("percent=%d index=%d got=%d", percent, index, count)
			}
		}
	}
}

func TestPublicReservationOnlyOccupiesSender(t *testing.T) {
	tx := []model.Transaction{{Type: "public", FromNode: "a", ToNode: "b", Status: "settling"}}
	if !transactionNodesBusy(tx, "a", "") || transactionNodesBusy(tx, "b", "") {
		t.Fatal("public reservation")
	}
	tx[0].Type = "transfer"
	if !transactionNodesBusy(tx, "a", "") || !transactionNodesBusy(tx, "b", "") {
		t.Fatal("transfer reservation")
	}
}

func TestPublicReadinessUsesCanonicalDepthNotPrivateBalance(t *testing.T) {
	x := readinessSample{TransactionHash: "tx", Hash: "block", Canonical: "block", Head: 15, Block: 10, Status: "1"}
	if !validPublicRunReadiness(x, "tx", 6) {
		t.Fatal("valid public receipt rejected")
	}
	if validRunReadiness(x, "tx", "0", 6) {
		t.Fatal("private state requirements lost")
	}
	for _, mutate := range []func(*readinessSample){func(x *readinessSample) { x.Head = 14 }, func(x *readinessSample) { x.Canonical = "fork" }, func(x *readinessSample) { x.Status = "0" }, func(x *readinessSample) { x.TransactionHash = "wrong" }} {
		y := x
		mutate(&y)
		if validPublicRunReadiness(y, "tx", 6) {
			t.Fatal("unsafe public release")
		}
	}
}

func TestMixedSchedulerBroadcasterQuotaAndBusyRecipient(t *testing.T) {
	a, now := runFixture(t)
	a.store.View(func(s model.State) {
		e, w := s.Experiments[0], s.Workloads[0]
		w.Type, w.TransferPercent = "mixed", 20
		// Force every account's next slot to Public, and make the recipient busy.
		for i := range e.Nodes {
			e.Nodes[i].Index = 0
			e.Nodes[i].Account = "account"
		}
		s.Transactions = []model.Transaction{{Type: "public", FromNode: "b", ToNode: "a", Status: "submitted"}}
		typ, from, to, ok := chooseRunTask(s, e, w, now)
		if !ok || typ != "public" || from.ID == "b" || from.ID == to.ID {
			t.Fatalf("bad public task %s %s %s %v", typ, from.ID, to.ID, ok)
		}
		w.TransferPercent = 100
		typ, from, to, ok = chooseRunTask(s, e, w, now)
		if ok && (typ != "transfer" || from.ID == "b" || to.ID == "b" || from.ID == to.ID) {
			t.Fatal("busy Transfer participant selected")
		}
	})
}
