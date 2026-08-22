package opencode

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentsync/internal/session"
)

// trustedFixture builds a session payload plus an adapter whose version pin
// passes and whose producer baseline is a throwaway, so trusted-path checks
// are the only variable under test. The BinaryPath stub is applied last so it
// overrides stubProducer's default.
func trustedFixture(t *testing.T, resolved string, resolveErr error) (*session.Session, *Adapter) {
	t.Helper()
	export := filepath.Join(t.TempDir(), "export.json")
	if err := fixtureExport(export, "ses_pin", "/tmp"); err != nil {
		t.Fatal(err)
	}
	ad := &Adapter{ToolVersion: compatibleVersion}
	stubProducer(t, ad)
	ad.BinaryPath = func() (string, error) {
		return resolved, resolveErr
	}
	s := &session.Session{ID: "ses_pin", CanonicalKey: "k", PayloadPath: export}
	return s, ad
}

// stubProducer wires fixture adapters so the producer drift check passes
// silently against a throwaway baseline (used by legacy write-back tests that
// predate provenance and only exercise import mechanics).
func stubProducer(t *testing.T, ad *Adapter) {
	t.Helper()
	ad.BinaryPath = func() (string, error) { return "/fixture/bin/opencode", nil }
	ad.Fingerprint = func() (string, error) { return strings.Repeat("ab", 32), nil }
	ad.ProducerStateFile = func() (string, error) {
		return filepath.Join(t.TempDir(), "opencode-producer.json"), nil
	}
}

// driftFixture returns a session plus an adapter whose producer state lives at
// statePath, resolving to binPath with fingerprint fp. The returned slice
// collects warnings emitted through Loggerf.
func driftFixture(t *testing.T, statePath, binPath, fp string) (*session.Session, *Adapter, *[]string) {
	t.Helper()
	export := filepath.Join(t.TempDir(), "export.json")
	if err := fixtureExport(export, "ses_drift", "/tmp"); err != nil {
		t.Fatal(err)
	}
	warnings := &[]string{}
	ad := &Adapter{
		ToolVersion:       compatibleVersion,
		BinaryPath:        func() (string, error) { return binPath, nil },
		Fingerprint:       func() (string, error) { return fp, nil },
		ProducerStateFile: func() (string, error) { return statePath, nil },
		Loggerf: func(format string, args ...any) {
			*warnings = append(*warnings, fmt.Sprintf(format, args...))
		},
	}
	s := &session.Session{ID: "ses_drift", CanonicalKey: "k", PayloadPath: export}
	return s, ad, warnings
}

func TestValidateArtifactFirstRunStoresProducerRecord(t *testing.T) {
	state := filepath.Join(t.TempDir(), "opencode-producer.json")
	s, ad, warnings := driftFixture(t, state, "/fixture/bin/opencode", strings.Repeat("cd", 32))
	if err := ad.ValidateArtifact(s); err != nil {
		t.Fatalf("first observation should pass silently, got %v", err)
	}
	if len(*warnings) != 0 {
		t.Fatalf("first observation must not warn, got %v", *warnings)
	}
	rec, err := loadProducerRecord(state)
	if err != nil {
		t.Fatal(err)
	}
	if rec.BinaryPath != "/fixture/bin/opencode" || rec.SHA256 != strings.Repeat("cd", 32) || rec.Version != "1.18.18" {
		t.Fatalf("stored record = %#v", rec)
	}
	if _, err := time.Parse(time.RFC3339, rec.LastSeen); err != nil {
		t.Errorf("last_seen %q is not RFC3339: %v", rec.LastSeen, err)
	}
	info, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file permissions = %#o, want 600", perm)
	}
}

func TestValidateArtifactUnchangedBinaryPassesSilently(t *testing.T) {
	state := filepath.Join(t.TempDir(), "opencode-producer.json")
	const oldStamp = "2020-01-01T00:00:00Z"
	if err := saveProducerRecord(state, producerRecord{
		BinaryPath: "/fixture/bin/opencode",
		SHA256:     strings.Repeat("cd", 32),
		Version:    "1.18.18",
		LastSeen:   oldStamp,
	}); err != nil {
		t.Fatal(err)
	}
	s, ad, warnings := driftFixture(t, state, "/fixture/bin/opencode", strings.Repeat("cd", 32))
	if err := ad.ValidateArtifact(s); err != nil {
		t.Fatalf("unchanged binary should pass, got %v", err)
	}
	if len(*warnings) != 0 {
		t.Fatalf("unchanged binary must not warn, got %v", *warnings)
	}
	rec, err := loadProducerRecord(state)
	if err != nil {
		t.Fatal(err)
	}
	if rec.LastSeen == oldStamp {
		t.Error("last_seen was not refreshed on a matching binary")
	}
	if rec.SHA256 != strings.Repeat("cd", 32) || rec.BinaryPath != "/fixture/bin/opencode" {
		t.Errorf("matching refresh mutated identity fields: %#v", rec)
	}
}

func TestValidateArtifactDriftWarnsAndProceeds(t *testing.T) {
	tests := []struct {
		name          string
		storedPath    string
		storedHash    string
		currentPath   string
		currentHash   string
		wantInWarning []string
	}{
		{
			name:          "binary hash changed",
			storedPath:    "/fixture/bin/opencode",
			storedHash:    strings.Repeat("cd", 32),
			currentPath:   "/fixture/bin/opencode",
			currentHash:   strings.Repeat("ab", 32),
			wantInWarning: []string{"hash changed"},
		},
		{
			name:          "binary path changed",
			storedPath:    "/fixture/bin/opencode",
			storedHash:    strings.Repeat("cd", 32),
			currentPath:   "/opt/new/bin/opencode",
			currentHash:   strings.Repeat("cd", 32),
			wantInWarning: []string{"path changed", "/opt/new/bin/opencode"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "opencode-producer.json")
			if err := saveProducerRecord(state, producerRecord{
				BinaryPath: tt.storedPath,
				SHA256:     tt.storedHash,
				Version:    "1.18.18",
				LastSeen:   "2020-01-01T00:00:00Z",
			}); err != nil {
				t.Fatal(err)
			}
			s, ad, warnings := driftFixture(t, state, tt.currentPath, tt.currentHash)
			if err := ad.ValidateArtifact(s); err != nil {
				t.Fatalf("non-strict drift should proceed, got %v", err)
			}
			if len(*warnings) != 1 {
				t.Fatalf("expected exactly one loud warning, got %v", *warnings)
			}
			for _, want := range tt.wantInWarning {
				if !strings.Contains((*warnings)[0], want) {
					t.Errorf("warning %q missing %q", (*warnings)[0], want)
				}
			}
			// Drift alone must NOT re-baseline; that happens only after a
			// successful import.
			rec, err := loadProducerRecord(state)
			if err != nil {
				t.Fatal(err)
			}
			if rec.SHA256 != tt.storedHash || rec.BinaryPath != tt.storedPath {
				t.Errorf("validation re-baselined drifted state: %#v", rec)
			}
		})
	}
}

func TestValidateArtifactDriftStrictRefuses(t *testing.T) {
	state := filepath.Join(t.TempDir(), "opencode-producer.json")
	if err := saveProducerRecord(state, producerRecord{
		BinaryPath: "/fixture/bin/opencode",
		SHA256:     strings.Repeat("cd", 32),
		Version:    "1.18.18",
		LastSeen:   "2020-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	s, ad, warnings := driftFixture(t, state, "/fixture/bin/opencode", strings.Repeat("ab", 32))
	ad.StrictCheck = true
	err := ad.ValidateArtifact(s)
	if err == nil {
		t.Fatal("strict mode must refuse on fingerprint drift")
	}
	for _, want := range []string{"strict_producer_check", state} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	if len(*warnings) != 0 {
		t.Errorf("refusal is returned as an error, not warned: %v", *warnings)
	}
	rec, err2 := loadProducerRecord(state)
	if err2 != nil {
		t.Fatal(err2)
	}
	if rec.SHA256 != strings.Repeat("cd", 32) {
		t.Errorf("refusal must not re-baseline: %#v", rec)
	}
}

func TestValidateArtifactCorruptProducerStateFailsClosed(t *testing.T) {
	for _, content := range []string{
		`{`,
		`{"binary_path":"/x","sha256":"nothex","version":"1"}`,
		`{"sha256":"` + strings.Repeat("a", 64) + `"}`,
	} {
		dir := t.TempDir()
		state := filepath.Join(dir, "opencode-producer.json")
		if err := os.WriteFile(state, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		before, statErr := os.ReadFile(state)
		if statErr != nil {
			t.Fatal(statErr)
		}
		s, ad, _ := driftFixture(t, state, "/fixture/bin/opencode", strings.Repeat("ab", 32))
		err := ad.ValidateArtifact(s)
		if err == nil {
			t.Fatalf("content %q: expected fail-closed error, got none", content)
		}
		if !strings.Contains(err.Error(), state) {
			t.Errorf("content %q: error %v should name the path %s", content, err, state)
		}
		after, statErr := os.ReadFile(state)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if string(before) != string(after) {
			t.Errorf("content %q: corrupt file was modified in place", content)
		}
	}
}

func TestFingerprintFileHashesKnownContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "opencode-fake")
	content := []byte("fake opencode binary bytes\n")
	if err := os.WriteFile(p, content, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	got, err := fingerprintFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("fingerprint = %s, want %s", got, want)
	}
}

func TestWriteBackPersistsNewFingerprintAfterImport(t *testing.T) {
	state := filepath.Join(t.TempDir(), "opencode-producer.json")
	oldFP, newFP := strings.Repeat("cd", 32), strings.Repeat("ef", 32)
	if err := saveProducerRecord(state, producerRecord{
		BinaryPath: "/fixture/bin/opencode-old",
		SHA256:     oldFP,
		Version:    "1.17.0",
		LastSeen:   "2020-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	export := filepath.Join(t.TempDir(), "export.json")
	if err := fixtureExport(export, "ses_rebase", "/tmp"); err != nil {
		t.Fatal(err)
	}
	imported := false
	ad := &Adapter{
		ImportInto:   func(string, string) error { imported = true; return nil },
		PatchImport:  func(string, string, string) error { return nil },
		VerifyImport: func(string, string) error { return nil },
		ProcessGuard: func(string) (bool, error) { return false, nil },
		ToolVersion:  compatibleVersion,
		BinaryPath:   func() (string, error) { return "/fixture/bin/opencode-new", nil },
		Fingerprint: func() (string, error) {
			calls++
			if calls == 1 {
				return newFP, nil // validation observes the drifted (new) binary
			}
			return newFP, nil // re-baseline after successful import
		},
		ProducerStateFile: func() (string, error) { return state, nil },
		Loggerf:           func(string, ...any) {}, // drift warning expected; keep test output clean
	}
	s := &session.Session{ID: "ses_rebase", CanonicalKey: "k", PayloadPath: export}
	if err := ad.WriteBack(s, "/tmp"); err != nil {
		t.Fatalf("WriteBack: %v", err)
	}
	if !imported {
		t.Fatal("import did not run")
	}
	rec, err := loadProducerRecord(state)
	if err != nil {
		t.Fatal(err)
	}
	if rec.SHA256 != newFP || rec.BinaryPath != "/fixture/bin/opencode-new" || rec.Version != "1.18.18" {
		t.Fatalf("baseline was not re-recorded after import: %#v", rec)
	}
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
			name:     "empty trusted path allows any resolved path",
			trusted:  "",
			resolved: other,
			wantErr:  nil,
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
