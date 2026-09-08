package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const attachModulesTimeoutFatal = "Fatal: Failed to start the JavaScript console: api modules: context deadline exceeded"

// PreExecutionAttachError is returned only after completed attach processes
// prove console initialization failed before --exec was evaluated. It must not
// be used for connection loss, outer cancellation, or generic RPC errors.
type PreExecutionAttachError struct {
	Err error
}

func (e *PreExecutionAttachError) Error() string {
	return fmt.Sprintf("attach console initialization failed before expression execution: %v", e.Err)
}

func (e *PreExecutionAttachError) Unwrap() error { return e.Err }

func isAttachModulesStartupTimeout(output string) bool {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(output, "\r\n", "\n")), "\n")
	// Fatalf writes to both stdout/stderr unless they share a file. Reject all
	// additional output, including hashes, script results and generic timeouts.
	if len(lines) < 1 || len(lines) > 2 {
		return false
	}
	for _, line := range lines {
		if line != attachModulesTimeoutFatal {
			return false
		}
	}
	return true
}

func attachWithStartupRetry(ctx context.Context, run func(context.Context) (string, error)) (string, error) {
	return attachWithStartupRetryWait(ctx, run, func(ctx context.Context) error {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	})
}

func attachWithStartupRetryWait(ctx context.Context, run func(context.Context) (string, error), wait func(context.Context) error) (string, error) {
	var output string
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return output, err
		}
		out, err := run(ctx)
		output = out
		if err == nil || ctx.Err() != nil || !isAttachModulesStartupTimeout(out) {
			return out, err
		}
		if attempt == 1 {
			return out, &PreExecutionAttachError{Err: err}
		}
		if err := wait(ctx); err != nil {
			return out, err
		}
	}
	panic("unreachable attach retry state")
}
