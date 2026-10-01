package store

import (
	"errors"
	"sync"
	"testing"

	"github.com/pfap/lab/internal/model"
)

func requests(n int, fn func(*model.State) error) []*coalescedUpdate {
	out := make([]*coalescedUpdate, n)
	for i := range out {
		out[i] = &coalescedUpdate{apply: fn, done: make(chan error, 1)}
	}
	return out
}

func TestCoalescedOneCommitAndRejectedCallbackIsolation(t *testing.T) {
	s := seedStore(t)
	rename, commits := s.files.rename, 0
	s.files.rename = func(a, b string) error {
		if b == s.path {
			commits++
		}
		return rename(a, b)
	}
	batch := requests(3, func(v *model.State) error { v.Servers[0].Labels = append(v.Servers[0].Labels, "accepted"); return nil })
	rejected := errors.New("rejected")
	batch[1].apply = func(v *model.State) error { v.Servers[0].ID = "bad"; v.Servers[0].Labels[0] = "bad"; return rejected }
	s.commitCoalesced(batch)
	for i, r := range batch {
		err := <-r.done
		if (i == 1 && !errors.Is(err, rejected)) || (i != 1 && err != nil) {
			t.Fatal(i, err)
		}
	}
	if commits != 1 {
		t.Fatal("not one durable primary commit", commits)
	}
	s.View(func(v model.State) {
		if v.Servers[0].ID != "original" || len(v.Servers[0].Labels) != 3 || v.Servers[0].Labels[0] != "original" {
			t.Fatal("rejected changes escaped")
		}
	})
	assertStateID(t, s, "original")
}

func TestCoalescedNoSuccessBeforeDurableCommit(t *testing.T) {
	s := seedStore(t)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	rename := s.files.rename
	s.files.rename = func(a, b string) error {
		if b == s.path {
			close(entered)
			<-release
		}
		return rename(a, b)
	}
	batch := requests(4, func(v *model.State) error { v.Servers[0].ID = "committed"; return nil })
	go func() { s.commitCoalesced(batch); close(finished) }()
	<-entered
	for _, r := range batch {
		select {
		case <-r.done:
			t.Error("acknowledged before commit")
		default:
		}
	}
	close(release)
	<-finished
	for _, r := range batch {
		if err := <-r.done; err != nil {
			t.Fatal(err)
		}
	}
	assertStateID(t, s, "committed")
}

func TestCoalescedPanickingCallbackDoesNotStrandCallers(t *testing.T) {
	s := seedStore(t)
	batch := requests(2, func(v *model.State) error { v.Servers[0].ID = "accepted"; return nil })
	batch[0].apply = func(v *model.State) error { v.Servers[0].ID = "bad"; panic("test") }
	s.commitCoalesced(batch)
	if <-batch[0].done == nil || <-batch[1].done != nil {
		t.Fatal("panic isolation")
	}
	assertStateID(t, s, "accepted")
}

func TestCoalescedPersistenceOrEncodingFailureRejectsWholeBatch(t *testing.T) {
	for _, kind := range []string{"disk", "encoding"} {
		t.Run(kind, func(t *testing.T) {
			s := seedStore(t)
			batch := requests(4, func(v *model.State) error { v.Servers[0].ID = "uncommitted"; return nil })
			if kind == "disk" {
				rename := s.files.rename
				s.files.rename = func(a, b string) error {
					if b == s.path {
						return errors.New("disk fault")
					}
					return rename(a, b)
				}
			} else {
				batch[3].apply = func(v *model.State) error { v.Events[0].Fields["invalid"] = func() {}; return nil }
			}
			s.commitCoalesced(batch)
			for _, r := range batch {
				if err := <-r.done; err == nil {
					t.Fatal("failed batch reported success")
				}
			}
			assertStateID(t, s, "original")
		})
	}
}

func TestConcurrentCoalescedAndSeparateUpdatesPreserveAllChanges(t *testing.T) {
	s := seedStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fn := func(v *model.State) error { v.Servers[0].Labels = append(v.Servers[0].Labels, "entry"); return nil }
			var err error
			if i%4 == 0 {
				err = s.Update(fn)
			} else {
				err = s.UpdateCoalesced(fn)
			}
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	s.View(func(v model.State) {
		if len(v.Servers[0].Labels) != 129 {
			t.Fatal("lost changes", len(v.Servers[0].Labels))
		}
	})
}
