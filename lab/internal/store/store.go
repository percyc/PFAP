package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pfap/lab/internal/model"
)

type Health struct {
	Degraded    bool      `json:"degraded"`
	LastError   string    `json:"lastError,omitempty"`
	LastErrorAt time.Time `json:"lastErrorAt,omitempty"`
	LastSavedAt time.Time `json:"lastSavedAt,omitempty"`
	BackupPath  string    `json:"backupPath"`
}

// MarshalJSON omits unknown timestamps, which time.Time's omitempty tag alone
// does not do.
func (h Health) MarshalJSON() ([]byte, error) {
	type healthAlias Health
	var lastErrorAt, lastSavedAt *time.Time
	if !h.LastErrorAt.IsZero() {
		lastErrorAt = &h.LastErrorAt
	}
	if !h.LastSavedAt.IsZero() {
		lastSavedAt = &h.LastSavedAt
	}
	return json.Marshal(struct {
		healthAlias
		LastErrorAt *time.Time `json:"lastErrorAt,omitempty"`
		LastSavedAt *time.Time `json:"lastSavedAt,omitempty"`
	}{healthAlias(h), lastErrorAt, lastSavedAt})
}

type syncedFile interface {
	io.Writer
	Name() string
	Sync() error
	Close() error
}

// Operations are scoped to a store so failure tests do not alter global state.
type fileOperations struct {
	createTemp func(string, string) (syncedFile, error)
	rename     func(string, string) error
	syncDir    func(string) error
}

type Store struct {
	mu        sync.RWMutex
	path      string
	data      []byte // Immutable JSON: neither callbacks nor views can retain aliases.
	persisted bool
	health    Health
	files     fileOperations
}

func (s *Store) DataDir() string { return filepath.Dir(s.path) }

func (s *Store) Health() Health {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.health
}

func Open(path string) (*Store, error) {
	initial := model.State{Servers: []model.Server{}, Experiments: []model.Experiment{}, Events: []model.Event{}, Transactions: []model.Transaction{}, Workloads: []model.Workload{}, AccountSnapshots: []model.AccountSnapshot{}}
	data, err := json.Marshal(initial)
	if err != nil {
		return nil, err
	}
	s := &Store{
		path: path, data: data, health: Health{BackupPath: path + ".bak"},
		files: fileOperations{
			createTemp: func(dir, pattern string) (syncedFile, error) { return os.CreateTemp(dir, pattern) },
			rename:     os.Rename,
			syncDir:    syncDirectory,
		},
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &initial); err != nil {
		return nil, err
	}
	s.data, s.persisted = b, true
	return s, nil
}

func (s *Store) View(fn func(model.State)) {
	s.mu.RLock()
	b := s.data
	s.mu.RUnlock()
	var snapshot model.State
	// Every committed snapshot has already been validated as a model.State.
	if err := json.Unmarshal(b, &snapshot); err != nil {
		panic(fmt.Sprintf("invalid committed store snapshot: %v", err))
	}
	fn(snapshot)
}

func (s *Store) Update(fn func(*model.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next model.State
	if err := json.Unmarshal(s.data, &next); err != nil {
		return s.persistenceError(fmt.Errorf("copy store state: %w", err))
	}
	if err := fn(&next); err != nil {
		return err
	}
	if len(next.Events) > 5000 {
		next.Events = next.Events[len(next.Events)-5000:]
	}
	if len(next.AccountSnapshots) > 5000 {
		next.AccountSnapshots = next.AccountSnapshots[len(next.AccountSnapshots)-5000:]
	}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return s.persistenceError(fmt.Errorf("encode store state: %w", err))
	}
	// Custom JSON marshalers in event fields must not create a snapshot that
	// cannot be read back. Decoding also validates the committed representation.
	var validated model.State
	if err := json.Unmarshal(b, &validated); err != nil {
		return s.persistenceError(fmt.Errorf("validate store state: %w", err))
	}
	if err := s.save(b); err != nil {
		return s.persistenceError(err)
	}
	s.health.Degraded = false
	s.health.LastError = ""
	s.health.LastErrorAt = time.Time{}
	s.health.LastSavedAt = time.Now().UTC()
	return nil
}

func (s *Store) persistenceError(err error) error {
	s.health.Degraded = true
	s.health.LastError = err.Error()
	s.health.LastErrorAt = time.Now().UTC()
	return err
}

func (s *Store) save(b []byte) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create store directory: %w", err)
	}
	tmp, err := s.writeTemp(s.path, b)
	if err != nil {
		return fmt.Errorf("write store: %w", err)
	}
	defer os.Remove(tmp)
	if s.persisted {
		backup, err := s.writeTemp(s.health.BackupPath, s.data)
		if err != nil {
			return fmt.Errorf("write store backup: %w", err)
		}
		defer os.Remove(backup)
		if err := s.files.rename(backup, s.health.BackupPath); err != nil {
			return fmt.Errorf("replace store backup: %w", err)
		}
		// Make the previous primary durable before replacing it.
		if err := s.files.syncDir(dir); err != nil {
			return fmt.Errorf("sync store backup directory: %w", err)
		}
	}
	if err := s.files.rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace store: %w", err)
	}
	// Rename is the commit point. A later directory-sync error must not leave
	// memory behind the visible primary, even though durability is uncertain.
	s.data, s.persisted = b, true
	if err := s.files.syncDir(dir); err != nil {
		return fmt.Errorf("sync store directory after commit: %w", err)
	}
	return nil
}

func (s *Store) writeTemp(target string, b []byte) (name string, err error) {
	f, err := s.files.createTemp(filepath.Dir(target), "."+filepath.Base(target)+"-*.tmp")
	if err != nil {
		return "", err
	}
	name = f.Name()
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	n, err := f.Write(b)
	if err != nil {
		return name, err
	}
	if n != len(b) {
		return name, io.ErrShortWrite
	}
	if err = f.Sync(); err != nil {
		return name, err
	}
	closed = true
	if err = f.Close(); err != nil {
		return name, err
	}
	return name, nil
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
