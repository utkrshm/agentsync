// Package retry stores durable retry state for asynchronous sync operations.
package retry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	OperationPush   = "push"
	OperationImport = "import"
	maxDelay        = time.Hour
)

type Item struct {
	Key         string    `json:"key"`
	Operation   string    `json:"operation"`
	Attempts    int       `json:"attempts"`
	NextAttempt time.Time `json:"next_attempt"`
	LastError   string    `json:"last_error,omitempty"`
}

type stateFile struct {
	SchemaVersion int             `json:"schema_version"`
	Items         map[string]Item `json:"items"`
}

type Store struct{ path string }

func DefaultPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent-sync", "retry-state.json"), nil
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return &Store{path: path}, nil
}

func (s *Store) Schedule(operation, key, message string) error {
	return s.ScheduleAt(time.Now().UTC(), operation, key, message)
}

func (s *Store) ScheduleAt(now time.Time, operation, key, message string) error {
	st, err := s.load()
	if err != nil {
		return err
	}
	item := st.Items[operation+"\x00"+key]
	item.Operation = operation
	item.Key = key
	item.Attempts++
	item.NextAttempt = now.Add(backoff(item.Attempts))
	item.LastError = message
	st.Items[operation+"\x00"+key] = item
	return s.save(st)
}

func (s *Store) Complete(operation, key string) error {
	st, err := s.load()
	if err != nil {
		return err
	}
	delete(st.Items, operation+"\x00"+key)
	return s.save(st)
}

func (s *Store) Due(now time.Time) ([]Item, error) {
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0)
	for _, item := range st.Items {
		if !item.NextAttempt.After(now) {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NextAttempt.Before(out[j].NextAttempt) })
	return out, nil
}

func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Second
	for i := 1; i < attempt && delay < maxDelay; i++ {
		delay *= 2
	}
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

// Backoff returns the bounded exponential delay for a retry attempt.
func Backoff(attempt int) time.Duration { return backoff(attempt) }

func (s *Store) load() (stateFile, error) {
	st := stateFile{SchemaVersion: 1, Items: map[string]Item{}}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("parse retry state %s: %w", s.path, err)
	}
	if st.Items == nil {
		st.Items = map[string]Item{}
	}
	return st, nil
}

func (s *Store) save(st stateFile) error {
	st.SchemaVersion = 1
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".retry-state-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path)
}
