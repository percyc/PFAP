package api

import (
	"context"
	"sync"
	"time"

	"github.com/pfap/lab/internal/model"
)

const monitorNodeConcurrency = 8
const monitorNodeTimeout = 10 * time.Second

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
