package api

import (
	"errors"
	"time"

	"github.com/pfap/lab/internal/model"
)

// Minimum stabilization time and maximum time to cover every account are
// separate controls. A large mixed run must warm both broadcast paths, not
// merely wait out the minimum. This never changes its measurement duration.
func configureWarmupTimeout(w *model.Workload) error {
	if w.WarmupTimeoutSeconds == 0 {
		extra := 1800
		if w.Type == "mixed" {
			extra = max(extra, len(w.NodeIDs)*60)
		}
		w.WarmupTimeoutSeconds = w.WarmupSeconds + extra
	}
	if w.WarmupTimeoutSeconds < w.WarmupSeconds || w.WarmupTimeoutSeconds > 86400 {
		return errors.New("预热超时上限不能小于最短预热时间，且不能超过 24 小时")
	}
	return nil
}

func runWarmupTimeout(w model.Workload) time.Duration {
	seconds := w.WarmupTimeoutSeconds
	if seconds == 0 {
		// Preserve the meaning of pre-existing persisted runs.
		seconds = w.WarmupSeconds + 1800
	}
	return time.Duration(seconds) * time.Second
}
