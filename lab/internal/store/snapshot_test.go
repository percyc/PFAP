package store

import (
	"encoding/json"
	"github.com/pfap/lab/internal/model"
	"os"
	"testing"
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
