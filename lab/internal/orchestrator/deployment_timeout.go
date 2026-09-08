package orchestrator

import (
	"context"
	"fmt"
	"github.com/pfap/lab/internal/model"
	"time"
)

// The total budget scales with the sequential placement plan. Each remote
// operation also has its own deadline, so a stalled worker cannot consume it all.
func DeploymentTimeout(e model.Experiment) time.Duration {
	nodes := 0
	for _, p := range e.Placements {
		nodes += max(0, min(p.Count, 300))
	}
	return 15*time.Minute + time.Duration(min(len(e.Placements), 300))*3*time.Minute + time.Duration(min(nodes, 300))*2*time.Minute
}

// StopNodes gives each node 25 seconds, plus SSH and bookkeeping overhead.
func StopTimeout(e model.Experiment) time.Duration {
	count := len(e.Nodes)
	if count == 0 {
		for _, p := range e.Placements {
			count += max(0, min(p.Count, 300))
		}
	}
	return max(20*time.Minute, time.Duration(min(count, 300))*30*time.Second)
}

func deploymentStep(ctx context.Context, stage string, limit time.Duration, action func(context.Context) (string, error)) (string, error) {
	step, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	out, err := action(step)
	if err != nil {
		if step.Err() != nil {
			return out, fmt.Errorf("%s: %w（阶段上限 %s；远端结果需核验，不自动重试）", stage, step.Err(), limit)
		}
		return out, fmt.Errorf("%s: %w", stage, err)
	}
	return out, nil
}
