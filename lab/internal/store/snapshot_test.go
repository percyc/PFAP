package store

import (
	"encoding/json"
	"github.com/pfap/lab/internal/model"
	"os"
	"testing"
	"time"
)

func BenchmarkLiveSnapshot(b *testing.B) {
	path := os.Getenv("PFAP_BENCH_STATE")
	if path == "" {
		b.Skip("set PFAP_BENCH_STATE for read-only live-state benchmark")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	var state model.State
	if err = json.Unmarshal(data, &state); err != nil {
		b.Fatal(err)
	}
	b.Run("JSON", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var v model.State
			if err := json.Unmarshal(data, &v); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Clone", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = cloneSnapshot(state)
		}
	})
	b.Run("DurableUpdate", func(b *testing.B) {
		s, err := Open(b.TempDir() + "/state.json")
		if err != nil {
			b.Fatal(err)
		}
		if err := s.Update(func(v *model.State) error { *v = state; return nil }); err != nil {
			b.Fatal(err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := s.Update(func(*model.State) error { return nil }); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Coalesced32", func(b *testing.B) {
		s, err := Open(b.TempDir() + "/state.json")
		if err != nil {
			b.Fatal(err)
		}
		if err := s.Update(func(v *model.State) error { *v = state; return nil }); err != nil {
			b.Fatal(err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			batch := requests(32, func(*model.State) error { return nil })
			s.commitCoalesced(batch)
			for _, r := range batch {
				if err := <-r.done; err != nil {
					b.Fatal(err)
				}
			}
		}
	})
}

func TestCloneSnapshotNestedContainers(t *testing.T) {
	value := true
	source := model.State{Experiments: []model.Experiment{{Nodes: []model.Node{{Mining: &value}}}}, Events: []model.Event{{Fields: map[string]any{"nested": []any{map[string]any{"value": "original"}}}}}}
	copied := cloneSnapshot(source)
	*copied.Experiments[0].Nodes[0].Mining = false
	copied.Events[0].Fields["nested"].([]any)[0].(map[string]any)["value"] = "changed"
	if !*source.Experiments[0].Nodes[0].Mining || source.Events[0].Fields["nested"].([]any)[0].(map[string]any)["value"] != "original" {
		t.Fatal("mutable aliases escaped snapshot")
	}
}

func TestValueSliceCloneStillIsolatesRecords(t *testing.T) {
	source := model.State{Transactions: []model.Transaction{{ID: "old"}}, AccountSnapshots: []model.AccountSnapshot{{ID: "snapshot"}}}
	copy := cloneSnapshot(source)
	copy.Transactions[0].ID = "new"
	copy.AccountSnapshots[0].ID = "changed"
	if source.Transactions[0].ID != "old" || source.AccountSnapshots[0].ID != "snapshot" {
		t.Fatal("value slice backing array shared")
	}
	if cloneSnapshot(model.State{}).Transactions != nil {
		t.Fatal("nil slice changed")
	}
}

func TestViewReadsCommittedStateDuringPendingUpdate(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(v *model.State) error { v.Transactions = []model.Transaction{{ID: "old"}}; return nil }); err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.Update(func(v *model.State) error {
			v.Transactions[0].ID = "new"
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	read := make(chan string, 1)
	go s.View(func(v model.State) { read <- v.Transactions[0].ID; v.Transactions[0].ID = "reader mutation" })
	select {
	case id := <-read:
		if id != "old" {
			t.Error("uncommitted update exposed")
		}
	case <-time.After(time.Second):
		t.Error("read waited for pending write")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s.View(func(v model.State) {
		if v.Transactions[0].ID != "new" {
			t.Fatal("new commit not published")
		}
	})
}
