package repoindex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initRepo(t *testing.T, path, remote string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	must(t, path, "git", "init", "-q", "-b", "main")
	if remote != "" {
		must(t, path, "git", "remote", "add", "origin", remote)
	}
}

func must(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
	return string(out)
}

func TestScanAndResolve(t *testing.T) {
	root := t.TempDir()

	// Two clones of the same remote → both should resolve for one key.
	remote := "git@github.com:user/proj.git"
	initRepo(t, filepath.Join(root, "clone-a"), remote)
	initRepo(t, filepath.Join(root, "clone-b"), remote)
	// An unrelated repo → different key.
	initRepo(t, filepath.Join(root, "other"), "git@github.com:user/other.git")

	db := t.TempDir()
	d, err := Open(filepath.Join(db, "repo-index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Scan(context.Background(), []string{root}, nil); err != nil {
		t.Fatal(err)
	}

	// The shared-remote key resolves to both clones.
	matches, err := d.Resolve("github.com-user-proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 clones for shared remote, got %d: %+v", len(matches), matches)
	}
	seen := map[string]bool{}
	for _, m := range matches {
		if m.LastSeen.IsZero() {
			t.Errorf("expected last_seen populated for %s", m.LocalPath)
		}
		seen[m.LocalPath] = true
	}
	if !seen[filepath.Join(root, "clone-a")] || !seen[filepath.Join(root, "clone-b")] {
		t.Errorf("missing expected clones: %v", seen)
	}

	// The unrelated repo resolves separately.
	other, err := d.Resolve("github.com-user-other")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 {
		t.Errorf("expected 1 match for other repo, got %d", len(other))
	}
}

func TestScanStopsAtGitAndSkipsNodeModules(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo, "git@github.com:user/r.git")

	// node_modules deep inside the repo's working tree — the walker must not
	// descend into it at all (we detect via: it also would not index a nested
	// git repo there, but the key assertion is the walk cost / non-descent).
	nested := filepath.Join(repo, "node_modules", "deep", "deeper")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// A git repo nested under node_modules that would only be found if we
	// wrongly descended into node_modules.
	initRepo(t, filepath.Join(nested, "evil"), "git@github.com:user/evil.git")

	d, err := Open(filepath.Join(t.TempDir(), "repo-index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Scan(context.Background(), []string{root}, nil); err != nil {
		t.Fatal(err)
	}

	matches, err := d.Resolve("github.com-user-r")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected only the top-level repo, got %d", len(matches))
	}
	// The evil nested repo must never appear.
	evil, err := d.Resolve("github.com-user-evil")
	if err != nil {
		t.Fatal(err)
	}
	if len(evil) != 0 {
		t.Errorf("walker descended into node_modules and found the nested repo: %v", evil)
	}
}

func TestScanIgnoresConfiguredDirs(t *testing.T) {
	root := t.TempDir()
	// A heavy dir that is itself a git repo at root level, but ignored.
	ignored := filepath.Join(root, "vendor")
	initRepo(t, ignored, "git@github.com:user/v.git")

	d, err := Open(filepath.Join(t.TempDir(), "repo-index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Scan(context.Background(), []string{root}, nil); err != nil {
		t.Fatal(err)
	}
	if matches, _ := d.Resolve("github.com-user-v"); len(matches) != 0 {
		t.Errorf("ignored dir was indexed: %v", matches)
	}
}

func TestResolveMissingKeyIsEmpty(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "repo-index.db"))
	if err != nil {
		t.Fatal(err)
	}
	matches, err := d.Resolve("nope")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("expected empty result for unknown key, got %v", matches)
	}
}
