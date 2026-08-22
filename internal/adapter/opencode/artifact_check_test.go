package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validExport = `{"info":{"id":"ses_x","projectID":"p","directory":"/d","version":"1.0"}}`

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
		{"outside expected depth rejected", "opencode/ses_x.json", validExport, "unexpected location"},
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
