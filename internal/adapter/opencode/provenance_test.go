package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentsync/internal/session"
)

// trustedFixture builds a session payload plus an adapter whose version pin
// passes, so provenance checks are the only variable under test.
func trustedFixture(t *testing.T, resolved string, resolveErr error) (*session.Session, *Adapter) {
	t.Helper()
	export := filepath.Join(t.TempDir(), "export.json")
	if err := fixtureExport(export, "ses_pin", "/tmp"); err != nil {
		t.Fatal(err)
	}
	ad := &Adapter{
		ToolVersion: compatibleVersion,
		BinaryPath: func() (string, error) {
			return resolved, resolveErr
		},
	}
	s := &session.Session{ID: "ses_pin", CanonicalKey: "k", PayloadPath: export}
	return s, ad
}

func TestValidateArtifactTrustedBinaryPath(t *testing.T) {
	trusted := filepath.Join(t.TempDir(), "bin", "opencode")
	if err := os.MkdirAll(filepath.Dir(trusted), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "elsewhere", "opencode")

	tests := []struct {
		name       string
		trusted    string
		resolved   string
		resolveErr error
		wantErr    []string // substrings required in the error; empty slice = must pass
	}{
		{
			name:     "resolved matches trusted pin",
			trusted:  trusted,
			resolved: trusted,
			wantErr:  nil,
		},
		{
			name:     "resolved differs from trusted pin",
			trusted:  trusted,
			resolved: other,
			wantErr:  []string{trusted, other, "refusing"},
		},
		{
			name:       "empty trusted path disables the check even on resolution failure",
			trusted:    "",
			resolved:   "",
			resolveErr: errors.New(`exec: "opencode": executable file not found in $PATH`),
			wantErr:    nil,
		},
		{
			name:       "resolution failure with a pin set is actionable",
			trusted:    trusted,
			resolved:   "",
			resolveErr: errors.New(`exec: "opencode": executable file not found in $PATH`),
			wantErr:    []string{"not found on PATH", string(trusted)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ad := trustedFixture(t, tt.resolved, tt.resolveErr)
			ad.TrustedPath = tt.trusted
			err := ad.ValidateArtifact(s)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error mentioning %v, got none", tt.wantErr)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}

func TestNewAdapterWiresBinaryPath(t *testing.T) {
	ad := NewAdapter()
	if ad.BinaryPath == nil {
		t.Fatal("NewAdapter must wire BinaryPath")
	}
	first, firstErr := ad.BinaryPath()
	second, secondErr := ad.BinaryPath()
	if (firstErr == nil) != (secondErr == nil) || first != second {
		t.Fatalf("BinaryPath not cached per process: (%q, %v) then (%q, %v)", first, firstErr, second, secondErr)
	}
}
