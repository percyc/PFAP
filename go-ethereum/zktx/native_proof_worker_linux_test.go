package zktx

import (
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestNativeProofWorkerThreadAndSerialization(t *testing.T) {
	var wg sync.WaitGroup
	var active int32
	ids := make(chan int, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runNativeProof(func() {
				if atomic.AddInt32(&active, 1) != 1 {
					t.Error("overlapping native proofs")
				}
				ids <- syscall.Gettid()
				atomic.AddInt32(&active, -1)
			})
		}()
	}
	wg.Wait()
	close(ids)
	thread := 0
	for id := range ids {
		if thread == 0 {
			thread = id
		}
		if thread != id {
			t.Fatal("native proving moved to another OS thread")
		}
	}
}

func TestNativeProofWorkerPreservesPanicAndSurvives(t *testing.T) {
	func() {
		defer func() {
			if recover() != "test panic" {
				t.Error("caller lost native job panic")
			}
		}()
		runNativeProof(func() { panic("test panic") })
	}()
	called := false
	runNativeProof(func() { called = true })
	if !called {
		t.Fatal("worker stopped after a panic")
	}
}
