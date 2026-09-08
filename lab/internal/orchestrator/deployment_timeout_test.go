package orchestrator

import (
	"context"
	"errors"
	"github.com/pfap/lab/internal/model"
	"strings"
	"testing"
	"time"
)

func TestDeploymentBudgetScalesWithPlan(t *testing.T) {
	for _, n := range []int{1, 100, 300} {
		e := model.Experiment{}
		for i := 0; i < n; i++ {
			e.Placements = append(e.Placements, model.Placement{Count: 1})
		}
		if got, want := DeploymentTimeout(e), 15*time.Minute+time.Duration(n)*5*time.Minute; got != want {
			t.Fatalf("%d: %s != %s", n, got, want)
		}
	}
}
func TestDeploymentStepReportsDeadlineAndDoesNotRetry(t *testing.T) {
	calls := 0
	_, err := deploymentStep(context.Background(), "upload worker", time.Millisecond, func(ctx context.Context) (string, error) { calls++; <-ctx.Done(); return "", errors.New("ssh killed") })
	if calls != 1 || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "upload worker") {
		t.Fatal(calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = deploymentStep(ctx, "start", time.Hour, func(ctx context.Context) (string, error) { return "", ctx.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
