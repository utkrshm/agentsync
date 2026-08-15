package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentsync/internal/session"
)

// fixtureExport writes a minimal export JSON to outPath, mimicking the
// testdata/export.json shape but with a caller-chosen id and directory.
func fixtureExport(outPath, id, directory string) error {
	doc := map[string]any{
		"info": map[string]any{
			"id":        id,
			"slug":      "fixture",
			"projectID": "global",
			"directory": directory,
			"title":     "fixture session",
			"version":   "1.18.18",
		},
		"messages": []any{},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, data, 0o600)
}

func TestAdapterOnChangeAndMirror(t *testing.T) {
	cfgDir := t.TempDir()
	repoRoot := t.TempDir()
	// A real git repo so canonical key resolution finds a remote.
	remote := "git@github.com:user/fixture.git"
	projDir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, projDir, "init", "-q", "-b", "main")
	mustGit(t, projDir, "remote", "add", "origin", remote)

	ad := &Adapter{
		DataDir: func() (string, error) { return t.TempDir(), nil },
		Export: func(id, outPath string) error {
			return fixtureExport(outPath, id, projDir)
		},
		QueryRecent: func(sql string) ([]byte, error) {
			rows := []changedSession{
				{ID: "ses_new1", TimeUpdated: 1786822992739, Directory: projDir},
				{ID: "ses_new2", TimeUpdated: 1786822993740, Directory: projDir},
			}
			return json.Marshal(rows)
		},
		StateFile: func() (string, error) { return filepath.Join(cfgDir, "opencode-watch.json"), nil },
	}

	// First sweep: both sessions are new.
	sessions, err := ad.OnChange(session.WatchEvent{Path: "opencode.db"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	for _, s := range sessions {
		if s.Tool != session.ToolOpenCode {
			t.Errorf("expected tool opencode, got %v", s.Tool)
		}
		if s.CanonicalKey == "" {
			t.Errorf("expected a canonical key for %s", s.ID)
		}
		if !strings.Contains(string(s.CanonicalKey), "fixture") {
			t.Errorf("expected key derived from remote, got %q", s.CanonicalKey)
		}
		if err := ad.Mirror(&s, repoRoot); err != nil {
			t.Fatalf("mirror %s: %v", s.ID, err)
		}
		// PayloadPath must now point into the sync repo.
		if !strings.HasPrefix(s.PayloadPath, repoRoot) {
			t.Errorf("payload path should be under repo root, got %q", s.PayloadPath)
		}
		// Export and import-meta files must exist.
		for _, rel := range []string{
			filepath.Join("opencode", string(s.CanonicalKey), "export", s.ID+".json"),
			filepath.Join("opencode", string(s.CanonicalKey), "import-meta", s.ID+".json"),
		} {
			if _, err := os.Stat(filepath.Join(repoRoot, rel)); err != nil {
				t.Errorf("expected %s to exist: %v", rel, err)
			}
		}
	}

	// The watermark must have advanced to the max time_updated.
	st, err := os.ReadFile(filepath.Join(cfgDir, "opencode-watch.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(st), "1786822993740") {
		t.Errorf("watermark should be the max time_updated, got %s", st)
	}

	// Second sweep with the watermark set: a fake query returning only the old
	// sessions must produce nothing new.
	ad2 := &Adapter{
		DataDir: func() (string, error) { return t.TempDir(), nil },
		Export: func(id, outPath string) error {
			return fixtureExport(outPath, id, projDir)
		},
		QueryRecent: func(sql string) ([]byte, error) {
			rows := []changedSession{{ID: "ses_new1", TimeUpdated: 1786822992739, Directory: projDir}}
			return json.Marshal(rows)
		},
		StateFile: func() (string, error) { return filepath.Join(cfgDir, "opencode-watch.json"), nil },
	}
	again, err := ad2.OnChange(session.WatchEvent{Path: "opencode.db"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("watermark should prevent re-export, got %d sessions", len(again))
	}
}

func TestOnChangeSkipsExportFailure(t *testing.T) {
	ad := &Adapter{
		DataDir: func() (string, error) { return t.TempDir(), nil },
		Export: func(id, outPath string) error {
			return os.ErrNotExist // simulate opencode export failing for this id
		},
		QueryRecent: func(sql string) ([]byte, error) {
			rows := []changedSession{{ID: "bad", TimeUpdated: 1786822992739, Directory: "/denied"}}
			return json.Marshal(rows)
		},
		StateFile: func() (string, error) { return filepath.Join(t.TempDir(), "w.json"), nil },
	}
	sessions, err := ad.OnChange(session.WatchEvent{Path: "x"})
	if err != nil {
		t.Fatalf("OnChange should not fail on a single bad export: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected the failing session to be skipped, got %d", len(sessions))
	}
}

func TestWatchPaths(t *testing.T) {
	ad := &Adapter{
		DataDir: func() (string, error) { return "/tmp/fake-opencode-data", nil },
	}
	paths, err := ad.WatchPaths()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/tmp/fake-opencode-data/opencode.db",
		"/tmp/fake-opencode-data/opencode.db-wal",
		"/tmp/fake-opencode-data/opencode.db-shm",
	}
	for i, p := range paths {
		if p != want[i] {
			t.Errorf("path %d = %q, want %q", i, p, want[i])
		}
	}
}

func TestWatchStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "opencode-watch.json")
	ad := NewAdapter()
	ad.StateFile = func() (string, error) { return p, nil }

	// Create the payload temp file Mirror will copy.
	tmp := filepath.Join(t.TempDir(), "payload.json")
	if err := fixtureExport(tmp, "ses_1", "/tmp"); err != nil {
		t.Fatal(err)
	}
	s := &session.Session{
		ID:           "ses_1",
		Tool:         session.ToolOpenCode,
		CanonicalKey: "key",
		LastModified: time.UnixMilli(1786822993740),
		PayloadPath:  tmp,
	}
	repo := t.TempDir()
	if err := ad.Mirror(s, repo); err != nil {
		t.Fatal(err)
	}
	// Fresh adapter should read the persisted watermark.
	ad2 := NewAdapter()
	ad2.StateFile = func() (string, error) { return p, nil }
	if err := ad2.loadState(); err != nil {
		t.Fatal(err)
	}
	if got := ad2.acknowledged["ses_1"]; got != 1786822993740 {
		t.Errorf("expected persisted per-session acknowledgement, got %d", got)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := gitCmd(dir, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func gitCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd
}
