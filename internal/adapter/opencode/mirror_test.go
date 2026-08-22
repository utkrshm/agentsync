package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentsync/internal/session"
)

// TestMirrorTruncatedPayloadWritesNothing proves the validation gate runs
// before anything durable is written: a truncated export leaves no export and
// no import-meta file under the sync repo destination.
func TestMirrorTruncatedPayloadWritesNothing(t *testing.T) {
	payload := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(payload, []byte(`{"info":{"id":"ses_trunc","directory":"/p","version":"1.18.1`), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	s := &session.Session{
		ID:           "ses_trunc",
		Tool:         session.ToolOpenCode,
		CanonicalKey: "key",
		LastModified: time.UnixMilli(1),
		PayloadPath:  payload,
	}
	ad := &Adapter{
		StateFile: func() (string, error) { return filepath.Join(t.TempDir(), "state.json"), nil },
	}
	err := ad.Mirror(s, repo)
	if err == nil {
		t.Fatal("truncated payload must fail Mirror")
	}
	for _, rel := range []string{
		filepath.Join("opencode", "key", "export", "ses_trunc.json"),
		filepath.Join("opencode", "key", "import-meta", "ses_trunc.json"),
	} {
		if _, statErr := os.Stat(filepath.Join(repo, rel)); !os.IsNotExist(statErr) {
			t.Errorf("failed validation must leave nothing at %s (stat err=%v)", rel, statErr)
		}
	}
	// PayloadPath must still point at the temp source; the session stays
	// unmirrored and retriable.
	if s.PayloadPath != payload {
		t.Errorf("PayloadPath mutated on failure: %q", s.PayloadPath)
	}
}

// TestMirrorValidatedImportMetaMatchesExport checks that the persisted
// import-meta is built from the same parsed info as the stored export.
func TestMirrorValidatedImportMetaMatchesExport(t *testing.T) {
	project := t.TempDir()
	payload := filepath.Join(t.TempDir(), "payload.json")
	if err := fixtureExport(payload, "ses_meta", project); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	s := &session.Session{
		ID:           "ses_meta",
		Tool:         session.ToolOpenCode,
		CanonicalKey: "key",
		LastModified: time.UnixMilli(1),
		PayloadPath:  payload,
	}
	ad := &Adapter{
		StateFile: func() (string, error) { return filepath.Join(t.TempDir(), "state.json"), nil },
	}
	if err := ad.Mirror(s, repo); err != nil {
		t.Fatal(err)
	}
	exported, err := os.ReadFile(filepath.Join(repo, "opencode", "key", "export", "ses_meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := ValidateExport(exported, "ses_meta")
	if err != nil {
		t.Fatal(err)
	}
	metaBytes, err := os.ReadFile(filepath.Join(repo, "opencode", "key", "import-meta", "ses_meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta ExportInfo
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatal(err)
	}
	if meta != want {
		t.Errorf("import-meta %#v does not match validated export info %#v", meta, want)
	}
}
