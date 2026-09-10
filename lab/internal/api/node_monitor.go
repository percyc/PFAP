package api

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/pfap/lab/internal/model"
)

const monitorNodeConcurrency = 8
const monitorNodeTimeout = 10 * time.Second

// Persist a small group of successful observations atomically. Avoid one full
// JSON rewrite and durable backup per node, without relaxing sample freshness,
// error handling, or the per-node obsolete-observation guards. Manual samples
// still commit immediately. Failed saves do not publish any part of the batch.
func monitorNodeBatches(nodes []model.Node, sample func(context.Context, model.Node, func(func(*model.State) error) error), commit func(func(*model.State) error) error) {
	monitorNodeBatchesLimit(nodes, monitorNodeConcurrency, sample, commit)
}

func monitorNodeBatchesLimit(nodes []model.Node, concurrency int, sample func(context.Context, model.Node, func(func(*model.State) error) error), commit func(func(*model.State) error) error) {
	eligible := make([]model.Node, 0, len(nodes))
	for _, node := range nodes {
		if node.Status != "recovering" {
			eligible = append(eligible, node)
		}
	}
	nodes = eligible
	concurrency = max(1, min(concurrency, monitorNodeConcurrency))
	type result struct{ updates []func(*model.State) error }
	results := make(chan result, concurrency)
	next, active := 0, 0
	launch := func() bool {
		if next == len(nodes) {
			return false
		}
		node := nodes[next]
		next++
		active++
		go func() {
			var mu sync.Mutex
			var updates []func(*model.State) error
			ctx, cancel := context.WithTimeout(context.Background(), monitorNodeTimeout)
			defer cancel()
			sample(ctx, node, func(update func(*model.State) error) error {
				mu.Lock()
				updates = append(updates, update)
				mu.Unlock()
				return nil
			})
			results <- result{updates}
		}()
		return true
	}
	for active < concurrency && launch() {
	}
	var updates []func(*model.State) error
	failed := false
	for active > 0 {
		r := <-results
		active--
		if failed {
			continue
		}
		updates = append(updates, r.updates...)
		if len(updates) >= concurrency || (active == 0 && next == len(nodes)) {
			if len(updates) > 0 {
				if err := commit(func(s *model.State) error {
					for _, update := range updates {
						if err := update(s); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					failed = true // Drain outstanding work without publishing after failure.
				}
			}
			updates = nil
		}
		if !failed {
			launch()
		}
	}
}

// Proof RPCs can hold the private-account lock for minutes. Keep their monitor
// queue independent so idle/readiness/observer samples never wait behind it.
// Submitted, settling and uncertain transactions remain in the ordinary lane:
// their state must be observed promptly to reconcile or complete confirmation.
func monitorLaneNodes(s model.State, exp model.Experiment, proving bool) []model.Node {
	busy := make(map[string]bool)
	for _, tx := range s.Transactions {
		if tx.ExperimentID == exp.ID && tx.Status == "proving" {
			busy[tx.FromNode] = true
			busy[tx.ToNode] = true
		}
	}
	var nodes []model.Node
	for _, node := range exp.Nodes {
		if busy[node.ID] == proving {
			nodes = append(nodes, node)
		}
	}
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].LastSeen.Before(nodes[j].LastSeen) })
	return nodes
}

func monitorNodeProving(s model.State, experimentID, nodeID string) bool {
	for _, tx := range s.Transactions {
		if tx.ExperimentID == experimentID && tx.Status == "proving" && (tx.FromNode == nodeID || tx.ToNode == nodeID) {
			return true
		}
	}
	return false
}

// monitorNodeRound waits for the entire bounded round, so slow samples cannot
// cause overlapping rounds. Each timeout starts only when a worker is available,
// not while a node is waiting in the queue. Recovery owns its own state checks.
func monitorNodeRound(nodes []model.Node, sample func(context.Context, model.Node)) {
	jobs := make(chan model.Node)
	var workers sync.WaitGroup
	for i := 0; i < min(monitorNodeConcurrency, len(nodes)); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for node := range jobs {
				ctx, cancel := context.WithTimeout(context.Background(), monitorNodeTimeout)
				sample(ctx, node)
				cancel()
			}
		}()
	}
	for _, node := range nodes {
		if node.Status != "recovering" {
			jobs <- node
		}
	}
	close(jobs)
	workers.Wait()
}
