package store

import (
	"errors"

	"github.com/pfap/lab/internal/model"
)

const coalescedBatchLimit = 32

type coalescedUpdate struct {
	apply func(*model.State) error
	done  chan error
}

var errNoCoalescedChanges = errors.New("no accepted coalesced changes")

// UpdateCoalesced is for high-frequency, in-memory state mutations. Callbacks
// must validate their inputs, contain no I/O, and only insert JSON-compatible
// values. Concurrent callers may share one durable commit, but NONE return
// success before that commit. A rejected callback's mutations are discarded;
// encoding or persistence failure rejects every otherwise accepted member.
// Use Update for operations requiring a separate durable boundary.
func (s *Store) UpdateCoalesced(fn func(*model.State) error) error {
	r := &coalescedUpdate{apply: fn, done: make(chan error, 1)}
	s.coalescedMu.Lock()
	s.coalescedQueue = append(s.coalescedQueue, r)
	if !s.coalescedFlushing {
		s.coalescedFlushing = true
		go s.flushCoalesced()
	}
	s.coalescedMu.Unlock()
	return <-r.done
}

func (s *Store) flushCoalesced() {
	for {
		s.coalescedMu.Lock()
		if len(s.coalescedQueue) == 0 {
			s.coalescedFlushing = false
			s.coalescedMu.Unlock()
			return
		}
		n := min(coalescedBatchLimit, len(s.coalescedQueue))
		batch := append([]*coalescedUpdate(nil), s.coalescedQueue[:n]...)
		clear(s.coalescedQueue[:n])
		s.coalescedQueue = s.coalescedQueue[n:]
		s.coalescedMu.Unlock()
		s.commitCoalesced(batch)
	}
}

func (s *Store) commitCoalesced(batch []*coalescedUpdate) {
	errs := make([]error, len(batch))
	err := s.Update(func(state *model.State) error {
		accepted := 0
		for i, r := range batch {
			// Isolate a callback that mutates and then rejects its operation.
			next := cloneSnapshot(*state)
			if errs[i] = applyCoalesced(r.apply, &next); errs[i] != nil {
				continue
			}
			*state = next
			accepted++
		}
		if accepted == 0 {
			return errNoCoalescedChanges
		}
		return nil
	})
	for i, r := range batch {
		if errs[i] == nil {
			errs[i] = err
		}
		r.done <- errs[i]
	}
}

func applyCoalesced(fn func(*model.State) error, state *model.State) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("coalesced state callback panicked; changes discarded")
		}
	}()
	return fn(state)
}
