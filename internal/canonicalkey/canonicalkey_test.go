package canonicalkey

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentsync/internal/session"
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

// TestKeyFromURLEquivalenceMatrix pins THE core guarantee of NormalizeRemote:
// every transport spelling of one origin produces the identical key.
func TestKeyFromURLEquivalenceMatrix(t *testing.T) {
	const want = "github.com-user-repo"
	for _, remote := range []string{
		"git@github.com:user/repo.git",
		"ssh://git@github.com/user/repo.git",
		"https://github.com/user/repo.git",
		"https://Utkarsh@github.com/user/repo.git/",
		"ssh://git@github.com:22/user/repo.git",
		"HTTPS://GITHUB.COM/USER/REPO.GIT",
		"github.com:user/repo",
	} {
		t.Run(remote, func(t *testing.T) {
			if got := keyFromURL(remote); got != want {
				t.Errorf("keyFromURL(%q) = %q, want %q", remote, got, want)
			}
		})
	}
}

func TestNormalizeRemotePortEquivalence(t *testing.T) {
	base := keyFromURL("git@github.com:user/repo.git")
	for _, withPort := range []string{
		"ssh://git@github.com:22/user/repo.git",
		"ssh://git@github.com:2222/user/repo.git",
		"https://github.com:443/user/repo.git",
	} {
		if got := keyFromURL(withPort); got != base {
			t.Errorf("keyFromURL(%q) = %q, want %q (ports are stripped, not identity)", withPort, got, base)
		}
	}
	if host, _ := NormalizeRemote("ssh://git@GitHub.com:443/x/y"); host != "github.com" {
		t.Errorf("NormalizeRemote port/lowercase host = %q, want %q", host, "github.com")
	}
}

func TestNormalizeRemoteSubgroupDepthDistinct(t *testing.T) {
	sub := keyFromURL("https://gitlab.com/g/sub/r")
	noSub := keyFromURL("https://gitlab.com/g/r")
	if sub == noSub {
		t.Errorf("subgroup depth must stay distinct: %q collided with %q", sub, noSub)
	}
}

func TestNormalizeRemoteNoOverCollapsing(t *testing.T) {
	pairs := [][2]string{
		{"git@github.com:owner-a/repo.git", "git@github.com:owner-b/repo.git"},
		{"git@github.com:user/repo-one.git", "git@github.com:user/repo-two.git"},
	}
	for _, p := range pairs {
		a, b := keyFromURL(p[0]), keyFromURL(p[1])
		if a == b {
			t.Errorf("distinct projects must stay distinct: %q and %q both -> %q", p[0], p[1], a)
		}
	}
}

// TestKeyFromURLEmptyHostPinsCurrentBehavior documents and pins degenerate
// inputs that never had a real host under scp-era parsing: the raw string
// flowed into the join unchanged except separators flattening to "-". These
// bytes are load-bearing for existing on-disk keys — do not "fix".
func TestKeyFromURLEmptyHostPinsCurrentBehavior(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "",
		"/local/path/repo": "-local-path-repo",
		"plainname":        "plainname",
	} {
		if got := keyFromURL(in); got != want {
			t.Errorf("keyFromURL(%q) = %q, want %q (pinned current behavior)", in, got, want)
		}
	}
	// Resolve still passes these through slug(); slugging must not alter them
	// (they're already slug-safe apart from separators that don't appear here).
	for in, want := range map[string]string{
		"/local/path/repo": "-local-path-repo",
		"plainname":        "plainname",
	} {
		if got := slug(keyFromURL(in)); got != session.CanonicalKey(want) {
			t.Errorf("slug(keyFromURL(%q)) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeRemoteUnitTable(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantSegs []string
	}{
		// host lowercase + numeric-only ":port" strip, in authority position.
		{"HTTPS://GitHub.com:443/", "github.com", nil},
		{"ssh://git@GitHub.com:443/x/y", "github.com", []string{"x", "y"}},
		{"https://User@Example.COM:8443/A/B", "example.com", []string{"a", "b"}},
		// Bare "host:port-ish" input has no authority section (git's scp
		// grammar starts the path at the first ":"), so digits stay a segment;
		// only host slots get port-stripped.
		{"GitHub.com:443/foo", "github.com", []string{"443", "foo"}},
		// "."/".." segment drops and repeated-slash collapse on real URL
		// paths.
		{"https://h.example/a/./b/../c", "h.example", []string{"a", "b", "c"}},
		{"https://h//a///b", "h", []string{"a", "b"}},
		// Degenerate (no-host) inputs: separator positions survive verbatim
		// — empty parts and "."/".." stay, unlike host-carrying remotes.
		// NOTE the global trailing-"/"/".git" trim loop runs before this
		// branch, so trailing separators on bare paths are already gone.
		{"/x/./y/../z", "", []string{"", "x", ".", "y", "..", "z"}},
		{"/p//q///r/", "", []string{"", "p", "", "q", "", "", "r"}},
		// trailing ".git"/"/" multi-strip loop.
		{"git@gitlab.com:g/r.git///", "gitlab.com", []string{"g", "r"}},
		{"u.git/x.git//", "", []string{"u.git", "x"}},
		{".git", "", []string{""}},
	}
	for _, tc := range cases {
		host, segs := NormalizeRemote(tc.in)
		if host != tc.wantHost || !equalStrings(segs, tc.wantSegs) {
			t.Errorf("NormalizeRemote(%q) = (%q, %v), want (%q, %v)", tc.in, host, segs, tc.wantHost, tc.wantSegs)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
