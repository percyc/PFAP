package orchestrator

import (
	"context"
	"errors"
	"testing"
)

type fakeAttachResult struct {
	output string
	err    error
}

type fakeAttachRunner struct {
	results []fakeAttachResult
	calls   int
}

func (f *fakeAttachRunner) run(context.Context) (string, error) {
	result := f.results[f.calls]
	f.calls++
	return result.output, result.err
}

func TestAttachStartupRetryRequiresExactPreExecutionEvidence(t *testing.T) {
	failure := errors.New("exit status 1")
	for _, tc := range []struct {
		name    string
		results []fakeAttachResult
		calls   int
		typed   bool
	}{
		{"success", []fakeAttachResult{{"result", nil}}, 1, false},
		{"startup then success", []fakeAttachResult{{attachModulesTimeoutFatal, failure}, {"result", nil}}, 2, false},
		{"two startup failures", []fakeAttachResult{{attachModulesTimeoutFatal, failure}, {attachModulesTimeoutFatal, failure}}, 2, true},
		{"duplicate stdout stderr", []fakeAttachResult{{attachModulesTimeoutFatal + "\n" + attachModulesTimeoutFatal + "\n", failure}, {attachModulesTimeoutFatal, failure}}, 2, true},
		{"rpc failure", []fakeAttachResult{{"Error: context deadline exceeded", failure}}, 1, false},
		{"other startup failure", []fakeAttachResult{{"Fatal: Failed to start the JavaScript console: namespace flattening: error", failure}}, 1, false},
		{"transport failure", []fakeAttachResult{{"ssh: connection lost", failure}}, 1, false},
		{"empty timeout", []fakeAttachResult{{"", context.DeadlineExceeded}}, 1, false},
		{"extra hash", []fakeAttachResult{{attachModulesTimeoutFatal + "\n0x0123456789abcdef", failure}}, 1, false},
		{"extra script output", []fakeAttachResult{{"generated proof\n" + attachModulesTimeoutFatal, failure}}, 1, false},
		{"extra generic timeout", []fakeAttachResult{{attachModulesTimeoutFatal + "\ncontext deadline exceeded", failure}}, 1, false},
		{"fatal with successful exit", []fakeAttachResult{{attachModulesTimeoutFatal, nil}}, 1, false},
		{"retry transport failure", []fakeAttachResult{{attachModulesTimeoutFatal, failure}, {"connection lost", failure}}, 2, false},
		{"retry uncertain execution", []fakeAttachResult{{attachModulesTimeoutFatal, failure}, {"proof generated; connection lost", failure}}, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := fakeAttachRunner{results: tc.results}
			waits := 0
			output, err := attachWithStartupRetryWait(context.Background(), runner.run, func(context.Context) error { waits++; return nil })
			var preExecution *PreExecutionAttachError
			if runner.calls != tc.calls || waits != tc.calls-1 || errors.As(err, &preExecution) != tc.typed {
				t.Fatalf("calls=%d waits=%d typed=%t err=%v", runner.calls, waits, preExecution != nil, err)
			}
			last := tc.results[tc.calls-1]
			if output != last.output || !errors.Is(err, last.err) {
				t.Fatal("final attempt output or error cause was lost")
			}
		})
	}
}

func TestAttachStartupRetryNeverRetriesCancellation(t *testing.T) {
	for _, when := range []string{"before attempt", "during attempt", "during wait"} {
		t.Run(when, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			if when == "before attempt" {
				cancel()
			}
			_, err := attachWithStartupRetryWait(ctx, func(context.Context) (string, error) {
				calls++
				if when == "during attempt" {
					cancel()
				}
				return attachModulesTimeoutFatal, errors.New("exit status 1")
			}, func(ctx context.Context) error {
				cancel()
				return ctx.Err()
			})
			want := 1
			if when == "before attempt" {
				want = 0
			}
			var preExecution *PreExecutionAttachError
			if calls != want || err == nil || errors.As(err, &preExecution) {
				t.Fatalf("cancelled attach calls=%d err=%v", calls, err)
			}
		})
	}
}
