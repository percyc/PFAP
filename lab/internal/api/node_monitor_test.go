package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
)

func TestMonitorNodeRoundBoundsConcurrencyAndWaits(t *testing.T) {
	nodes := make([]model.Node, 300)
	for i := range nodes {
		nodes[i] = model.Node{ID: fmt.Sprintf("node-%d", i), Status: "running"}
	}
	// Unreachable nodes still need sampling to discover their recovery.
	nodes[1].Status = "unreachable"
	nodes[10].Status = "recovering"
	started := make(chan struct{}, len(nodes))
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	done := make(chan struct{})
	var mu sync.Mutex
	active, peak := 0, 0
	seen := make(map[string]int)
	var contexts []context.Context
	go func() {
		monitorNodeRound(nodes, func(ctx context.Context, node model.Node) {
			deadline, ok := ctx.Deadline()
			if remaining := time.Until(deadline); !ok || remaining <= 0 || remaining > monitorNodeTimeout {
				t.Error("node sample has no fresh individual timeout")
			}
			mu.Lock()
			active++
			peak = max(peak, active)
			seen[node.ID]++
			contexts = append(contexts, ctx)
			mu.Unlock()
			started <- struct{}{}
			<-release
			mu.Lock()
			active--
			mu.Unlock()
		})
		close(done)
	}()
	for i := 0; i < monitorNodeConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("monitor did not run samples concurrently")
		}
	}
	select {
	case <-done:
		t.Fatal("monitor round returned with samples still active")
	case <-started:
		t.Fatal("monitor exceeded its concurrency bound")
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("monitor round did not finish")
	}
	if peak != monitorNodeConcurrency || active != 0 {
		t.Fatalf("peak=%d active=%d", peak, active)
	}
	for _, node := range nodes {
		want := 1
		if node.Status == "recovering" {
			want = 0
		}
		if seen[node.ID] != want {
			t.Errorf("%s sampled %d times; want %d", node.ID, seen[node.ID], want)
		}
	}
	for _, ctx := range contexts {
		if ctx.Err() != context.Canceled {
			t.Error("sample timeout was not released")
		}
	}
}

func TestMonitorNodeRoundEmptyAndRecovering(t *testing.T) {
	for _, nodes := range [][]model.Node{nil, {{ID: "recovering", Status: "recovering"}}} {
		monitorNodeRound(nodes, func(context.Context, model.Node) {
			t.Error("unexpected node sample")
		})
	}
}

func TestMonitorNodeBatchesCommitEachGroupOnce(t *testing.T) {
	nodes := make([]model.Node, 100)
	for i := range nodes {
		nodes[i] = model.Node{ID: fmt.Sprint(i), Status: "running"}
	}
	state := model.State{}
	commits := 0
	monitorNodeBatches(nodes, func(ctx context.Context, node model.Node, enqueue func(func(*model.State) error) error) {
		if ctx.Err() != nil {
			t.Error("expired sample context")
		}
		_ = enqueue(func(s *model.State) error {
			s.Events = append(s.Events, model.Event{ID: node.ID})
			return nil
		})
	}, func(update func(*model.State) error) error {
		before := len(state.Events)
		if err := update(&state); err != nil {
			return err
		}
		if count := len(state.Events) - before; count < 1 || count > monitorNodeConcurrency {
			t.Fatalf("batch size %d", count)
		}
		commits++
		return nil
	})
	seen := map[string]bool{}
	for _, e := range state.Events {
		if seen[e.ID] {
			t.Fatal("duplicate observation")
		}
		seen[e.ID] = true
	}
	if commits != 13 || len(seen) != 100 {
		t.Fatalf("commits=%d observations=%d", commits, len(seen))
	}
}

func TestMonitorNodeBatchesStopOnPersistenceFailure(t *testing.T) {
	nodes := make([]model.Node, 20)
	commits := 0
	monitorNodeBatches(nodes, func(_ context.Context, _ model.Node, enqueue func(func(*model.State) error) error) {
		_ = enqueue(func(*model.State) error { return nil })
	}, func(func(*model.State) error) error {
		commits++
		return errors.New("disk unavailable")
	})
	if commits != 1 {
		t.Fatalf("continued after failed persistence: %d commits", commits)
	}
}
