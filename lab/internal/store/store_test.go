package store

import (
	"path/filepath"
	"testing"

	"github.com/pfap/lab/internal/model"
)

func TestStorePersistsAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lab.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(v *model.State) error {
		v.Servers = append(v.Servers, model.Server{ID: "s1", Name: "one"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened.View(func(v model.State) {
		if len(v.Servers) != 1 || v.Servers[0].ID != "s1" {
			t.Fatalf("unexpected state: %#v", v)
		}
	})
}

func TestViewReturnsCopy(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "x.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Update(func(v *model.State) error { v.Servers = append(v.Servers, model.Server{ID: "s1"}); return nil })
	s.View(func(v model.State) { v.Servers[0].ID = "changed" })
	s.View(func(v model.State) {
		if v.Servers[0].ID != "s1" {
			t.Fatal("view mutated store")
		}
	})
}
