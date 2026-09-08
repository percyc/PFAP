package api

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestStopWaitsForMaintenanceAndThenAdmits(t *testing.T) {
	a := hostGroupTestAPI(t)
	a.lifecycleMu.Lock()
	done := make(chan error, 1)
	go func() { _, _, _, err := a.beginLifecycle("exp", "stop"); done <- err }()
	deadline := time.After(time.Second)
	for a.stopWaiters.Load() == 0 {
		select {
		case <-deadline:
			a.lifecycleMu.Unlock()
			t.Fatal("stop did not queue")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case err := <-done:
		a.lifecycleMu.Unlock()
		t.Fatalf("stop returned during maintenance: %v", err)
	default:
	}
	if recoveryTestState(a).Experiments[0].Status != "running" {
		a.lifecycleMu.Unlock()
		t.Fatal("stop bypassed maintenance")
	}
	a.lifecycleMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not resume")
	}
	if recoveryTestState(a).Experiments[0].Status != "stopping" || a.stopWaiters.Load() != 0 {
		t.Fatal("stop not admitted cleanly")
	}
}

func TestDeployWaitsForMaintenanceBeforeCheckingState(t *testing.T) {
	a := hostGroupTestAPI(t)
	a.lifecycleMu.Lock()
	done := make(chan error, 1)
	go func() { _, _, _, err := a.beginLifecycle("missing", "deploy"); done <- err }()
	deadline := time.After(time.Second)
	for a.stopWaiters.Load() == 0 {
		select {
		case <-deadline:
			a.lifecycleMu.Unlock()
			t.Fatal("deployment did not queue")
		case <-time.After(time.Millisecond):
		}
	}
	if a.tryLogRotationAdmission() {
		a.lifecycleMu.Unlock()
		t.Fatal("rotation overtook deployment")
	}
	a.lifecycleMu.Unlock()
	select {
	case err := <-done:
		if err == nil || err == errLifecycleBusy {
			t.Fatalf("admission not retried: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("deployment did not resume")
	}
}

func TestStopTimeoutPreservesMaintenanceOwnerAndState(t *testing.T) {
	a := hostGroupTestAPI(t)
	before := recoveryTestState(a)
	a.lifecycleMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := a.waitForStopAdmission(ctx); err == nil {
		t.Fatal("expected timeout")
	}
	if a.lifecycleMu.TryLock() {
		a.lifecycleMu.Unlock()
		t.Fatal("released another owner's lock")
	}
	a.lifecycleMu.Unlock()
	if a.stopWaiters.Load() != 0 || !reflect.DeepEqual(before, recoveryTestState(a)) {
		t.Fatal("timeout changed state")
	}
	if !a.tryLogRotationAdmission() {
		t.Fatal("rotation not restored")
	}
	a.lifecycleMu.Unlock()
}

func TestQueuedStopPreventsNewBackgroundRotation(t *testing.T) {
	a := &API{}
	a.stopWaiters.Add(1)
	if a.tryLogRotationAdmission() {
		a.lifecycleMu.Unlock()
		t.Fatal("background maintenance jumped queued stop")
	}
	if !a.lifecycleMu.TryLock() {
		t.Fatal("rejected rotation leaked lock")
	}
	a.lifecycleMu.Unlock()
	a.stopWaiters.Add(-1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if a.waitForStopAdmission(ctx) == nil {
		a.lifecycleMu.Unlock()
		t.Fatal("cancelled stop acquired lock")
	}
}
