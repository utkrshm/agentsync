package revision

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPayload(t *testing.T, content string) []byte {
	t.Helper()
	if !strings.HasPrefix(content, "{") {
		content = `{"info":{"id":"ses_1","directory":"/p","version":"1.0"},"body":"` + content + `"}`
	}
	return []byte(content)
}

func testMeta(data []byte) Meta {
	return Meta{
		OriginalSessionID: "ses_1",
		Digest:            DigestBytes(data),
		SourceDeviceID:    "dev-a",
		CapturedAt:        time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		ProducerVersion:   "1.18.18",
		Status:            StatusCaptured,
	}
}

// TestWriteHappyPath covers a first write: both files land at the exact
// layout paths, written reports creation, and the sidecar round-trips.
func TestWriteHappyPath(t *testing.T) {
	root := t.TempDir()
	data := testPayload(t, "first")
	meta := testMeta(data)
	meta.Title = "hello"
	meta.ProjectID = "prj_1"
	meta.Directory = "/p"

	written, err := Write(root, "key1", "ses_1", data, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !written {
		t.Error("first write must report written=true")
	}
	payloadPath := filepath.Join(root, filepath.FromSlash(Path("key1", "ses_1", meta.Digest)))
	metaPath := filepath.Join(root, filepath.FromSlash(MetaPath("key1", "ses_1", meta.Digest)))
	got, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("payload missing: %v", err)
	}
	if string(got) != string(data) {
		t.Error("stored payload bytes differ from input")
	}
	saved, err := ReadMeta(metaPath)
	if err != nil {
		t.Fatalf("sidecar unreadable: %v", err)
	}
	want := meta
	want.SchemaVersion = SchemaVersion
	if saved != want {
		t.Errorf("sidecar = %#v, want %#v", saved, want)
	}
}

// TestWriteIdempotentForIdenticalContent proves re-mirroring identical bytes
// is a payload no-op while the sidecar stays present.
func TestWriteIdempotentForIdenticalContent(t *testing.T) {
	root := t.TempDir()
	data := testPayload(t, "same")

	if _, err := Write(root, "key1", "ses_1", data, testMeta(data)); err != nil {
		t.Fatal(err)
	}
	payloadPath := filepath.Join(root, filepath.FromSlash(Path("key1", "ses_1", DigestBytes(data))))
	infoBefore, err := os.Stat(payloadPath)
	if err != nil {
		t.Fatal(err)
	}

	written, err := Write(root, "key1", "ses_1", data, testMeta(data))
	if err != nil {
		t.Fatalf("identical rewrite must succeed: %v", err)
	}
	if written {
		t.Error("idempotent rewrite must report written=false")
	}
	infoAfter, err := os.Stat(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Error("idempotent rewrite must not touch the existing payload file")
	}
	entries, err := os.ReadDir(filepath.Dir(payloadPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 { // exactly <digest>.json and <digest>.meta.json
		t.Errorf("expected 2 revision files after idempotent write, got %d", len(entries))
	}
}

// TestWriteSidecarUpdatedWhenDifferent checks last-write-wins for compatible
// sidecars: same digest+session, changed descriptive fields.
func TestWriteSidecarUpdatedWhenDifferent(t *testing.T) {
	root := t.TempDir()
	data := testPayload(t, "meta-drift")

	first := testMeta(data)
	first.DeviceAlias = ""
	if _, err := Write(root, "key1", "ses_1", data, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.DeviceAlias = "laptop"
	written, err := Write(root, "key1", "ses_1", data, second)
	if err != nil {
		t.Fatal(err)
	}
	if written {
		t.Error("sidecar-only update must still be written=false for the payload")
	}
	metaPath := filepath.Join(root, filepath.FromSlash(MetaPath("key1", "ses_1", second.Digest)))
	saved, err := ReadMeta(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.DeviceAlias != "laptop" {
		t.Errorf("sidecar not updated: alias = %q, want laptop", saved.DeviceAlias)
	}
}

// TestWriteDigestCollisionFailsLoudly plants different bytes at a valid
// digest path; the next Write of the true content must hard-fail and leave
// the planted file untouched.
func TestWriteDigestCollisionFailsLoudly(t *testing.T) {
	root := t.TempDir()
	data := testPayload(t, "true content")
	digest := DigestBytes(data)

	planted := []byte(`{"planted":true}`)
	p := filepath.Join(root, filepath.FromSlash(Path("key1", "ses_1", digest)))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, planted, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Write(root, "key1", "ses_1", data, testMeta(data))
	if !errors.Is(err, ErrDigestCollision) {
		t.Fatalf("expected ErrDigestCollision, got %v", err)
	}
	got, rerr := os.ReadFile(p)
	if rerr != nil || string(got) != string(planted) {
		t.Errorf("collision error must leave the planted file untouched (err=%v)", rerr)
	}
}

// TestWriteRepairsMissingSidecar verifies the sidecar is restored when it was
// lost while the identical payload already exists.
func TestWriteRepairsMissingSidecar(t *testing.T) {
	root := t.TempDir()
	data := testPayload(t, "repair-me")
	meta := testMeta(data)

	if _, err := Write(root, "key1", "ses_1", data, meta); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(root, filepath.FromSlash(MetaPath("key1", "ses_1", meta.Digest)))
	if err := os.Remove(metaPath); err != nil {
		t.Fatal(err)
	}

	written, err := Write(root, "key1", "ses_1", data, meta)
	if err != nil {
		t.Fatalf("missing-sidecar repair failed: %v", err)
	}
	if written {
		t.Error("repair must not count as a new payload write")
	}
	if _, err := ReadMeta(metaPath); err != nil {
		t.Errorf("sidecar not repaired: %v", err)
	}
}

// TestWriteRejectsInvalidMeta covers the validation gate: bad status,
// mismatched session id, empty fields.
func TestWriteRejectsInvalidMeta(t *testing.T) {
	data := testPayload(t, "invalid-meta-cases")
	tests := []struct {
		name    string
		mutate  func(Meta) Meta
		wantErr string
	}{
		{
			name:    "unknown status",
			mutate:  func(m Meta) Meta { m.Status = "weird"; return m },
			wantErr: `unknown status "weird"`,
		},
		{
			name:    "empty status",
			mutate:  func(m Meta) Meta { m.Status = ""; return m },
			wantErr: "unknown status",
		},
		{
			name:    "session mismatch",
			mutate:  func(m Meta) Meta { m.OriginalSessionID = "ses_other"; return m },
			wantErr: "original_session_id",
		},
		{
			name:    "empty original session",
			mutate:  func(m Meta) Meta { m.OriginalSessionID = ""; return m },
			wantErr: "empty original_session_id",
		},
		{
			name:    "digest mismatch vs payload",
			mutate:  func(m Meta) Meta { m.Digest = strings.Repeat("a", 64); return m },
			wantErr: "does not match payload digest",
		},
		{
			name:    "empty digest",
			mutate:  func(m Meta) Meta { m.Digest = ""; return m },
			wantErr: "empty digest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			_, err := Write(root, "key1", "ses_1", data, tt.mutate(testMeta(data)))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
			entries, rerr := os.ReadDir(root)
			if rerr != nil || len(entries) != 0 {
				t.Errorf("rejected meta must write nothing under root (err=%v entries=%d)", rerr, len(entries))
			}
		})
	}
}

// TestWriteRejectsEmptySessionID guards the caller-level session argument.
func TestWriteRejectsEmptySessionID(t *testing.T) {
	root := t.TempDir()
	data := testPayload(t, "no-session")
	if _, err := Write(root, "key1", "", data, testMeta(data)); err == nil || !strings.Contains(err.Error(), "empty session id") {
		t.Fatalf("expected empty-session-id error, got %v", err)
	}
}

// TestWriteSidecarConflictHardErrors plants a sidecar belonging to another
// revision; writing ours over it must fail loudly instead of clobbering.
func TestWriteSidecarConflictHardErrors(t *testing.T) {
	root := t.TempDir()
	data := testPayload(t, "conflicted")
	digest := DigestBytes(data)

	foreign := Meta{
		SchemaVersion:     SchemaVersion,
		OriginalSessionID: "ses_other",
		Digest:            strings.Repeat("f", 64),
		Status:            StatusCaptured,
	}
	mp := filepath.Join(root, filepath.FromSlash(MetaPath("key1", "ses_1", digest)))
	if err := os.MkdirAll(filepath.Dir(mp), 0o700); err != nil {
		t.Fatal(err)
	}
	foreignBlob, merr := json.MarshalIndent(foreign, "", "  ")
	if merr != nil {
		t.Fatal(merr)
	}
	if err := os.WriteFile(mp, foreignBlob, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, werr := Write(root, "key1", "ses_1", data, testMeta(data)); !errors.Is(werr, ErrSidecarConflict) {
		t.Fatalf("expected ErrSidecarConflict, got %v", werr)
	}
	saved, rerr := ReadMeta(mp)
	if rerr != nil || saved.Digest != foreign.Digest {
		t.Errorf("conflicting sidecar must be left untouched (err=%v saved=%#v)", rerr, saved)
	}
}

// TestReadMetaErrors pins the typed missing-file error and corrupt-JSON
// rejection.
func TestReadMetaErrors(t *testing.T) {
	dir := t.TempDir()

	if _, err := ReadMeta(filepath.Join(dir, "absent.meta.json")); !errors.Is(err, ErrNoMeta) {
		t.Errorf("missing sidecar must wrap ErrNoMeta, got %v", err)
	}

	corrupt := filepath.Join(dir, "corrupt.meta.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMeta(corrupt); err == nil || errors.Is(err, ErrNoMeta) {
		t.Errorf("corrupt sidecar must fail with a parse error, got %v", err)
	}

	incomplete := filepath.Join(dir, "incomplete.meta.json")
	if err := os.WriteFile(incomplete, []byte(`{"status":"captured"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMeta(incomplete); err == nil || !strings.Contains(err.Error(), "missing required field") {
		t.Errorf("sidecar without identity fields must fail, got %v", err)
	}
}

// TestPathsAreSlashRelativeAndStable locks the layout contract used by the
// staging validator and the repo walker.
func TestPathsAreSlashRelativeAndStable(t *testing.T) {
	const d = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got, want := Path("k", "s", d), "opencode/k/sessions/s/revisions/"+d+".json"; got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
	if got, want := MetaPath("k", "s", d), "opencode/k/sessions/s/revisions/"+d+".meta.json"; got != want {
		t.Errorf("MetaPath = %q, want %q", got, want)
	}
}

// TestDigestBytesMatchesSha256Hex pins the digest definition.
func TestDigestBytesMatchesSha256Hex(t *testing.T) {
	got := DigestBytes([]byte("abc"))
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got != want {
		t.Errorf("DigestBytes(abc) = %s, want %s", got, want)
	}
}
