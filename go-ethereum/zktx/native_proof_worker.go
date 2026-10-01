package zktx

import (
	"runtime"
	"sync"
)

var proofWorker struct {
	sync.Once
	jobs chan func()
}

// Keep large native proving allocations on one OS thread. Serial account
// admission alone is not enough: successive Go goroutines may execute C calls
// on different threads, each retaining its own allocator arena. Verification
// is deliberately not routed here and can continue during proof generation.
func runNativeProof(fn func()) {
	proofWorker.Do(func() {
		proofWorker.jobs = make(chan func())
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			for job := range proofWorker.jobs {
				job()
			}
		}()
	})
	done := make(chan struct{})
	var panicValue interface{}
	completed := false
	proofWorker.jobs <- func() {
		defer func() { panicValue = recover(); close(done) }()
		fn()
		completed = true
	}
	<-done
	if !completed {
		panic(panicValue)
	}
}
