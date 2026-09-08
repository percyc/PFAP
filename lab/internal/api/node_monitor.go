package api

import (
	"context"
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
	for start := 0; start < len(nodes); start += monitorNodeConcurrency {
		var mu sync.Mutex
		var updates []func(*model.State) error
		monitorNodeRound(nodes[start:min(start+monitorNodeConcurrency, len(nodes))], func(ctx context.Context, node model.Node) {
			sample(ctx, node, func(update func(*model.State) error) error {
				mu.Lock()
				updates = append(updates, update)
				mu.Unlock()
				return nil
			})
		})
		if len(updates) > 0 {
			if err := commit(func(s *model.State) error {
				for _, update := range updates {
					if err := update(s); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return // Store health exposes persistence failure; next round retries sampling.
			}
		}
	}
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
