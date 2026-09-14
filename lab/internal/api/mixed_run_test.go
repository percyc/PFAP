package api

import (
	"github.com/pfap/lab/internal/model"
	"testing"
)

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
