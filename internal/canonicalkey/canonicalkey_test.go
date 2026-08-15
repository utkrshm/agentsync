package canonicalkey

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// helper to init a git repo at path and add a remote.
func initRepo(t *testing.T, path, remote string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun(t, path, "git", "init", "-q", "-b", "main")
	if remote != "" {
		mustRun(t, path, "git", "remote", "add", "origin", remote)
	}
}

func mustRun(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
	return string(out)
}

func TestResolveRemoteURL(t *testing.T) {
	root := t.TempDir()
	remote := "git@github.com:user/example.git"
	repoA := filepath.Join(root, "clone-a")
	repoB := filepath.Join(root, "clone-b")
	initRepo(t, repoA, remote)
	initRepo(t, repoB, remote)

	keyA := Resolve(repoA)
	keyB := Resolve(repoB)
	if keyA == "" {
		t.Fatal("expected a non-empty key")
	}
	if keyA != keyB {
		t.Errorf("same remote should resolve to same key: %q vs %q", keyA, keyB)
	}
	if strings.Contains(string(keyA), "/") {
		t.Errorf("key should be filesystem-safe (no slashes): %q", keyA)
	}
}

func TestResolveNoRemoteUsesCommitHash(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "no-remote")
	initRepo(t, repo, "")
	// Make a commit so there's a hash to fall back to.
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi"), 0o644)
	mustRun(t, repo, "git", "add", "-A")
	mustRun(t, repo, "git", "commit", "-q", "-m", "c1")

	key := Resolve(repo)
	if key == "" {
		t.Fatal("expected commit-hash fallback key")
	}
	if strings.HasPrefix(string(key), "_unmapped") {
		t.Errorf("repo with commits should not be unmapped: %q", key)
	}
	// Re-resolution must be stable.
	if key != Resolve(repo) {
		t.Errorf("key not stable across resolutions: %q vs %q", key, Resolve(repo))
	}
}

func TestResolveAlias(t *testing.T) {
	// Point the alias file at a temp config.
	cfgDir := t.TempDir()
	old := aliasesFile
	aliasesFile = func() string { return filepath.Join(cfgDir, "project-aliases.toml") }
	defer func() { aliasesFile = old }()

	target := filepath.Join(t.TempDir(), "plain-dir")
	os.MkdirAll(target, 0o755)
	os.WriteFile(filepath.Join(cfgDir, "project-aliases.toml"),
		[]byte(target+" = \"alias/project\""), 0o644)

	if key := Resolve(target); key != "alias/project" {
		t.Errorf("expected alias key, got %q", key)
	}
}

func TestResolveUnmapped(t *testing.T) {
	cfgDir := t.TempDir()
	old := aliasesFile
	aliasesFile = func() string { return filepath.Join(cfgDir, "project-aliases.toml") }
	defer func() { aliasesFile = old }()

	target := filepath.Join(t.TempDir(), "no-repo-no-alias")
	os.MkdirAll(target, 0o755)

	key := Resolve(target)
	if !strings.HasPrefix(string(key), "_unmapped") {
		t.Errorf("expected _unmapped prefix, got %q", key)
	}
	// Sentinel must not collide with a normal slug format (has - or alnum only).
	if string(key) == strings.Map(sanitize, string(key)) {
		t.Errorf("_unmapped key should be distinguishable from a slug: %q", key)
	}
}

func sanitize(r rune) rune {
	if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
		return r
	}
	return '?'
}
