package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentsync/internal/session"
)

func TestWriteBackImportsAndPatches(t *testing.T) {
	// A project dir that is a real git repo so PatchImport's deriveProjectID
	// (fallback path) is exercised without a remote.
	proj := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, proj, "init", "-q", "-b", "main")

	// Payload export file with a projectID to patch from.
	export := filepath.Join(t.TempDir(), "export.json")
	if err := fixtureExport(export, "ses_import1", proj); err != nil {
		t.Fatal(err)
	}

	imported := ""
	ad := &Adapter{
		Import:       func(p string) error { imported = p; return nil },
		PatchImport:  func(string, string, string) error { return nil },
		ProcessGuard: func(string) (bool, error) { return false, nil },
	}
	if err := ad.WriteBack(&session.Session{
		ID:           "ses_import1",
		CanonicalKey: "github.com-user-x",
		PayloadPath:  export,
	}, proj); err != nil {
		t.Fatalf("WriteBack: %v", err)
	}
	if imported != export {
		t.Errorf("expected Import called with %s, got %s", export, imported)
	}
}

func TestBroadcastSkipsBusyCandidate(t *testing.T) {
	export := filepath.Join(t.TempDir(), "export.json")
	if err := fixtureExport(export, "ses_b1", "/tmp"); err != nil {
		t.Fatal(err)
	}

	var imported []string
	ad := &Adapter{
		Import:      func(p string) error { imported = append(imported, p); return nil },
		PatchImport: func(string, string, string) error { return nil },
		ProcessGuard: func(cand string) (bool, error) {
			return strings.HasSuffix(cand, "busy"), nil
		},
	}

	s := &session.Session{ID: "ses_b1", CanonicalKey: "k", PayloadPath: export}
	res := ad.BroadcastWriteBack(s, []string{"/tmp/idle", "/tmp/busy"})
	if len(res.Imported) != 1 {
		t.Errorf("expected exactly 1 imported (idle), got %v", res.Imported)
	}
	if res.Imported[0] != "/tmp/idle" {
		t.Errorf("expected idle candidate imported, got %v", res.Imported)
	}
	if len(res.Busy) != 1 || res.Busy[0] != "/tmp/busy" {
		t.Errorf("expected busy candidate in Busy, got %v", res.Busy)
	}
	if res.Degraded {
		t.Error("single import is not a degraded outcome")
	}
}

func TestBroadcastDegradedWhenMultipleImported(t *testing.T) {
	export := filepath.Join(t.TempDir(), "export.json")
	if err := fixtureExport(export, "ses_b2", "/tmp"); err != nil {
		t.Fatal(err)
	}
	ad := &Adapter{
		Import:       func(p string) error { return nil },
		PatchImport:  func(string, string, string) error { return nil },
		ProcessGuard: func(string) (bool, error) { return false, nil },
	}
	s := &session.Session{ID: "ses_b2", CanonicalKey: "k", PayloadPath: export}
	res := ad.BroadcastWriteBack(s, []string{"/tmp/c1", "/tmp/c2"})
	if len(res.Imported) != 2 {
		t.Fatalf("expected both candidates imported, got %v", res.Imported)
	}
	if !res.Degraded {
		t.Error("multiple imports of a one-to-one session must be flagged degraded (invariant #8)")
	}
}

func TestBroadcastSkipsOnGuardErrorAndImportError(t *testing.T) {
	export := filepath.Join(t.TempDir(), "export.json")
	if err := fixtureExport(export, "ses_b3", "/tmp"); err != nil {
		t.Fatal(err)
	}
	ad := &Adapter{
		Import:      func(p string) error { return os.ErrNotExist },
		PatchImport: func(string, string, string) error { return nil },
		ProcessGuard: func(cand string) (bool, error) {
			if strings.HasSuffix(cand, "failguard") {
				return false, os.ErrPermission
			}
			return false, nil
		},
	}
	s := &session.Session{ID: "ses_b3", CanonicalKey: "k", PayloadPath: export}
	res := ad.BroadcastWriteBack(s, []string{"/tmp/failguard", "/tmp/failimport"})
	if len(res.Imported) != 0 {
		t.Errorf("expected no imports, got %v", res.Imported)
	}
	if len(res.Busy) != 0 {
		t.Errorf("guard error is not 'busy', got %v", res.Busy)
	}
}

func TestWriteBackNoPayloadErrors(t *testing.T) {
	ad := &Adapter{Import: func(string) error { return nil }}
	err := ad.WriteBack(&session.Session{ID: "x"}, "/tmp")
	if err == nil {
		t.Error("expected error when PayloadPath is empty")
	}
}
