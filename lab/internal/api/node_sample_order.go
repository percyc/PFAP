package api

import (
	"time"

	"github.com/pfap/lab/internal/model"
)

// A query begun before a newer successful observation (or recovery) may carry
// stale state. Ignore its late result atomically; never move LastSeen forward for
// a failure. The next ordinary query still detects a genuinely unavailable node.
func obsoleteNodeSample(exp model.Experiment, node model.Node, started time.Time, reason string) bool {
	return exp.Status != "running" || node.LastSeen.After(started) || node.RecoveryStartedAt.After(started) || (node.Status == "recovering" && reason != "recovery")
}

func (a *API) setNodeSampleError(experimentID, nodeID, message, reason string, started time.Time) {
	_ = a.store.Update(func(s *model.State) error {
		for i := range s.Experiments {
			e := &s.Experiments[i]
			if e.ID != experimentID {
				continue
			}
			for j := range e.Nodes {
				n := &e.Nodes[j]
				if n.ID != nodeID || obsoleteNodeSample(*e, *n, started, reason) || n.Status == "recovering" {
					continue
				}
				n.Status, n.StateError, n.Mining = "unreachable", message, nil
			}
		}
		return nil
	})
}
