package opencode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentsync/internal/revision"
)

const validExport = `{"info":{"id":"ses_x","projectID":"p","directory":"/d","version":"1.0"}}`

// revisionPayload builds a valid export payload for session ses_x plus its
// correct digest filename.
func revisionPayload() ([]byte, string) {
	data := []byte(validExport)
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:])
}

// writeRevision plants a payload+sidecar pair (or a corrupted variant) under
// dir using the real revisions layout.
func writeRevision(t *testing.T, dir, key, sid string, payload []byte, digest string) {
	t.Helper()
	rel := revision.Path(key, sid, digest)
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	meta := revision.Meta{
		SchemaVersion:     revision.SchemaVersion,
		OriginalSessionID: sid,
		Digest:            digest,
		SourceDeviceID:    "dev-a",
		Status:            revision.StatusCaptured,
	}
	blob, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(dir, filepath.FromSlash(revision.MetaPath(key, sid, digest)))
	if err := os.WriteFile(mp, blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckArtifactFile(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) string {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	payload, digest := revisionPayload()
	writeRevision(t, dir, "k", "ses_x", payload, digest)

	badHex := strings.Repeat("g", 64)

	cases := []struct {
		name    string
		rel     string
		content string
		wantErr string // empty = must pass
	}{
		{"valid export", "opencode/k/export/ses_x.json", validExport, ""},
		{"export id mismatches filename", "opencode/k/export/other.json", validExport, "mismatched"},
		{"truncated export rejected", "opencode/k/export/ses_x.json", `{"info":{"id":"ses_x"`, "not valid JSON"},
		{"missing version rejected", "opencode/k/export/ses_x.json",
			`{"info":{"id":"ses_x","projectID":"p","directory":"/d"}}`, "version is empty"},
		{"valid import-meta", "opencode/k/import-meta/ses_x.json", `{"id":"ses_x","title":"T"}`, ""},
		{"import-meta id mismatch", "opencode/k/import-meta/wrong.json", `{"id":"ses_x"}`, "does not match filename"},
		{"non-json name rejected", "opencode/k/export/ses_x.txt", validExport, ".json"},
		{"unknown subdirectory rejected", "opencode/k/misc/x.json", `{}`, "unrecognized"},

		{"valid revision payload passes", "opencode/k/sessions/ses_x/revisions/" + digest + ".json", validExport, ""},
		{"valid revision sidecar passes", "opencode/k/sessions/ses_x/revisions/" + digest + ".meta.json",
			fmt.Sprintf(`{"schema_version":1,"original_session_id":"ses_x","digest":%q,"source_device_id":"dev-a","status":"captured"}`, digest), ""},
		{
			name:    "revision payload content must match filename digest",
			rel:     "opencode/k/sessions/ses_x/revisions/" + digest + ".json",
			content: validExport + " trailing garbage",
			wantErr: "content hashes to",
		}, {
			name:    "revision filename must be lowercase hex digest",
			rel:     "opencode/k/sessions/ses_x/revisions/notadigest.json",
			content: validExport,
			wantErr: "64-character lowercase hex",
		}, {
			name:    "revision uppercase hex rejected",
			rel:     "opencode/k/sessions/ses_x/revisions/" + strings.ToUpper(digest) + ".json",
			content: validExport,
			wantErr: "64-character lowercase hex",
		}, {
			name:    "revision short hex rejected",
			rel:     "opencode/k/sessions/ses_x/revisions/abc123.json",
			content: validExport,
			wantErr: "64-character lowercase hex",
		},
		{
			name:    "revision sidecar digest mismatch rejected",
			rel:     "opencode/k/sessions/ses_x/revisions/" + digest + ".meta.json",
			content: fmt.Sprintf(`{"original_session_id":"ses_x","digest":"%s","status":"captured"}`, badHex),
			wantErr: "records digest",
		},
		{
			name:    "revision sidecar session mismatch rejected",
			rel:     "opencode/k/sessions/ses_x/revisions/" + digest + ".meta.json",
			content: fmt.Sprintf(`{"original_session_id":"other","digest":%q,"status":"captured"}`, digest),
			wantErr: "does not match session directory",
		},
		{
			name:    "revision sidecar corrupt JSON rejected",
			rel:     "opencode/k/sessions/ses_x/revisions/" + digest + ".meta.json",
			content: "{oops",
			wantErr: "not valid JSON metadata",
		},
		{
			name:    "unexpected depth under sessions rejected",
			rel:     "opencode/k/sessions/ses_x/revisions/extra/" + digest + ".json",
			content: validExport,
			wantErr: "unrecognized",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			abs := write(tc.rel, tc.content)
			err := CheckArtifactFile(abs, tc.rel)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected pass, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
