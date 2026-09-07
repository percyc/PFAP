package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pfap/lab/internal/model"
)

var errWorkloadStopped = errors.New("workload stopped submitting")

func (a *API) workloadAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[3] != "stop" {
		fail(w, 404, errors.New("unknown workload action"))
		return
	}
	if r.Method != http.MethodPost {
		fail(w, 405, errors.New("method not allowed"))
		return
	}
	workload, err := a.stopWorkloadSubmission(parts[2])
	if err != nil {
		code := http.StatusConflict
		if errors.Is(err, os.ErrNotExist) {
			code = http.StatusNotFound
		}
		if a.store.Health().Degraded {
			code = http.StatusServiceUnavailable
		}
		fail(w, code, err)
		return
	}
	if workload.Status == "draining" {
		go a.drainWorkload(workload, workload.Submitted)
	}
	jsonOut(w, http.StatusAccepted, workload)
}

func (a *API) stopWorkloadSubmission(workloadID string) (model.Workload, error) {
	var result model.Workload
	err := a.store.Update(func(s *model.State) error {
		for i := range s.Workloads {
			w := &s.Workloads[i]
			if w.ID != workloadID {
				continue
			}
			if w.Status != "queued" && w.Status != "running" && w.Status != "draining" && w.Status != "interrupted" {
				result = *w
				return nil // idempotent; terminal rules never start again
			}
			w.StopRequested = true
			if w.SubmissionStoppedAt.IsZero() {
				w.SubmissionStoppedAt = time.Now()
			}
			w.Status, w.Error, w.FinishedAt = "draining", "", time.Time{}
			if w.Submitted == 0 {
				w.Status, w.FinishedAt = "cancelled", time.Now()
			}
			result = *w
			return nil
		}
		return os.ErrNotExist
	})
	return result, err
}

// Check stopRequested in the same durable update as enqueueing: after the
// stop endpoint returns, no later scheduler tick can enqueue another tx.
func (a *API) enqueueWorkloadTick(workloadID string, tx model.Transaction) (bool, error) {
	queued := false
	err := a.store.Update(func(s *model.State) error {
		for i := range s.Workloads {
			w := &s.Workloads[i]
			if w.ID != workloadID {
				continue
			}
			if w.StopRequested || w.Status != "running" {
				return errWorkloadStopped
			}
			w.Attempted++
			if err := transactionNodesAvailable(*s, tx); err != nil {
				if errors.Is(err, errNodeUnavailable) {
					w.SkippedUnavailable++
					return nil
				}
				return err
			}
			if transactionNodesBusy(s.Transactions, tx.FromNode, tx.ToNode) {
				w.SkippedBusy++
				return nil
			}
			w.Submitted++
			tx.Sequence = w.Submitted
			s.Transactions = append(s.Transactions, tx)
			queued = true
			return nil
		}
		return os.ErrNotExist
	})
	if err != nil && queued {
		_ = a.finishTx(tx.ID, "failed", "", fmt.Errorf("自动交易入队保存未完成，未执行 RPC：%w", err))
	}
	return queued && err == nil, err
}
