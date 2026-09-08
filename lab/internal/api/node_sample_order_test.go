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

func TestLateSuccessfulMonitorCannotOverwriteNewReadiness(t *testing.T) {
	a, _ := newRecoveryTestAPI(t)
	exp, node, server, bin := configureRecoverySample(t, a, "0x10")
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(bin, "release"), nil, 0600) })
	if err := os.WriteFile(filepath.Join(bin, "hold"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- a.sampleNode(ctx, exp, node, server, "monitor") }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(bin, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sample stub did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	saveTestState(t, a, func(s *model.State) {
		n := &s.Experiments[0].Nodes[0]
		n.LastSeen, n.Commitment, n.Block = time.Now(), "0xnew-readiness", 100
	})
	before := recoveryTestState(a)
	if err := os.WriteFile(filepath.Join(bin, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("sample stub did not finish")
	}
	if after := recoveryTestState(a); !reflect.DeepEqual(before, after) {
		t.Fatal("late success changed newer account state or added stale snapshot")
	}
}

func TestLateSampleErrorCannotOverwriteNewReadiness(t *testing.T) {
	for _, status := range []string{"proving", "settling", "confirmed"} {
		t.Run(status, func(t *testing.T) {
			a, now := runFixture(t)
			saveTestState(t, a, func(s *model.State) {
				s.Transactions = []model.Transaction{{ID: "tx", ExperimentID: "e", FromNode: "a", ToNode: "b", Status: status}}
				s.Experiments[0].Nodes[0].LastSeen = now
			})
			before := recoveryTestState(a)
			started := now.Add(-time.Second)
			ctx, cancel := context.WithDeadline(context.Background(), started)
			defer cancel()
			if handled, err := a.preserveBusyMonitorTimeout(ctx, "e", "a", "monitor", "old timeout", started); err != nil || !handled {
				t.Fatalf("stale timeout not discarded: %t %v", handled, err)
			}
			a.setNodeSampleError("e", "a", "old hard failure", "monitor", started)
			if after := recoveryTestState(a); !reflect.DeepEqual(before, after) {
				t.Fatal("late error overwrote newer successful account observation")
			}
			// The same error from a query started after that sample must still
			// fail closed. Ordering does not hide ongoing hard failures.
			a.setNodeSampleError("e", "a", "current failure", "monitor", now.Add(time.Second))
			n := recoveryTestState(a).Experiments[0].Nodes[0]
			if n.Status != "unreachable" || n.StateError != "current failure" || !n.LastSeen.Equal(now) {
				t.Fatal("current failure was ignored or fabricated freshness")
			}
		})
	}
}

func TestSampleOrderingGuard(t *testing.T) {
	now := time.Now()
	e := model.Experiment{Status: "running"}
	n := model.Node{Status: "running", LastSeen: now}
	if !obsoleteNodeSample(e, n, now.Add(-time.Second), "monitor") || obsoleteNodeSample(e, n, now, "monitor") {
		t.Fatal("sample start ordering boundary incorrect")
	}
	n.Status, n.RecoveryStartedAt = "recovering", now
	if !obsoleteNodeSample(e, n, now.Add(time.Second), "monitor") || obsoleteNodeSample(e, n, now.Add(time.Second), "recovery") {
		t.Fatal("recovery isolation incorrect")
	}
	e.Status = "stopped"
	if !obsoleteNodeSample(e, n, now.Add(time.Second), "recovery") {
		t.Fatal("stopped experiment accepted a late observation")
	}
}
