package api

import (
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
)

func TestWarmupTimeoutSeparateFromMinimumAndMeasurement(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		nodes, requested, want int
		bad                    bool
	}{
		{"small-default", 2, 0, 2400, false},
		{"large-default", 94, 0, 6240, false},
		{"explicit-campaign", 94, 7200, 7200, false},
		{"minimum-boundary", 94, 600, 600, false},
		{"too-short", 94, 599, 0, true},
		{"negative", 94, -1, 0, true},
		{"too-long", 94, 86401, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := model.Workload{Type: "mixed", NodeIDs: make([]string, tc.nodes), WarmupSeconds: 600, WarmupTimeoutSeconds: tc.requested, DurationSeconds: 3600}
			err := configureWarmupTimeout(&w)
			if (err != nil) != tc.bad {
				t.Fatalf("error=%v", err)
			}
			if !tc.bad && (w.WarmupTimeoutSeconds != tc.want || runWarmupTimeout(w) != time.Duration(tc.want)*time.Second) {
				t.Fatal("wrong deadline", w.WarmupTimeoutSeconds)
			}
			if w.WarmupSeconds != 600 || w.DurationSeconds != 3600 {
				t.Fatal("measurement/minimum changed")
			}
		})
	}
	w := model.Workload{WarmupSeconds: 600}
	if runWarmupTimeout(w) != 40*time.Minute {
		t.Fatal("legacy deadline changed")
	}
}

func TestWarmupTimeoutIncludedInSafeRunExport(t *testing.T) {
	w := model.Workload{WarmupSeconds: 600, WarmupTimeoutSeconds: 7200, DurationSeconds: 3600}
	out := safeRun(w)
	if out["warmupTimeoutSeconds"] != float64(7200) {
		t.Fatal("missing reproducibility setting", out["warmupTimeoutSeconds"])
	}
}
