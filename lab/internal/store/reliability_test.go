package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pfap/lab/internal/model"
)

func seedStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(state *model.State) error {
		mining := true
		state.Servers = []model.Server{{ID: "original", Labels: []string{"original"}}}
		state.Experiments = []model.Experiment{{Nodes: []model.Node{{Mining: &mining}}}}
		state.Events = []model.Event{{Fields: map[string]any{"nested": map[string]any{"key": "original"}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertStateID(t *testing.T, s *Store, id string) {
	t.Helper()
	s.View(func(state model.State) {
		if len(state.Servers) != 1 || state.Servers[0].ID != id {
			t.Errorf("state servers = %#v; want ID %q", state.Servers, id)
		}
	})
	reopened, err := Open(s.path)
	if err != nil {
		t.Fatal(err)
	}
	reopened.View(func(state model.State) {
		if len(state.Servers) != 1 || state.Servers[0].ID != id {
			t.Errorf("persisted servers = %#v; want ID %q", state.Servers, id)
		}
	})
}

func TestUpdateValidationAndMarshalRollback(t *testing.T) {
	s := seedStore(t)
	original := readBytes(t, s.path)
	health := s.Health()
	validationErr := errors.New("invalid request")
	err := s.Update(func(state *model.State) error {
		state.Servers[0].Labels[0] = "mutated"
		*state.Experiments[0].Nodes[0].Mining = false
		state.Events[0].Fields["nested"].(map[string]any)["key"] = "mutated"
		return validationErr
	})
	if !errors.Is(err, validationErr) || s.Health() != health {
		t.Fatalf("validation error = %v, health = %#v", err, s.Health())
	}
	s.View(func(state model.State) {
		if state.Servers[0].Labels[0] != "original" || !*state.Experiments[0].Nodes[0].Mining || state.Events[0].Fields["nested"].(map[string]any)["key"] != "original" {
			t.Fatal("failed callback mutated nested state")
		}
	})
	err = s.Update(func(state *model.State) error {
		state.Servers[0].ID = "mutated"
		state.Events[0].Fields["unsupported"] = func() {}
		return nil
	})
	if err == nil {
		t.Fatal("unsupported value was saved")
	}
	assertStateID(t, s, "original")
	if !bytes.Equal(original, readBytes(t, s.path)) {
		t.Fatal("failed update changed primary")
	}
	failedHealth := s.Health()
	if !failedHealth.Degraded || failedHealth.LastError == "" || failedHealth.LastErrorAt.IsZero() || failedHealth.LastSavedAt != health.LastSavedAt {
		t.Fatalf("missing persistence failure health: %#v", failedHealth)
	}
	_ = s.Update(func(*model.State) error { return validationErr })
	if s.Health() != failedHealth {
		t.Fatal("validation failure overwrote persistence health")
	}
	if err := s.Update(func(*model.State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if h := s.Health(); h.Degraded || h.LastError != "" || !h.LastErrorAt.IsZero() || h.LastSavedAt.Before(health.LastSavedAt) {
		t.Fatalf("successful save did not recover health: %#v", h)
	}
}

func TestUpdateAndViewDoNotExposeAliases(t *testing.T) {
	s := seedStore(t)
	var escaped *model.State
	if err := s.Update(func(state *model.State) error { escaped = state; return nil }); err != nil {
		t.Fatal(err)
	}
	mutate := func(state *model.State) {
		state.Servers[0].Labels[0] = "mutated"
		*state.Experiments[0].Nodes[0].Mining = false
		state.Events[0].Fields["nested"].(map[string]any)["key"] = "mutated"
	}
	mutate(escaped)
	s.View(func(state model.State) {
		if state.Servers[0].Labels[0] != "original" || !*state.Experiments[0].Nodes[0].Mining || state.Events[0].Fields["nested"].(map[string]any)["key"] != "original" {
			t.Fatal("escaped update callback mutated state")
		}
		mutate(&state)
	})
	s.View(func(state model.State) {
		if state.Servers[0].Labels[0] != "original" || !*state.Experiments[0].Nodes[0].Mining || state.Events[0].Fields["nested"].(map[string]any)["key"] != "original" {
			t.Fatal("view mutated nested state")
		}
	})
}

type failingFile struct {
	syncedFile
	stage string
	err   error
}

func (f failingFile) Write(b []byte) (int, error) {
	if f.stage == "write" {
		_, _ = f.syncedFile.Write(b[:len(b)/2])
		return len(b) / 2, f.err
	}
	if f.stage == "short-write" {
		return f.syncedFile.Write(b[:len(b)/2])
	}
	return f.syncedFile.Write(b)
}

func (f failingFile) Sync() error {
	if f.stage == "sync" {
		return f.err
	}
	return f.syncedFile.Sync()
}

func (f failingFile) Close() error {
	err := f.syncedFile.Close()
	if f.stage == "close" {
		return f.err
	}
	return err
}

func TestPersistenceFailuresPreservePrimaryAndMemory(t *testing.T) {
	for _, target := range []string{"primary", "backup"} {
		for _, stage := range []string{"create", "write", "short-write", "sync", "close", "rename", "directory-sync"} {
			if target == "primary" && stage == "directory-sync" {
				continue // A primary directory-sync failure occurs after commit.
			}
			t.Run(target+"/"+stage, func(t *testing.T) {
				s := seedStore(t)
				original := readBytes(t, s.path)
				originalHealth := s.Health()
				originalFiles := s.files
				injected := errors.New("injected persistence failure")
				s.files.createTemp = func(dir, pattern string) (syncedFile, error) {
					isBackup := strings.Contains(pattern, ".bak-")
					fail := isBackup == (target == "backup")
					if fail && stage == "create" {
						return nil, injected
					}
					f, err := originalFiles.createTemp(dir, pattern)
					if err != nil || !fail {
						return f, err
					}
					return failingFile{f, stage, injected}, nil
				}
				s.files.rename = func(from, to string) error {
					if stage == "rename" && (to == s.health.BackupPath) == (target == "backup") {
						return injected
					}
					return originalFiles.rename(from, to)
				}
				if stage == "directory-sync" {
					s.files.syncDir = func(string) error { return injected }
				}
				err := s.Update(func(state *model.State) error { state.Servers[0].ID = "new"; return nil })
				wantErr := injected
				if stage == "short-write" {
					wantErr = io.ErrShortWrite
				}
				if !errors.Is(err, wantErr) {
					t.Fatalf("update error = %v; want %v", err, wantErr)
				}
				assertStateID(t, s, "original")
				if !bytes.Equal(original, readBytes(t, s.path)) {
					t.Fatal("failure changed primary bytes")
				}
				if h := s.Health(); !h.Degraded || h.LastError == "" || h.LastErrorAt.IsZero() || h.LastSavedAt != originalHealth.LastSavedAt {
					t.Fatalf("unexpected failure health: %#v", h)
				}
				entries, err := os.ReadDir(s.DataDir())
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if strings.HasSuffix(entry.Name(), ".tmp") {
						t.Errorf("temporary file leaked: %s", entry.Name())
					}
				}
			})
		}
	}
}

func TestDirectorySyncFailureKeepsCommittedMemory(t *testing.T) {
	s := seedStore(t)
	original := readBytes(t, s.path)
	originalFiles := s.files
	injected := errors.New("directory sync failed")
	calls := 0
	s.files.syncDir = func(dir string) error {
		calls++
		if calls == 2 {
			return injected
		}
		return originalFiles.syncDir(dir)
	}
	err := s.Update(func(state *model.State) error { state.Servers[0].ID = "committed"; return nil })
	if !errors.Is(err, injected) || !s.Health().Degraded {
		t.Fatalf("error = %v, health = %#v", err, s.Health())
	}
	assertStateID(t, s, "committed")
	if !bytes.Equal(original, readBytes(t, s.Health().BackupPath)) {
		t.Fatal("backup is not the previous primary")
	}
	committed := readBytes(t, s.path)
	s.files = originalFiles
	if err := s.Update(func(state *model.State) error { state.Servers[0].ID = "recovered"; return nil }); err != nil {
		t.Fatal(err)
	}
	if s.Health().Degraded || !bytes.Equal(committed, readBytes(t, s.Health().BackupPath)) {
		t.Fatal("recovery lost the committed state or retained degradation")
	}
}

func TestBackupUsesExactPreviousPrimaryAndUniqueTemporaryFiles(t *testing.T) {
	s := seedStore(t)
	if _, err := os.Stat(s.Health().BackupPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first save created a backup without an earlier primary: %v", err)
	}
	legacyTemp := s.path + ".tmp"
	if err := os.WriteFile(legacyTemp, []byte("unrelated file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		previous := readBytes(t, s.path)
		reopened, err := Open(s.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := reopened.Update(func(state *model.State) error { state.Servers[0].ID = fmt.Sprint(i); return nil }); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(previous, readBytes(t, s.Health().BackupPath)) {
			t.Fatal("backup does not match previous primary bytes")
		}
		for _, path := range []string{s.path, s.Health().BackupPath} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("private file permissions for %s: %v, %v", path, info, err)
			}
		}
	}
	if string(readBytes(t, legacyTemp)) != "unrelated file" {
		t.Fatal("store reused an existing temporary filename")
	}
}

func TestOpenDoesNotRestoreCorruptPrimary(t *testing.T) {
	s := seedStore(t)
	if err := s.Update(func(*model.State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	backup := readBytes(t, s.Health().BackupPath)
	if err := os.WriteFile(s.path, []byte("corrupt primary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(s.path); err == nil {
		t.Fatal("corrupt primary was silently restored")
	}
	if string(readBytes(t, s.path)) != "corrupt primary" || !bytes.Equal(backup, readBytes(t, s.Health().BackupPath)) {
		t.Fatal("open modified primary or backup")
	}
}

func TestHealthJSONOmitsUnknownTimes(t *testing.T) {
	b, err := json.Marshal(Health{BackupPath: "state.json.bak"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || fields["degraded"] != false || fields["backupPath"] != "state.json.bak" {
		t.Fatalf("unexpected health JSON: %s", b)
	}
}

func TestConcurrentUpdatesViewsAndHealth(t *testing.T) {
	s := seedStore(t)
	const writers, updates = 8, 8
	var group sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		group.Add(1)
		go func(writer int) {
			defer group.Done()
			for i := 0; i < updates; i++ {
				if err := s.Update(func(state *model.State) error {
					state.Transactions = append(state.Transactions, model.Transaction{ID: fmt.Sprintf("%d-%d", writer, i)})
					return nil
				}); err != nil {
					t.Errorf("concurrent update: %v", err)
				}
			}
		}(writer)
	}
	for reader := 0; reader < 4; reader++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for i := 0; i < 100; i++ {
				s.View(func(state model.State) { state.Servers[0].Labels[0] = "reader mutation" })
				if s.Health().Degraded {
					t.Error("healthy concurrent store reported degradation")
				}
				b, err := os.ReadFile(s.path)
				if err != nil || !json.Valid(b) {
					t.Errorf("reader observed invalid primary: %v", err)
				}
			}
		}()
	}
	group.Wait()
	s.View(func(state model.State) {
		if len(state.Transactions) != writers*updates || state.Servers[0].Labels[0] != "original" {
			t.Fatalf("updates lost or view mutation escaped: %d transactions, labels %v", len(state.Transactions), state.Servers[0].Labels)
		}
	})
	assertStateID(t, s, "original")
}
