package syncrepo

import (
	"errors"
	"fmt"
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
	os.WriteFile(filepath.Join(deviceA, "opencode", "f.txt"), []byte("a"), 0o600)
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
	writeFile(t, filepath.Join(deviceB, "opencode", "g.txt"), "b")
	if _, err := repoB.Commit("opencode", "sB", "2026-08-16T00:02:00Z"); err != nil {
		t.Fatal(err)
	}

	// Device A pushes another commit, so remote advances.
	writeFile(t, filepath.Join(deviceA, "opencode", "h.txt"), "c")
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
	writeFile(t, filepath.Join(deviceA, "opencode", "x.txt"), "x")
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
	if _, err := os.Stat(filepath.Join(deviceB, "opencode", "x.txt")); err != nil {
		t.Errorf("pulled repo should contain opencode/x.txt: %v", err)
	}
}

func TestPullFastForwardPushCreatedRemote(t *testing.T) {
	// Reproduce the two-device `agent-sync init` flow: neither device cloned
	// the remote (so there is no origin/HEAD symref — that only exists on
	// clone-created remotes), device A pushes, device B must still pull.
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, t.TempDir(), "init", "--bare", "-q", bare)

	// Device A: init + remote + commit + push (no clone).
	deviceA := filepath.Join(t.TempDir(), "devA")
	repoA := Open(deviceA)
	if err := repoA.Init(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(deviceA, "opencode", "x.txt"), "x")
	if _, err := repoA.Commit("opencode", "s", "2026-08-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := repoA.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	if err := repoA.Push(); err != nil {
		t.Fatalf("device A push should succeed: %v", err)
	}

	// Device B: init + remote only, no clone → no origin/HEAD.
	deviceB := filepath.Join(t.TempDir(), "devB")
	repoB := Open(deviceB)
	if err := repoB.Init(); err != nil {
		t.Fatal(err)
	}
	if err := repoB.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	if err := repoB.PullFastForward(); err != nil {
		t.Fatalf("device B pull should succeed despite no origin/HEAD: %v", err)
	}
	if _, err := os.Stat(filepath.Join(deviceB, "opencode", "x.txt")); err != nil {
		t.Errorf("pulled repo should contain opencode/x.txt: %v", err)
	}
}

func TestCommitNoChangesReturnsErrNoChanges(t *testing.T) {
	dir := t.TempDir()
	repo := Open(dir)
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	// First commit creates real content.
	writeFile(t, filepath.Join(dir, "opencode", "k", "export", "s1.json"), "{}")
	if _, err := repo.Commit("opencode", "s1", "2026-08-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	// A second commit with no file changes must be a no-op, not a hard error.
	if _, err := repo.Commit("opencode", "s1", "2026-08-16T00:01:00Z"); err != ErrNoChanges {
		t.Errorf("expected ErrNoChanges, got %v", err)
	}
	// And the repo must still be usable afterwards.
	writeFile(t, filepath.Join(dir, "opencode", "k", "export", "s2.json"), "{}")
	if _, err := repo.Commit("opencode", "s2", "2026-08-16T00:02:00Z"); err != nil {
		t.Fatalf("repo unusable after no-op commit: %v", err)
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

// captureWarnings swaps in an injectable Warnf that records every warning so
// tests can assert on emitted output instead of stderr noise.
func captureWarnings(r *Repo) func() string {
	var buf strings.Builder
	r.Warnf = func(format string, args ...any) {
		fmt.Fprintf(&buf, format, args...)
	}
	return func() string { return buf.String() }
}

func TestCommitSkipsForeignPathsAndWarns(t *testing.T) {
	dir := t.TempDir()
	repo := Open(dir)
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "notes.txt"), "user placed this")
	writeFile(t, filepath.Join(dir, "opencode", "k", "export", "s1.json"), "{}")

	warnings := captureWarnings(repo)

	version, err := repo.Commit("opencode", "s1", "2026-08-16T00:00:00Z")
	if err != nil {
		t.Fatalf("commit with foreign files present should still succeed: %v", err)
	}
	if version != 1 {
		t.Errorf("first commit should be v1, got v%d", version)
	}

	changed := git(t, dir, "show", "--name-only", "--format=", "HEAD")
	if strings.Contains(changed, "notes.txt") {
		t.Errorf("foreign file must not be committed, but HEAD contains it:\n%s", changed)
	}
	if !strings.Contains(changed, filepath.Join("opencode", "k", "export", "s1.json")) {
		t.Errorf("sync-owned session file should be committed, got:\n%s", changed)
	}
	if !strings.Contains(warnings(), "skipping non-sync path: notes.txt") {
		t.Errorf("expected a warning naming the foreign path, got:\n%s", warnings())
	}

	// The foreign file must remain untouched on disk and untracked.
	data, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil || string(data) != "user placed this" {
		t.Errorf("foreign file content changed on disk: %q, %v", data, err)
	}
	staged := git(t, dir, "status", "--porcelain")
	if !strings.Contains(staged, "notes.txt") {
		t.Errorf("foreign file should stay untracked, status:\n%s", staged)
	}
}

func TestCommitNoForeignPathsNoWarnings(t *testing.T) {
	dir := t.TempDir()
	repo := Open(dir)
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "opencode", "k", "export", "s1.json"), "{}")

	warnings := captureWarnings(repo)

	if _, err := repo.Commit("opencode", "s1", "2026-08-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if got := warnings(); got != "" {
		t.Errorf("expected zero warnings without foreign paths, got:\n%s", got)
	}
}

func TestCommitForeignOnlyChangeStillErrNoChanges(t *testing.T) {
	dir := t.TempDir()
	repo := Open(dir)
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	warnings := captureWarnings(repo)
	writeFile(t, filepath.Join(dir, "opencode", "k", "export", "s1.json"), "{}")
	if _, err := repo.Commit("opencode", "s1", "2026-08-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	// Only a foreign file changes afterwards → nothing sync-owned to stage,
	// so Commit must still be a no-op (ErrNoChanges), not a hard error.
	writeFile(t, filepath.Join(dir, "notes.txt"), "more user content")
	if _, err := repo.Commit("opencode", "s1", "2026-08-16T00:01:00Z"); !errors.Is(err, ErrNoChanges) {
		t.Errorf("expected ErrNoChanges when only foreign files changed, got %v", err)
	}
	if !strings.Contains(warnings(), "notes.txt") {
		t.Errorf("expected a warning for the foreign-only change, got:\n%s", warnings())
	}
}

// commitForeignFixture creates a history containing a tracked file outside
// the allowlist. Commit() refuses to stage such files by design now, so the
// raw git plumbing is used — this simulates legacy history (or another
// writer) that already contains foreign tracked paths.
func commitForeignFixture(t *testing.T, r *Repo, path, content string) {
	t.Helper()
	writeFile(t, filepath.Join(r.Path, path), content)
	if _, err := r.git("add", "--", path); err != nil {
		t.Fatal(err)
	}
	if _, err := r.git("-c", "user.email=test@example.com", "-c", "user.name=test",
		"commit", "-m", "fixture: foreign tracked file"); err != nil {
		t.Fatal(err)
	}
}

func TestPullFastForwardRemovesAllowlistedCollision(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, t.TempDir(), "init", "--bare", "-q", bare)

	deviceA := filepath.Join(t.TempDir(), "devA")
	repoA := Open(deviceA)
	if err := repoA.Init(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(deviceA, "opencode", "x", "export", "s.json"), `{"from":"device-a"}`)
	if _, err := repoA.Commit("opencode", "s", "2026-08-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := repoA.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	if err := repoA.Push(); err != nil {
		t.Fatal(err)
	}

	// Device B has its own untracked copy of the same sync-owned path (e.g.
	// generated by a prior init/import). It collides with incoming history;
	// being allowlisted it is safe to remove, and the merge must proceed.
	deviceB := filepath.Join(t.TempDir(), "devB")
	repoB := Open(deviceB)
	if err := repoB.Init(); err != nil {
		t.Fatal(err)
	}
	if err := repoB.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(deviceB, "opencode", "x", "export", "s.json"), `{"from":"device-b"}`)

	if err := repoB.PullFastForward(); err != nil {
		t.Fatalf("pull over allowlisted collision should succeed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(deviceB, "opencode", "x", "export", "s.json"))
	if err != nil || string(data) != `{"from":"device-a"}` {
		t.Errorf("allowlisted collision should be replaced by incoming content, got %q: %v", data, err)
	}
}

func TestPullFastForwardRefusesForeignUntrackedConflict(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, t.TempDir(), "init", "--bare", "-q", bare)

	// Device A's history contains a tracked file outside the allowlist.
	deviceA := filepath.Join(t.TempDir(), "devA")
	repoA := Open(deviceA)
	if err := repoA.Init(); err != nil {
		t.Fatal(err)
	}
	commitForeignFixture(t, repoA, "notes.txt", "incoming version")
	writeFile(t, filepath.Join(deviceA, "opencode", "a", "export", "s.json"), "{}")
	if _, err := repoA.Commit("opencode", "s", "2026-08-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := repoA.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	if err := repoA.Push(); err != nil {
		t.Fatal(err)
	}

	// Device B placed its own untracked notes.txt; the pull would overwrite
	// it → must refuse loudly, leave the file alone, and merge nothing.
	deviceB := filepath.Join(t.TempDir(), "devB")
	repoB := Open(deviceB)
	if err := repoB.Init(); err != nil {
		t.Fatal(err)
	}
	if err := repoB.SetRemote(bare); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(deviceB, "notes.txt"), "user's local notes")

	err := repoB.PullFastForward()
	if !errors.Is(err, ErrForeignUntrackedConflict) {
		t.Fatalf("expected ErrForeignUntrackedConflict, got %v", err)
	}
	for _, want := range []string{"refusing fast-forward", "notes.txt", "move or remove them manually"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message %q does not mention %q", err.Error(), want)
		}
	}
	data, readErr := os.ReadFile(filepath.Join(deviceB, "notes.txt"))
	if readErr != nil || string(data) != "user's local notes" {
		t.Errorf("user-placed file was modified or deleted: %q, %v", data, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(deviceB, "opencode", "a")); !os.IsNotExist(statErr) {
		t.Errorf("incoming payload must not appear when the pull is refused: %v", statErr)
	}

	// Push wraps divergence claims, not foreign conflicts — the sentinel and
	// remediation message must survive propagation unchanged, without any
	// misleading "diverged" wording.
	pushErr := repoB.Push()
	if !errors.Is(pushErr, ErrForeignUntrackedConflict) {
		t.Errorf("Push should propagate ErrForeignUntrackedConflict, got %v", pushErr)
	}
	if pushErr != nil && strings.Contains(pushErr.Error(), "diverged") {
		t.Errorf("foreign-conflict failure must not be misreported as divergence: %v", pushErr)
	}
}

// stubDeviceID swaps in a fixed durable identity for the duration of a test
// so meta assertions are deterministic and the real per-user config dir is
// never touched.
func stubDeviceID(t *testing.T, id string) {
	t.Helper()
	prev := loadDeviceID
	loadDeviceID = func() (string, error) { return id, nil }
	t.Cleanup(func() { loadDeviceID = prev })
}

func TestTouchMetaBackfillsDurableDeviceID(t *testing.T) {
	stubDeviceID(t, "11111111-2222-4333-8444-555555555555")
	dir := t.TempDir()
	repo := Open(dir)
	// Pre-existing meta carrying a legacy dev-<unixnano> identity.
	writeFile(t, filepath.Join(dir, ".sync-meta.json"),
		`{"schema_version":1,"device_id":"dev-1750000000000"}`)

	if err := repo.TouchMeta(); err != nil {
		t.Fatalf("TouchMeta with legacy id: %v", err)
	}
	m, err := repo.ReadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if m.DeviceID != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("legacy device id should be replaced by the durable one, got %q", m.DeviceID)
	}
	if m.SchemaVersion != 1 {
		t.Errorf("expected schema version 1, got %d", m.SchemaVersion)
	}
}

func TestInitUsesInjectedDeviceID(t *testing.T) {
	stubDeviceID(t, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	repo := Open(t.TempDir())
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	m, err := repo.ReadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if m.DeviceID != "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" {
		t.Errorf("expected injected device id, got %q", m.DeviceID)
	}
}

func TestCommitSkipsInvalidArtifactsAndWarns(t *testing.T) {
	dir := t.TempDir()
	repo := Open(dir)
	stubDeviceID(t, "dev-test")
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "opencode", "k", "export", "good.json"),
		`{"info":{"id":"good","projectID":"p","directory":"/d","version":"1.0"}}`)
	writeFile(t, filepath.Join(dir, "opencode", "k", "export", "junk.json"),
		`{"not":"a session export"}`)
	repo.ValidateArtifact = func(absPath, relPath string) error {
		if strings.Contains(relPath, "junk") {
			return errors.New("not an agent-sync export")
		}
		return nil
	}
	warnings := captureWarnings(repo)

	version, err := repo.Commit("opencode", "good", "2026-08-22T00:00:00Z")
	if err != nil {
		t.Fatalf("commit with one invalid artifact should still succeed: %v", err)
	}
	if version != 1 {
		t.Errorf("first commit should be v1, got v%d", version)
	}
	names := git(t, dir, "show", "--name-only", "--format=")
	for _, n := range strings.Split(names, "\n") {
		if strings.HasSuffix(strings.TrimSpace(n), "junk.json") {
			t.Errorf("invalid artifact must not be committed; got %q", names)
		}
	}
	if !strings.Contains(names, "opencode/k/export/good.json") {
		t.Errorf("valid artifact missing from commit: %q", names)
	}
	if w := warnings(); !strings.Contains(w, "junk.json") || !strings.Contains(w, "skipping invalid sync artifact") {
		t.Errorf("expected a warning naming junk.json, got %q", w)
	}
	// The rejected file stays on disk untouched.
	data, err := os.ReadFile(filepath.Join(dir, "opencode", "k", "export", "junk.json"))
	if err != nil || string(data) != `{"not":"a session export"}` {
		t.Errorf("invalid artifact must not be deleted or modified: %q err=%v", data, err)
	}
}

func TestCommitStagesArtifactDeletionsWithoutValidation(t *testing.T) {
	dir := t.TempDir()
	repo := Open(dir)
	stubDeviceID(t, "dev-test")
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "opencode", "k", "export", "gone.json")
	writeFile(t, p, `{"info":{"id":"gone","projectID":"p","directory":"/d","version":"1.0"}}`)
	if _, err := repo.Commit("opencode", "gone", "2026-08-22T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	// Delete locally without validation being possible (file is gone).
	calls := 0
	repo.ValidateArtifact = func(absPath, relPath string) error {
		calls++
		return fmt.Errorf("validator must not run for deletions")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("opencode", "gone", "2026-08-22T00:01:00Z"); err != nil {
		t.Fatalf("commit recording a deletion should succeed: %v", err)
	}
	if calls != 0 {
		t.Errorf("validator ran %d times for deletion entries", calls)
	}
	names := git(t, dir, "show", "--name-only", "--format=", "HEAD")
	if !strings.Contains(names, "opencode/k/export/gone.json") {
		t.Errorf("deletion of artifact missing from commit: %q", names)
	}
}
