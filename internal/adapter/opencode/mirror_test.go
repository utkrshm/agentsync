package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentsync/internal/revision"
	"agentsync/internal/session"
)

// stubDevice keeps fixture Mirror runs hermetic (no real device-id file).
func stubDevice(ad *Adapter) {
	ad.SourceDevice = func() (string, error) { return "dev-fixture", nil }
	ad.DeviceAlias = func() string { return "" }
}

// TestMirrorTruncatedPayloadWritesNothing proves the validation gate runs
// before anything durable is written: a truncated export leaves no file under
// the sync repo destination.
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
	stubDevice(ad)
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
	if _, statErr := os.Stat(filepath.Join(repo, "opencode")); !os.IsNotExist(statErr) {
		t.Errorf("failed validation must create no opencode/ tree at all (stat err=%v)", statErr)
	}
	// PayloadPath must still point at the temp source; the session stays
	// unmirrored and retriable.
	if s.PayloadPath != payload {
		t.Errorf("PayloadPath mutated on failure: %q", s.PayloadPath)
	}
}

// TestMirrorProducesRevisionPair checks that Mirror stores exactly two files
// — payload plus sidecar — under the revisions layout, with meta fields
// derived from the validated export and injected device identity.
func TestMirrorProducesRevisionPair(t *testing.T) {
	project := t.TempDir()
	payload := filepath.Join(t.TempDir(), "payload.json")
	if err := fixtureExport(payload, "ses_meta", project); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := revision.DigestBytes(raw)

	repo := t.TempDir()
	captured := time.UnixMilli(1786822993740)
	s := &session.Session{
		ID:           "ses_meta",
		Tool:         session.ToolOpenCode,
		CanonicalKey: "key",
		LastModified: captured,
		PayloadPath:  payload,
	}
	ad := &Adapter{
		StateFile: func() (string, error) { return filepath.Join(t.TempDir(), "state.json"), nil },
	}
	stubDevice(ad)
	if err := ad.Mirror(s, repo); err != nil {
		t.Fatal(err)
	}

	wantRel := revision.Path("key", "ses_meta", digest)
	stored, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(wantRel)))
	if err != nil {
		t.Fatalf("revision payload missing: %v", err)
	}
	if string(stored) != string(raw) {
		t.Error("stored revision bytes differ from the validated export payload")
	}

	meta, err := revision.ReadMeta(filepath.Join(repo, filepath.FromSlash(revision.MetaPath("key", "ses_meta", digest))))
	if err != nil {
		t.Fatalf("sidecar unreadable: %v", err)
	}
	if meta.SchemaVersion != revision.SchemaVersion {
		t.Errorf("schema_version = %d, want %d", meta.SchemaVersion, revision.SchemaVersion)
	}
	if meta.OriginalSessionID != "ses_meta" || meta.Digest != digest {
		t.Errorf("identity fields wrong: %#v", meta)
	}
	if meta.SourceDeviceID != "dev-fixture" {
		t.Errorf("source_device_id = %q, want dev-fixture", meta.SourceDeviceID)
	}
	if meta.Status != revision.StatusCaptured {
		t.Errorf("status = %q, want captured", meta.Status)
	}
	if !meta.CapturedAt.Equal(captured.UTC()) {
		t.Errorf("captured_at = %v, want %v", meta.CapturedAt, captured.UTC())
	}
	if meta.ProducerVersion != "1.18.18" {
		t.Errorf("producer_version = %q, want 1.18.18", meta.ProducerVersion)
	}
	// Export info passthrough: fixtureExport sets these exact values.
	if meta.ProjectID != "global" || meta.Directory != project || meta.Title != "fixture session" {
		t.Errorf("export info not carried into sidecar: %#v", meta)
	}
	// Exactly two files must exist in the revisions directory.
	entries, err := os.ReadDir(filepath.Dir(filepath.Join(repo, filepath.FromSlash(wantRel))))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected exactly payload+sidecar, got %v", names)
	}
	// PayloadPath must now point at the stored revision inside the repo.
	if !strings.HasSuffix(filepath.ToSlash(s.PayloadPath), wantRel) {
		t.Errorf("PayloadPath = %q, want suffix %q", s.PayloadPath, wantRel)
	}
	// Both stored files must pass the staging validator syncrepo.Commit uses.
	if err := CheckArtifactFile(filepath.Join(repo, filepath.FromSlash(wantRel)), wantRel); err != nil {
		t.Errorf("staging validator must accept mirrored payload: %v", err)
	}
	metaRel := revision.MetaPath("key", "ses_meta", digest)
	if err := CheckArtifactFile(filepath.Join(repo, filepath.FromSlash(metaRel)), metaRel); err != nil {
		t.Errorf("staging validator must accept mirrored sidecar: %v", err)
	}
}

// TestMirrorIdempotentReMirror proves re-mirroring identical content creates
// no duplicate revision and no error.
func TestMirrorIdempotentReMirror(t *testing.T) {
	project := t.TempDir()
	payload := filepath.Join(t.TempDir(), "payload.json")
	if err := fixtureExport(payload, "ses_remirror", project); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	newSession := func() *session.Session {
		return &session.Session{
			ID:           "ses_remirror",
			Tool:         session.ToolOpenCode,
			CanonicalKey: "key",
			LastModified: time.UnixMilli(1000),
			PayloadPath:  payload,
		}
	}
	ad := &Adapter{
		StateFile: func() (string, error) { return filepath.Join(t.TempDir(), "state.json"), nil },
	}
	stubDevice(ad)

	first := newSession()
	if err := ad.Mirror(first, repo); err != nil {
		t.Fatal(err)
	}
	second := newSession()
	if err := ad.Mirror(second, repo); err != nil {
		t.Fatalf("re-mirror of identical payload failed: %v", err)
	}
	revDir := filepath.Join(repo, "opencode", "key", "sessions", "ses_remirror", "revisions")
	entries, err := os.ReadDir(revDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("idempotent re-mirror duplicated files: %d entries", len(entries))
	}
	if first.PayloadPath != second.PayloadPath {
		t.Errorf("both mirrors should resolve to one stored path: %q vs %q", first.PayloadPath, second.PayloadPath)
	}
}

// TestMirrorZeroLastModifiedFallsBackToNow covers the CapturedAt fallback:
// a zero LastModified must not persist as a zero timestamp.
func TestMirrorZeroLastModifiedFallsBackToNow(t *testing.T) {
	payload := filepath.Join(t.TempDir(), "payload.json")
	if err := fixtureExport(payload, "ses_zero", "/p"); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	s := &session.Session{
		ID:           "ses_zero",
		Tool:         session.ToolOpenCode,
		CanonicalKey: "key",
		PayloadPath:  payload,
	}
	ad := &Adapter{
		StateFile: func() (string, error) { return filepath.Join(t.TempDir(), "state.json"), nil },
	}
	stubDevice(ad)
	before := time.Now().UTC().Add(-time.Second)
	if err := ad.Mirror(s, repo); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.PayloadPath)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := revision.ReadMeta(filepath.Join(repo,
		filepath.FromSlash(revision.MetaPath("key", "ses_zero", revision.DigestBytes(raw)))))
	if err != nil {
		t.Fatal(err)
	}
	if meta.CapturedAt.IsZero() || meta.CapturedAt.Before(before) {
		t.Errorf("zero LastModified must fall back to now, got %v", meta.CapturedAt)
	}
}

// TestMirrorRequiresSourceDevice pins the fail-loud behavior when the
// injectable device resolver was never wired.
func TestMirrorRequiresSourceDevice(t *testing.T) {
	payload := filepath.Join(t.TempDir(), "payload.json")
	if err := fixtureExport(payload, "ses_nodev", "/p"); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	s := &session.Session{
		ID:           "ses_nodev",
		Tool:         session.ToolOpenCode,
		CanonicalKey: "key",
		LastModified: time.UnixMilli(1),
		PayloadPath:  payload,
	}
	ad := &Adapter{
		StateFile:    func() (string, error) { return filepath.Join(t.TempDir(), "state.json"), nil },
		SourceDevice: func() (string, error) { return "", os.ErrPermission },
	}
	if err := ad.Mirror(s, repo); err == nil {
		t.Fatal("device id resolution failure must fail Mirror loudly")
	}
	if _, statErr := os.Stat(filepath.Join(repo, "opencode")); !os.IsNotExist(statErr) {
		t.Error("device failure must write nothing into the repo")
	}
}
