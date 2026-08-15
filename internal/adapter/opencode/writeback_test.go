package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentsync/internal/session"
)

func compatibleVersion() (string, error) { return "1.18.18", nil }

func TestWriteBackImportsFromTargetAndVerifies(t *testing.T) {
	proj := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, proj, "init", "-q", "-b", "main")
	export := filepath.Join(t.TempDir(), "export.json")
	if err := fixtureExport(export, "ses_import1", proj); err != nil {
		t.Fatal(err)
	}
	gotPath, gotDir := "", ""
	verified := false
	ad := &Adapter{
		ImportInto:   func(path, dir string) error { gotPath, gotDir = path, dir; return nil },
		PatchImport:  func(string, string, string) error { return nil },
		ToolVersion:  compatibleVersion,
		VerifyImport: func(path, dir string) error { verified = path == export && dir == proj; return nil },
		ProcessGuard: func(string) (bool, error) { return false, nil },
	}
	if err := ad.WriteBack(&session.Session{ID: "ses_import1", CanonicalKey: "github.com-user-x", PayloadPath: export}, proj); err != nil {
		t.Fatalf("WriteBack: %v", err)
	}
	if gotPath != export || gotDir != proj || !verified {
		t.Fatalf("target-aware import path=%q dir=%q verified=%v", gotPath, gotDir, verified)
	}
}

func TestWriteBackRefusesVersionMismatch(t *testing.T) {
	export := filepath.Join(t.TempDir(), "export.json")
	if err := fixtureExport(export, "ses_version", "/tmp"); err != nil {
		t.Fatal(err)
	}
	called := false
	ad := &Adapter{
		ImportInto:  func(string, string) error { called = true; return nil },
		ToolVersion: func() (string, error) { return "9.9.9", nil },
	}
	err := ad.WriteBack(&session.Session{ID: "ses_version", PayloadPath: export}, "/tmp")
	if err == nil || !strings.Contains(err.Error(), "version mismatch") || called {
		t.Fatalf("expected fail-closed version mismatch, err=%v importCalled=%v", err, called)
	}
}

func TestBroadcastSkipsBusyCandidateAndKeepsFailure(t *testing.T) {
	export := filepath.Join(t.TempDir(), "export.json")
	if err := fixtureExport(export, "ses_b1", "/tmp"); err != nil {
		t.Fatal(err)
	}
	ad := &Adapter{
		ImportInto: func(path, dir string) error {
			if strings.HasSuffix(dir, "fail") {
				return errors.New("import failed")
			}
			return nil
		},
		PatchImport:  func(string, string, string) error { return nil },
		ToolVersion:  compatibleVersion,
		ProcessGuard: func(cand string) (bool, error) { return strings.HasSuffix(cand, "busy"), nil },
	}
	s := &session.Session{ID: "ses_b1", CanonicalKey: "k", PayloadPath: export}
	res := ad.BroadcastWriteBack(s, []string{"/tmp/idle", "/tmp/busy", "/tmp/fail"})
	if len(res.Imported) != 1 || res.Imported[0] != "/tmp/idle" {
		t.Fatalf("imports = %v", res.Imported)
	}
	if len(res.Busy) != 1 || res.Busy[0] != "/tmp/busy" {
		t.Fatalf("busy = %v", res.Busy)
	}
	if len(res.Failed) != 1 || res.Failed[0].Path != "/tmp/fail" {
		t.Fatalf("failed = %#v", res.Failed)
	}
}

func TestBroadcastDegradedWhenMultipleImported(t *testing.T) {
	export := filepath.Join(t.TempDir(), "export.json")
	if err := fixtureExport(export, "ses_b2", "/tmp"); err != nil {
		t.Fatal(err)
	}
	ad := &Adapter{
		ImportInto:   func(string, string) error { return nil },
		PatchImport:  func(string, string, string) error { return nil },
		ToolVersion:  compatibleVersion,
		ProcessGuard: func(string) (bool, error) { return false, nil },
	}
	res := ad.BroadcastWriteBack(&session.Session{ID: "ses_b2", CanonicalKey: "k", PayloadPath: export}, []string{"/tmp/c1", "/tmp/c2"})
	if len(res.Imported) != 2 || !res.Degraded {
		t.Fatalf("expected degraded two-clone result, got %#v", res)
	}
}

func TestWriteBackNoPayloadErrors(t *testing.T) {
	ad := &Adapter{ToolVersion: compatibleVersion}
	if err := ad.WriteBack(&session.Session{ID: "x"}, "/tmp"); err == nil {
		t.Error("expected error when PayloadPath is empty")
	}
}
