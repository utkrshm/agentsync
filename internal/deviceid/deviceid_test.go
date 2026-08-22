package deviceid

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "agent-sync", "device-id")
}

func TestLoadFromCreatesValidV4UUID(t *testing.T) {
	p := tempPath(t)
	id, err := LoadFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	if !isValidUUID(id) {
		t.Errorf("generated id %q is not a valid canonical UUID", id)
	}
	if len(id) != 36 || id[14] != '4' {
		t.Errorf("generated id %q is not version 4", id)
	}
	if variant := id[19]; variant != '8' && variant != '9' && variant != 'a' && variant != 'b' {
		t.Errorf("generated id %q has non-RFC4122 variant nibble %q", id, string(variant))
	}
}

func TestLoadFromIsStableAcrossCalls(t *testing.T) {
	p := tempPath(t)
	first, err := LoadFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("identity changed between calls: %q != %q", first, second)
	}
}

func TestLoadFromReturnsExistingFileVerbatim(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "device-id")
	const existing = "01234567-89ab-cdef-0123-456789abcdef"
	if err := os.WriteFile(p, []byte(existing+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != existing {
		t.Errorf("expected pre-existing id %q, got %q", existing, got)
	}
}

func TestLoadFromCorruptFileFailsClosed(t *testing.T) {
	for _, content := range []string{"", "dev-123456789", "not a uuid at all", "0123456789abcdef0123456789abcdef"} {
		dir := t.TempDir()
		p := filepath.Join(dir, "device-id")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		before, statErr := os.ReadFile(p)
		if statErr != nil {
			t.Fatal(statErr)
		}
		_, err := LoadFrom(p)
		if err == nil {
			t.Fatalf("content %q: expected an error, got none", content)
		}
		if !strings.Contains(err.Error(), p) {
			t.Errorf("content %q: error %v should mention the path %s", content, err, p)
		}
		after, statErr := os.ReadFile(p)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if string(before) != string(after) {
			t.Errorf("content %q: corrupt file was modified in place (%q -> %q)", content, before, after)
		}
	}
}

func TestLoadFromEnforcesPermissionsOnCreatedFile(t *testing.T) {
	p := tempPath(t)
	if _, err := LoadFrom(p); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("created file permissions = %#o, want 600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("created parent directory permissions = %#o, want 700", perm)
	}
}
