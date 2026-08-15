package syncrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCommitIsTimestampedAndVersioned(t *testing.T) {
	dir := t.TempDir()
	repo := Open(dir)
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	// Write a payload file.
	writeFile(t, filepath.Join(dir, "opencode", "k", "export", "s1.json"), "{}")

	version, err := repo.Commit("opencode", "s1", "2026-08-16T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Errorf("first commit should be v1, got v%d", version)
	}
	msg := git(t, dir, "log", "-1", "--format=%s")
	re := regexp.MustCompile(`^sync: opencode s1 v1 2026-08-16T00:00:00Z$`)
	if !re.MatchString(msg) {
		t.Errorf("commit message %q does not match timestamped+versioned pattern", msg)
	}

	// Second commit should increment the version.
	writeFile(t, filepath.Join(dir, "opencode", "k", "export", "s2.json"), "{}")
	v2, err := repo.Commit("opencode", "s2", "2026-08-16T00:01:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if v2 != 2 {
		t.Errorf("second commit should be v2, got v%d", v2)
	}
}

func TestPushFastForwardAndDivergence(t *testing.T) {
	// Bare "remote" repo.
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, t.TempDir(), "init", "--bare", "-q", bare)

	// Device A clones, commits, pushes.
	deviceA := filepath.Join(t.TempDir(), "devA")
	git(t, t.TempDir(), "clone", "-q", bare, deviceA)
	repoA := Open(deviceA)
	if err := repoA.Init(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(deviceA, "f.txt"), []byte("a"), 0o600)
	if _, err := repoA.Commit("opencode", "sA", "2026-08-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := repoA.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	if err := repoA.Push(); err != nil {
		t.Fatalf("first push should succeed: %v", err)
	}

	// Device B clones, commits locally (no push yet).
	deviceB := filepath.Join(t.TempDir(), "devB")
	git(t, t.TempDir(), "clone", "-q", bare, deviceB)
	repoB := Open(deviceB)
	os.WriteFile(filepath.Join(deviceB, "g.txt"), []byte("b"), 0o600)
	if _, err := repoB.Commit("opencode", "sB", "2026-08-16T00:02:00Z"); err != nil {
		t.Fatal(err)
	}

	// Device A pushes another commit, so remote advances.
	os.WriteFile(filepath.Join(deviceA, "h.txt"), []byte("c"), 0o600)
	if _, err := repoA.Commit("opencode", "sA2", "2026-08-16T00:03:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := repoA.Push(); err != nil {
		t.Fatalf("second push should succeed: %v", err)
	}

	// Now device B tries to push without pulling → remote diverged → must fail
	// and must NOT force-push (no data loss).
	if err := repoB.Push(); err == nil {
		t.Fatal("expected push to fail on diverged remote, but it succeeded")
	}
}

func TestPullFastForward(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, t.TempDir(), "init", "--bare", "-q", bare)

	deviceA := filepath.Join(t.TempDir(), "devA")
	git(t, t.TempDir(), "clone", "-q", bare, deviceA)
	repoA := Open(deviceA)
	if err := repoA.Init(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(deviceA, "x.txt"), []byte("x"), 0o600)
	if _, err := repoA.Commit("opencode", "s", "2026-08-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := repoA.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	if err := repoA.Push(); err != nil {
		t.Fatal(err)
	}

	// Device B pulls and should get device A's commit.
	deviceB := filepath.Join(t.TempDir(), "devB")
	git(t, t.TempDir(), "clone", "-q", bare, deviceB)
	repoB := Open(deviceB)
	if err := repoB.PullFastForward(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(deviceB, "x.txt")); err != nil {
		t.Errorf("pulled repo should contain x.txt: %v", err)
	}
}

func TestSyncMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	repo := Open(dir)
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	if err := repo.TouchMeta(); err != nil {
		t.Fatal(err)
	}
	m, err := repo.ReadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if m.DeviceID == "" {
		t.Error("expected a device id to be generated")
	}
	if m.SchemaVersion != 1 {
		t.Errorf("expected schema version 1, got %d", m.SchemaVersion)
	}
}
