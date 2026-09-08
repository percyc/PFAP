package api

import (
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
)

func TestRunReportRequiresMeasuredBlocksNotOnlyWarmup(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	end := start.Add(time.Minute)
	w := model.Workload{ID: "w", Strategy: "ready-pool", Status: "completed", MeasurementStartedAt: start, MeasurementEndsAt: end,
		Blocks: []model.RunBlock{{Number: 1, Size: 100, ObservedAt: start.Add(-2 * time.Second)}, {Number: 2, Size: 100, ObservedAt: start.Add(-time.Second)}}}
	s := model.State{Transactions: []model.Transaction{{ID: "tx", WorkloadID: "w", RunPhase: "measuring", Status: "confirmed", SubmittedAt: start, ConfirmedAt: start.Add(10 * time.Second), ReadyAt: start.Add(15 * time.Second)}}}
	for samples := 0; samples <= 2; samples++ {
		r := runReport(s, w, end)
		if r["valid"].(bool) != (samples >= 2) {
			t.Fatalf("measured samples=%d valid=%v", samples, r["valid"])
		}
		w.Blocks = append(w.Blocks, model.RunBlock{Number: uint64(samples + 3), Size: 100, Timestamp: uint64(samples + 1), ObservedAt: start.Add(time.Duration(samples+1) * time.Second)})
	}
}

func TestWarmupReorgRequiresNewParticipantReadiness(t *testing.T) {
	a, now := runFixture(t)
	setReadiness := func(ready time.Time) {
		saveTestState(t, a, func(s *model.State) {
			s.Workloads[0].Blocks = []model.RunBlock{{Number: 1}, {Number: 2}}
			s.Workloads[0].Configuration = map[string]any{"blockWarmupResetAt": now.Add(5 * time.Second).Format(time.RFC3339Nano)}
			s.Transactions = []model.Transaction{{WorkloadID: "w", Status: "confirmed", ReadyAt: ready, FromNode: "a", ToNode: "b"}, {WorkloadID: "w", Status: "confirmed", ReadyAt: ready, FromNode: "c", ToNode: "d"}}
		})
	}
	setReadiness(now)
	tx, done, err := a.runFlowTick("w", now.Add(16*time.Second))
	if err != nil || done || tx == nil || tx.RunPhase != "warmup" {
		t.Fatalf("old-branch readiness must stay in warmup: tx=%v done=%v err=%v", tx, done, err)
	}
	setReadiness(now.Add(7 * time.Second))
	tx, done, err = a.runFlowTick("w", now.Add(17*time.Second))
	if err != nil || done || tx == nil || tx.RunPhase != "measuring" {
		t.Fatalf("new readiness after the full reset interval did not qualify: tx=%v done=%v err=%v", tx, done, err)
	}
}
