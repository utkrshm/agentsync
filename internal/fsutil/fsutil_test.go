package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteFileContentAndPerms(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.json")
	if err := AtomicWriteFile(p, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"ok":true}` {
		t.Errorf("content = %q", got)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perms = %#o, want 600", perm)
	}
}

func TestAtomicWriteFileLeavesNoTempEntries(t *testing.T) {
	dir := t.TempDir()
	if err := AtomicWriteFile(filepath.Join(dir, "a"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "a" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dir should contain only the final file, got %v", names)
	}
}

func TestAtomicWriteFileCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "deep", "nested", "file.json")
	if err := AtomicWriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatalf("parent dirs should be auto-created: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
}
