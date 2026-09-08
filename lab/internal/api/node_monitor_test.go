package api

import (
	"context"
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
