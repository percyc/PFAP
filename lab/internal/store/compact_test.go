package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pfap/lab/internal/model"
)

func TestCompactSnapshotRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(v *model.State) error {
		v.Servers = []model.Server{{ID: "server", Name: "line one\nline two"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, compact.Bytes()) {
		t.Fatal("snapshot contains formatting overhead")
	}
	reopened, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	reopened.View(func(v model.State) {
		if len(v.Servers) != 1 || v.Servers[0].Name != "line one\nline two" {
			t.Fatal("snapshot content changed")
		}
	})
}
