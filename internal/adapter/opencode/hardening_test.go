package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"agentsync/internal/session"
)

func TestOnChangeKeepsOlderUnacknowledgedSession(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(state, []byte(`{"sessions":{"ses_newer":200}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	ad := &Adapter{
		QueryRecent: func(string) ([]byte, error) {
			return json.Marshal([]changedSession{
				{ID: "ses_older", TimeUpdated: 100, Directory: project},
				{ID: "ses_newer", TimeUpdated: 200, Directory: project},
			})
		},
		Export:    func(id, out string) error { return fixtureExport(out, id, project) },
		StateFile: func() (string, error) { return state, nil },
	}
	sessions, err := ad.OnChange(session.WatchEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "ses_older" {
		t.Fatalf("expected older unacknowledged session, got %#v", sessions)
	}
}

func TestOnChangeAppliesDenyBeforeExport(t *testing.T) {
	called := false
	ad := &Adapter{
		QueryRecent: func(string) ([]byte, error) {
			return json.Marshal([]changedSession{{ID: "secret", TimeUpdated: 1, Directory: "/private/project"}})
		},
		Export: func(string, string) error {
			called = true
			return fmt.Errorf("must not export denied session")
		},
		ShouldCapture: func(path string, _ session.CanonicalKey) bool { return path != "/private/project" },
		StateFile:     func() (string, error) { return filepath.Join(t.TempDir(), "state.json"), nil },
	}
	sessions, err := ad.OnChange(session.WatchEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if called || len(sessions) != 0 {
		t.Fatalf("denied session was exported=%v sessions=%v", called, sessions)
	}
}

func TestOnChangeRetriesExportFailure(t *testing.T) {
	attempts := 0
	project := t.TempDir()
	ad := &Adapter{
		QueryRecent: func(string) ([]byte, error) {
			return json.Marshal([]changedSession{{ID: "retry", TimeUpdated: 1, Directory: project}})
		},
		Export: func(id, out string) error {
			attempts++
			if attempts == 1 {
				return os.ErrPermission
			}
			return fixtureExport(out, id, project)
		},
		StateFile: func() (string, error) { return filepath.Join(t.TempDir(), "state.json"), nil },
	}
	if sessions, err := ad.OnChange(session.WatchEvent{}); err != nil || len(sessions) != 0 {
		t.Fatalf("first attempt sessions=%v err=%v", sessions, err)
	}
	if sessions, err := ad.OnChange(session.WatchEvent{}); err != nil || len(sessions) != 1 {
		t.Fatalf("second attempt sessions=%v err=%v", sessions, err)
	}
}
