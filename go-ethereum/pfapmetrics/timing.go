// Package pfapmetrics records node-local research timings, never consensus state.
package pfapmetrics

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

const limit = 32768

var mu sync.Mutex
var broadcasts = make(map[string]time.Time)
var admissions = make(map[string]bool)
var headers = make(map[string]time.Duration)
var session = fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

func emit(v map[string]interface{}) {
	v["session"] = session
	b, _ := json.Marshal(v)
	fmt.Printf("PFAP_MEASURE %s\n", b)
}

// Broadcast records the start of the earliest successful outbound peer send.
// start retains Go's monotonic clock. No proof or account material is recorded.
func Broadcast(hash string, start time.Time) {
	mu.Lock()
	defer mu.Unlock()
	if old, ok := broadcasts[hash]; ok && !start.Before(old) {
		return
	}
	if len(broadcasts) >= limit {
		emit(map[string]interface{}{"kind": "overflow", "scope": "broadcast"})
		return
	}
	broadcasts[hash] = start
	emit(map[string]interface{}{"kind": "broadcast", "hash": hash, "unixNs": start.UnixNano()})
}

// Included must be called immediately after the local canonical head is updated.
// Repeated/reorg observations remain explicit; the exporter checks final hashes.
func Included(hash, block string, at time.Time) {
	mu.Lock()
	defer mu.Unlock()
	if start, ok := broadcasts[hash]; ok && !at.Before(start) {
		emit(map[string]interface{}{"kind": "inclusion", "hash": hash, "block": block, "unixNs": at.UnixNano(), "ns": at.Sub(start).Nanoseconds()})
	}
}

func Admission(hash string, started, finished time.Time) {
	mu.Lock()
	defer mu.Unlock()
	if admissions[hash] {
		return
	}
	if len(admissions) >= limit {
		emit(map[string]interface{}{"kind": "overflow", "scope": "admission"})
		return
	}
	admissions[hash] = true
	emit(map[string]interface{}{"kind": "admission", "hash": hash, "ns": finished.Sub(started).Nanoseconds()})
}

func Header(hash string, elapsed time.Duration) {
	mu.Lock()
	defer mu.Unlock()
	if len(headers) >= limit {
		emit(map[string]interface{}{"kind": "overflow", "scope": "headers"})
		return
	}
	headers[hash] = elapsed
}

// BlockValidation sums active header, body and execution/state phases. Header
// work runs in a parallel worker, so queue wait must not be measured as work.
// Header/body include PoW, uncles and necessary reads; commit is excluded.
func BlockValidation(hash string, body, execution time.Duration) {
	mu.Lock()
	defer mu.Unlock()
	h, ok := headers[hash]
	if !ok {
		emit(map[string]interface{}{"kind": "block-missing-header", "hash": hash})
		return
	}
	emit(map[string]interface{}{"kind": "block-validation", "hash": hash, "headerNs": h.Nanoseconds(), "bodyNs": body.Nanoseconds(), "executionNs": execution.Nanoseconds(), "ns": (h + body + execution).Nanoseconds()})
}
