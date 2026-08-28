package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/pfap/lab/internal/model"
)

type Store struct {
	mu   sync.RWMutex
	path string
	data model.State
}

func (s *Store) DataDir() string { return filepath.Dir(s.path) }

func Open(path string) (*Store, error) {
	s := &Store{path: path, data: model.State{Servers: []model.Server{}, Experiments: []model.Experiment{}, Events: []model.Event{}, Transactions: []model.Transaction{}, Workloads: []model.Workload{}, AccountSnapshots: []model.AccountSnapshot{}}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) View(fn func(model.State)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, _ := json.Marshal(s.data)
	var copy model.State
	_ = json.Unmarshal(b, &copy)
	fn(copy)
}

func (s *Store) Update(fn func(*model.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.data); err != nil {
		return err
	}
	if len(s.data.Events) > 5000 {
		s.data.Events = s.data.Events[len(s.data.Events)-5000:]
	}
	if len(s.data.AccountSnapshots) > 5000 {
		s.data.AccountSnapshots = s.data.AccountSnapshots[len(s.data.AccountSnapshots)-5000:]
	}
	return s.save()
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
