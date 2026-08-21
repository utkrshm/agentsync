// Package receivestate stores device-local OpenCode write-back outcomes.
//
// This state deliberately never lives in the sync repository: an import on
// one device must not acknowledge the artifact for every other device.
package receivestate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"agentsync/internal/retry"
)

const (
	StatusVerified = "verified"
	StatusBusy     = "busy"
	StatusFailed   = "failed"
	StatusDegraded = "degraded"
)

// Outcome describes this device's most recent attempt to restore one artifact
// into one local clone.
type Outcome struct {
	ArtifactDigest string    `json:"artifact_digest"`
	SessionID      string    `json:"session_id"`
	CandidatePath  string    `json:"candidate_path"`
	Status         string    `json:"status"`
	LastAttempt    time.Time `json:"last_attempt"`
	Attempts       int       `json:"attempts,omitempty"`
	NextAttempt    time.Time `json:"next_attempt,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
}

type stateFile struct {
	SchemaVersion int                `json:"schema_version"`
	Outcomes      map[string]Outcome `json:"outcomes"`
}

// Store owns one local JSON state file. The CLI is the only writer today; the
// atomic replace keeps a partial process crash from corrupting the file.
type Store struct {
	path string
}

func DefaultPath() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "agent-sync", "receive-state.json"), nil
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return &Store{path: path}, nil
}

func Key(digest, candidatePath string) string {
	return digest + "\x00" + filepath.Clean(candidatePath)
}

func (s *Store) Get(digest, candidatePath string) (Outcome, bool, error) {
	st, err := s.load()
	if err != nil {
		return Outcome{}, false, err
	}
	o, ok := st.Outcomes[Key(digest, candidatePath)]
	return o, ok, nil
}

func (s *Store) Put(outcome Outcome) error {
	if outcome.ArtifactDigest == "" {
		return fmt.Errorf("receive outcome requires an artifact digest")
	}
	if outcome.CandidatePath == "" {
		return fmt.Errorf("receive outcome requires a candidate path")
	}
	if outcome.LastAttempt.IsZero() {
		outcome.LastAttempt = time.Now().UTC()
	}
	outcome.CandidatePath = filepath.Clean(outcome.CandidatePath)

	st, err := s.load()
	if err != nil {
		return err
	}
	key := Key(outcome.ArtifactDigest, outcome.CandidatePath)
	if outcome.Status == StatusBusy || outcome.Status == StatusFailed {
		previous := st.Outcomes[key]
		if outcome.Attempts <= previous.Attempts {
			outcome.Attempts = previous.Attempts + 1
		}
		if outcome.NextAttempt.IsZero() {
			outcome.NextAttempt = outcome.LastAttempt.Add(retry.Backoff(outcome.Attempts))
		}
	} else {
		outcome.NextAttempt = time.Time{}
	}
	st.Outcomes[key] = outcome
	return s.save(st)
}

func (s *Store) List() ([]Outcome, error) {
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Outcome, 0, len(st.Outcomes))
	for _, o := range st.Outcomes {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ArtifactDigest == out[j].ArtifactDigest {
			return out[i].CandidatePath < out[j].CandidatePath
		}
		return out[i].ArtifactDigest < out[j].ArtifactDigest
	})
	return out, nil
}

func (s *Store) load() (stateFile, error) {
	st := stateFile{SchemaVersion: 1, Outcomes: map[string]Outcome{}}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("parse receive state %s: %w", s.path, err)
	}
	if st.Outcomes == nil {
		st.Outcomes = map[string]Outcome{}
	}
	return st, nil
}

func (s *Store) save(st stateFile) error {
	st.SchemaVersion = 1
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".receive-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
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
	return os.Rename(tmpName, s.path)
}
