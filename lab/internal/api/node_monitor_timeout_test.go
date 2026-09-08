package api

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
)

func TestBusyMonitorTimeoutPreservesOnlyOccupiedRunningObservation(t *testing.T) {
	for _, tc := range []struct {
		name, reason, nodeStatus, txStatus, txExperiment, from, to string
		deadline, preserve                                         bool
	}{
		{"payer proving", "monitor", "running", "proving", "e", "a", "b", true, true},
		{"receiver proving", "monitor", "running", "proving", "e", "b", "a", true, true},
		{"queued", "monitor", "running", "queued", "e", "a", "b", true, true},
		{"submitted", "monitor", "running", "submitted", "e", "a", "b", true, true},
		{"settling", "monitor", "running", "settling", "e", "a", "b", true, true},
		{"unknown still reserved", "monitor", "running", "unknown", "e", "a", "b", true, true},
		{"confirmed idle", "monitor", "running", "confirmed", "e", "a", "b", true, false},
		{"failed idle", "monitor", "running", "failed", "e", "a", "b", true, false},
		{"other pair", "monitor", "running", "proving", "e", "c", "d", true, false},
		{"other experiment", "monitor", "running", "proving", "other", "a", "b", true, false},
		{"already unreachable", "monitor", "unreachable", "proving", "e", "a", "b", true, false},
		{"recovering", "monitor", "recovering", "proving", "e", "a", "b", true, false},
		{"manual", "manual-query", "running", "proving", "e", "a", "b", true, false},
		{"readiness", "run-readiness:tx", "running", "settling", "e", "a", "b", true, false},
		{"transaction refresh", "transaction:tx", "running", "submitted", "e", "a", "b", true, false},
		{"hard failure", "monitor", "running", "proving", "e", "a", "b", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := runFixture(t)
			saveTestState(t, a, func(s *model.State) {
				s.Experiments[0].Nodes[0].Status = tc.nodeStatus
				s.Transactions = []model.Transaction{{ID: "tx", ExperimentID: tc.txExperiment, FromNode: tc.from, ToNode: tc.to, Type: "transfer", Status: tc.txStatus}}
			})
			before := recoveryTestState(a)
			ctx := context.Background()
			if tc.deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			preserved, err := a.preserveBusyMonitorTimeout(ctx, "e", "a", tc.reason, "query timeout", time.Now())
			if err != nil || preserved != tc.preserve {
				t.Fatalf("preserved=%t want=%t err=%v", preserved, tc.preserve, err)
			}
			after := recoveryTestState(a)
			if tc.preserve {
				if after.Experiments[0].Nodes[0].StateError != "query timeout" {
					t.Fatal("timeout warning missing")
				}
				after.Experiments[0].Nodes[0].StateError = before.Experiments[0].Nodes[0].StateError
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("timeout modified more than the allowed StateError field")
			}
		})
	}
}

func TestBusyMonitorTimeoutBlocksReuseAfterReservationEnds(t *testing.T) {
	a, now := runFixture(t)
	saveTestState(t, a, func(s *model.State) {
		s.Transactions = []model.Transaction{{ID: "tx", ExperimentID: "e", WorkloadID: "w", FromNode: "a", ToNode: "b", Type: "transfer", Status: "proving"}}
	})
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(-time.Second))
	defer cancel()
	if preserved, err := a.preserveBusyMonitorTimeout(ctx, "e", "a", "monitor", "query timeout", time.Now()); err != nil || !preserved {
		t.Fatalf("preserved=%t err=%v", preserved, err)
	}
	a.store.View(func(s model.State) {
		if problems := runProblems(s, s.Experiments[0], s.Workloads[0], false, now); len(problems) != 0 {
			t.Fatalf("occupied account caused an idle-state false alarm: %v", problems)
		}
		if !transactionNodesBusy(s.Transactions, "a", "") {
			t.Fatal("timeout released the account reservation")
		}
	})
	saveTestState(t, a, func(s *model.State) { s.Transactions[0].Status = "confirmed" })
	if preserved, err := a.preserveBusyMonitorTimeout(ctx, "e", "a", "monitor", "later timeout", time.Now()); err != nil || preserved {
		t.Fatalf("idle account retained observation: preserved=%t err=%v", preserved, err)
	}
	tx, done, err := a.runFlowTick("w", now)
	if err != nil || tx != nil || !done {
		t.Fatalf("idle account with a query error admitted new work: tx=%v done=%t err=%v", tx != nil, done, err)
	}
}

func TestSampleNodeMonitorTimeoutIntegration(t *testing.T) {
	for _, tc := range []struct {
		name, reason, txStatus string
		preserve               bool
	}{
		{"busy monitor", "monitor", "proving", true},
		{"idle monitor", "monitor", "confirmed", false},
		{"busy manual", "manual-query", "proving", false},
		{"busy readiness", "run-readiness:tx", "settling", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newRecoveryTestAPI(t)
			exp, node, server, _ := configureRecoverySample(t, a, "0x10")
			lastSeen := time.Now().Add(-time.Minute)
			mining := false
			saveTestState(t, a, func(s *model.State) {
				s.Experiments[0].Nodes[0].Mining = &mining
				s.Experiments[0].Nodes[0].LastSeen = lastSeen
				s.Transactions = []model.Transaction{{ID: "tx", ExperimentID: exp.ID, FromNode: node.ID, Status: tc.txStatus}}
			})
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			if err := a.sampleNode(ctx, exp, node, server, tc.reason); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected deadline failure, got %v", err)
			}
			current := recoveryTestState(a).Experiments[0].Nodes[0]
			if !current.LastSeen.Equal(lastSeen) || !strings.Contains(current.StateError, "节点状态查询超时") {
				t.Fatal("timeout fabricated freshness or lost the warning")
			}
			if tc.preserve {
				if current.Status != "running" || current.Mining == nil || *current.Mining {
					t.Fatal("busy periodic timeout overwrote the previous observation")
				}
			} else if current.Status != "unreachable" || current.Mining != nil {
				t.Fatal("non-periodic timeout was not fail-closed")
			}
		})
	}
}

func TestSampleNodeBusyMonitorHardFailureStaysFailClosed(t *testing.T) {
	a, _ := newRecoveryTestAPI(t)
	exp, node, server, _ := configureRecoverySample(t, a, "0x10")
	// Use a nonexistent runtime under the test-only directory, not a real SSH
	// host. This fails immediately with a command error, not a context timeout.
	exp.ArtifactSHA = "missing-test-runtime"
	mining := false
	saveTestState(t, a, func(s *model.State) {
		s.Experiments[0].Nodes[0].Mining = &mining
		s.Transactions = []model.Transaction{{ID: "tx", ExperimentID: exp.ID, FromNode: node.ID, Status: "proving"}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.sampleNode(ctx, exp, node, server, "monitor"); err == nil || ctx.Err() != nil {
		t.Fatalf("expected immediate non-timeout failure: %v (context %v)", err, ctx.Err())
	}
	current := recoveryTestState(a).Experiments[0].Nodes[0]
	if current.Status != "unreachable" || current.Mining != nil || current.StateError == "" {
		t.Fatal("hard monitor failure was not fail-closed")
	}
}
