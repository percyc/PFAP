package api

import (
	"context"
	"errors"
	"time"

	"github.com/pfap/lab/internal/model"
)

var errNotBusyMonitorTimeout = errors.New("not a monitor timeout on an occupied running node")

// A timed-out periodic query cannot distinguish proof-related contention from
// a dead process. Retain the previous observation only while a durable account
// reservation prevents reuse. Do not fabricate freshness or clear other errors:
// after the reservation ends, StateError still blocks admission until a real
// successful sample arrives. Manual/readiness queries and hard failures retain
// the usual fail-closed behavior.
func (a *API) preserveBusyMonitorTimeout(ctx context.Context, experimentID, nodeID, reason, message string, started time.Time) (bool, error) {
	if reason != "monitor" || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return false, nil
	}
	err := a.store.Update(func(s *model.State) error {
		for i := range s.Experiments {
			e := &s.Experiments[i]
			if e.ID != experimentID || e.Status != "running" {
				continue
			}
			for j := range e.Nodes {
				n := &e.Nodes[j]
				if n.ID != nodeID {
					continue
				}
				if n.Status != "running" {
					continue
				}
				if obsoleteNodeSample(*e, *n, started, reason) {
					return nil
				}
				for _, tx := range s.Transactions {
					if tx.ExperimentID == experimentID && activeTransaction(tx.Status) && (tx.FromNode == nodeID || tx.ToNode == nodeID) {
						n.StateError = message
						return nil
					}
				}
			}
		}
		return errNotBusyMonitorTimeout
	})
	if errors.Is(err, errNotBusyMonitorTimeout) {
		return false, nil
	}
	return err == nil, err
}
