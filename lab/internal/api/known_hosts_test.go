package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAppendKnownHostKeysPreservesExistingKeys(t *testing.T) {
	a := &API{}
	path := filepath.Join(t.TempDir(), "ssh", "known_hosts")
	const first = "first.example ssh-ed25519 first-key\n"
	const second = "second.example ssh-ed25519 second-key\n"
	if err := a.appendKnownHostKeys(path, first); err != nil {
		t.Fatal(err)
	}
	if err := a.appendKnownHostKeys(path, second); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != first+second {
		t.Fatalf("existing keys were lost or changed: %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("known_hosts permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestAppendKnownHostKeysConcurrentAppends(t *testing.T) {
	a := &API{}
	path := filepath.Join(t.TempDir(), "known_hosts")
	const original = "original.example ssh-ed25519 original-key\n"
	if err := a.appendKnownHostKeys(path, original); err != nil {
		t.Fatal(err)
	}
	const callers = 32
	start := make(chan struct{})
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results <- a.appendKnownHostKeys(path, fmt.Sprintf("worker-%d ssh-ed25519 key-%d\n", i, i))
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), original) {
		t.Fatalf("concurrent trust changed original keys: %q", data)
	}
	for i := 0; i < callers; i++ {
		line := fmt.Sprintf("worker-%d ssh-ed25519 key-%d\n", i, i)
		if count := strings.Count(string(data), line); count != 1 {
			t.Errorf("saved key %q occurs %d times, want 1", line, count)
		}
	}
}

func TestWriteKnownHostsFailurePreservesOriginal(t *testing.T) {
	for _, failure := range []string{"partial write", "close"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "known_hosts")
			const original = "existing.example ssh-ed25519 trusted-key\n"
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			writeErr := errors.New("simulated disk-full partial write")
			err := writeKnownHostsAtomically(path, func(temp *os.File) error {
				if _, err := temp.WriteString("partial"); err != nil {
					return err
				}
				if failure == "partial write" {
					return writeErr
				}
				// Closing here makes the helper's final Close report an error.
				return temp.Close()
			})
			if err == nil || (failure == "partial write" && !errors.Is(err, writeErr)) {
				t.Fatalf("write failure was swallowed: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != original {
				t.Fatalf("failed replacement damaged original keys: %q", data)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "known_hosts" {
				t.Fatalf("failed replacement left temporary files: %v", entries)
			}
		})
	}
}
