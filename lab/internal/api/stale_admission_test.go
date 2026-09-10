package api

import (
	"github.com/pfap/lab/internal/model"
	"testing"
	"time"
)

func TestStaleIdleQuarantineAndReadmission(t *testing.T) {
	a, now := runFixture(t)
	saveTestState(t, a, func(s *model.State) { s.Experiments[0].Nodes[0].LastSeen = now.Add(-3 * time.Minute) })
	a.store.View(func(s model.State) {
		if len(runProblems(s, s.Experiments[0], s.Workloads[0], true, now)) == 0 {
			t.Fatal("initial preflight relaxed")
		}
		if p := runProblems(s, s.Experiments[0], s.Workloads[0], false, now); len(p) != 0 {
			t.Fatal(p)
		}
		payer, receiver, ok := chooseRunPairAt(s, s.Experiments[0], s.Workloads[0], now)
		if !ok || payer.ID == "a" || receiver.ID == "a" {
			t.Fatal("stale account selected")
		}
		s.Experiments[0].Nodes[0].LastSeen = now
		payer, _, ok = chooseRunPairAt(s, s.Experiments[0], s.Workloads[0], now)
		if !ok || payer.ID != "a" {
			t.Fatal("fresh account not readmitted")
		}
	})
	tx, done, err := a.runFlowTick("w", now)
	if err != nil || done || tx == nil || tx.FromNode == "a" || tx.ToNode == "a" {
		t.Fatalf("healthy pair not admitted: %+v %v %v", tx, done, err)
	}
}

func TestAllStaleWaitsWithoutReleasingReservations(t *testing.T) {
	a, now := runFixture(t)
	saveTestState(t, a, func(s *model.State) {
		for i := 0; i < 4; i++ {
			s.Experiments[0].Nodes[i].LastSeen = now.Add(-3 * time.Minute)
		}
		s.Transactions = []model.Transaction{{ID: "held", ExperimentID: "e", FromNode: "a", ToNode: "b", Status: "proving"}}
	})
	tx, done, err := a.runFlowTick("w", now)
	if err != nil || done || tx != nil {
		t.Fatalf("all stale must wait: %+v %v %v", tx, done, err)
	}
	a.store.View(func(s model.State) {
		if s.Workloads[0].Status != "running" || s.Transactions[0].Status != "proving" {
			t.Fatal("run stopped or reservation changed")
		}
		samples := s.Workloads[0].Configuration["admissionSamples"].([]any)
		sample := samples[0].(map[string]any)
		if sample["busyAccounts"] != float64(2) || len(sample["staleIdleNodeIds"].([]any)) != 2 {
			t.Fatal("incorrect availability audit")
		}
	})
}

func TestPrivacyFaultStillStopsRun(t *testing.T) {
	a, now := runFixture(t)
	saveTestState(t, a, func(s *model.State) { s.Experiments[0].Nodes[0].PrivateStateError = "invalid commitment" })
	tx, done, err := a.runFlowTick("w", now)
	if err != nil || !done || tx != nil {
		t.Fatal("privacy fault ignored")
	}
}
