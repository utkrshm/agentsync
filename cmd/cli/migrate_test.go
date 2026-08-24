package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentsync/internal/revision"
	"agentsync/internal/syncrepo"
)

// captureStderr swaps os.Stderr for a pipe while fn runs, returning
// everything written (migration warnings go to stderr like receive's).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string)
	go func() {
		var sb strings.Builder
		_, _ = io.Copy(&sb, r)
		done <- sb.String()
	}()
	defer func() { os.Stderr = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return <-done
}

// initTestGitRepo creates a real git repo at dir with a deterministic
// identity so syncrepo.Commit can run hermetically in tests.
func initTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{"GIT_AUTHOR_NAME", "agent-sync-test"},
		{"GIT_AUTHOR_EMAIL", "test@agentsync.invalid"},
		{"GIT_COMMITTER_NAME", "agent-sync-test"},
		{"GIT_COMMITTER_EMAIL", "test@agentsync.invalid"},
		{"GIT_CONFIG_GLOBAL", "/dev/null"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	cmd := exec.Command("git", "-C", dir, "init", "-b", "main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v: %s", dir, err, out)
	}
}

// commitCount returns the number of commits on HEAD (0 when none yet).
func commitCount(t *testing.T, dir string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-list", "--count", "HEAD").CombinedOutput()
	if err != nil {
		return 0 // unborn branch
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n); err != nil {
		t.Fatalf("parse rev-list count %q: %v", out, err)
	}
	return n
}

// plantLegacyExport writes one legacy flat-layout export plus an optional
// import-meta sidecar, returning the payload path.
func plantLegacyExport(t *testing.T, root, key, sid, metaJSON string, payload []byte, modTime time.Time) string {
	t.Helper()
	payloadPath := filepath.Join(root, "opencode", key, "export", sid+".json")
	writeFileFixture(t, payloadPath, string(payload))
	if metaJSON != "" {
		writeFileFixture(t, filepath.Join(root, "opencode", key, "import-meta", sid+".json"), metaJSON)
	}
	if err := os.Chtimes(payloadPath, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	return payloadPath
}

func TestClassifyLayout(t *testing.T) {
	tests := []struct {
		name         string
		legacy       int
		sessionsDirs int
		want         layoutState
	}{
		{"empty repo", 0, 0, layoutEmpty},
		{"fresh join: legacy only", 3, 0, layoutLegacyOnly},
		{"mixed state", 2, 1, layoutMixed},
		{"already migrated", 0, 4, layoutRevisionsOnly},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyLayout(tc.legacy, tc.sessionsDirs); got != tc.want {
				t.Fatalf("classifyLayout(%d, %d) = %v, want %v", tc.legacy, tc.sessionsDirs, got, tc.want)
			}
		})
	}
}

func TestScanLegacyExports(t *testing.T) {
	root := t.TempDir()
	plantLegacyExport(t, root, "kb", "ses_b", "", exportFixture("ses_b"), time.Now())
	plantLegacyExport(t, root, "ka", "ses_a", "", exportFixture("ses_a"), time.Now())
	// Junk that must never appear: non-json files, sidecars, stray dirs.
	writeFileFixture(t, filepath.Join(root, "opencode", "ka", "export", "notes.txt"), "x")

	exports, err := scanLegacyExports(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(exports) != 2 {
		t.Fatalf("want 2 legacy exports, got %#v", exports)
	}
	if exports[0].Key != "ka" || exports[0].SessionID != "ses_a" ||
		exports[1].Key != "kb" || exports[1].SessionID != "ses_b" {
		t.Errorf("scan not sorted by (key, session): %#v", exports)
	}

	if n, err := countSessionsDirs(root); err != nil || n != 0 {
		t.Errorf("countSessionsDirs = %d, %v; want 0, nil", n, err)
	}
	writeFileFixture(t, filepath.Join(root, "opencode", "ka", "sessions", "ses_a", "revisions", "x.json"), "{}")
	if n, err := countSessionsDirs(root); err != nil || n != 1 {
		t.Errorf("countSessionsDirs = %d, %v; want 1, nil", n, err)
	}

	// Missing opencode/ tree yields nothing, not an error.
	exports, err = scanLegacyExports(t.TempDir())
	if err != nil || len(exports) != 0 {
		t.Errorf("missing tree: exports=%#v err=%v; want none", exports, err)
	}
}

func TestRunMigrationDryRunTouchesNothing(t *testing.T) {
	root := t.TempDir()
	captured := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	payload := exportFixture("ses_dry")
	legacyPath := plantLegacyExport(t, root, "kd", "ses_dry",
		`{"id":"ses_dry","title":"dry-title"}`, payload, captured)

	var out string
	repo := &syncrepo.Repo{Path: root}
	n, err := func() (int, error) {
		var runN int
		var runErr error
		out = captureStdout(t, func() { runN, runErr = runMigration(repo, true, false) })
		return runN, runErr
	}()
	if err != nil {
		t.Fatalf("dry-run migration: %v", err)
	}
	if n != 1 {
		t.Fatalf("planned %d, want 1", n)
	}
	digest := mustDigest(payload)
	targetRel := revision.Path("kd", "ses_dry", digest)
	if !strings.Contains(out, "would migrate opencode/kd/export/ses_dry.json -> "+targetRel) {
		t.Errorf("plan line missing:\n%s", out)
	}
	if !strings.Contains(out, "would migrate 1 artifact(s)") {
		t.Errorf("summary missing:\n%s", out)
	}
	// Nothing may change on disk.
	if _, err := os.Stat(legacyPath); err != nil {
		t.Errorf("legacy payload must remain after dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(targetRel))); !os.IsNotExist(err) {
		t.Errorf("dry-run must not write the target artifact (stat err: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "opencode", "kd", "sessions")); !os.IsNotExist(err) {
		t.Errorf("dry-run must not create sessions/ (stat err: %v)", err)
	}
}

func TestRunMigrationApplyMovesWritesMetaAndRemovesLegacy(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	captured := time.Date(2026, 8, 21, 9, 30, 15, 0, time.UTC)
	payload := exportFixture("ses_move")
	// The planted import-meta intentionally disagrees with the payload's own
	// info block: migration must derive sidecar info from the VALIDATED
	// digest-bound export bytes, never from the possibly-stale legacy
	// import-meta sibling (which also carries no producer version).
	legacyPath := plantLegacyExport(t, root, "km", "ses_move",
		`{"id":"ses_move","projectID":"prj_x","directory":"/d/x","title":"moved-title"}`,
		payload, captured)

	repo := &syncrepo.Repo{Path: root}
	n, err := applyMigration(repo, false, false)
	if err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	if n != 1 {
		t.Fatalf("migrated %d, want 1", n)
	}
	if got := commitCount(t, root); got != 1 {
		t.Fatalf("want exactly ONE migration commit, got %d", got)
	}
	out, err := exec.Command("git", "-C", root, "log", "-1", "--format=%s").Output()
	if err != nil {
		t.Fatal(err)
	}
	if msg := strings.TrimSpace(string(out)); !strings.Contains(msg, "sync: opencode migrate-layout v") {
		t.Errorf("commit message = %q, want sync: opencode migrate-layout v<N>", msg)
	}

	// Payload moved with identical bytes.
	digest := mustDigest(payload)
	newPayload := filepath.Join(root, filepath.FromSlash(revision.Path("km", "ses_move", digest)))
	got, err := os.ReadFile(newPayload)
	if err != nil {
		t.Fatalf("migrated payload missing: %v", err)
	}
	if string(got) != string(payload) {
		t.Error("migrated payload bytes differ from source export")
	}
	// Sidecar records honest migrated provenance.
	metaBytes, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(revision.MetaPath("km", "ses_move", digest))))
	if err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}
	var meta revision.Meta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Status != revision.StatusMigrated ||
		meta.OriginalSessionID != "ses_move" ||
		meta.Digest != digest ||
		meta.SchemaVersion != revision.SchemaVersion {
		t.Errorf("sidecar identity wrong: %#v", meta)
	}
	if meta.SourceDeviceID != "" || meta.DeviceAlias != "" {
		t.Errorf("migrated sidecar must carry no invented provenance: %#v", meta)
	}
	// Info fields map from the export payload's validated info block
	// (exportFixture: prj_ses_move / /d/ses_move / title-ses_move), NOT from
	// the disagreeing import-meta sibling planted above.
	if meta.ProducerVersion != "1.18.18" || meta.ProjectID != "prj_ses_move" ||
		meta.Directory != "/d/ses_move" || meta.Title != "title-ses_move" {
		t.Errorf("sidecar info mapping wrong: %#v", meta)
	}
	if meta.CapturedAt.Unix() != captured.UTC().Unix() {
		t.Errorf("captured_at = %s, want file mtime %s", meta.CapturedAt, captured.UTC())
	}
	// Legacy pair removed.
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy payload must be removed after success (err: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "opencode", "km", "import-meta", "ses_move.json")); !os.IsNotExist(err) {
		t.Errorf("legacy import-meta sibling must be removed after success (err: %v)", err)
	}
}

func TestRunMigrationIdempotentSecondRunMigratesZero(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	plantLegacyExport(t, root, "ki", "ses_idem", "", exportFixture("ses_idem"), time.Now())

	repo := &syncrepo.Repo{Path: root}
	if n, err := applyMigration(repo, false, true); err != nil || n != 1 {
		t.Fatalf("first run: n=%d err=%v; want 1, nil", n, err)
	}
	before := commitCount(t, root)

	n, err := applyMigration(repo, false, true)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n != 0 {
		t.Fatalf("idempotent second run migrated %d, want 0", n)
	}
	if got := commitCount(t, root); got != before {
		t.Errorf("second run created extra commits: before=%d after=%d", before, got)
	}
}

// A crash between Write and Remove leaves destination + legacy copy in place.
// Re-running must complete the move silently instead of erroring or rewriting.
func TestRunMigrationCompletesInterruptedMove(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	payload := exportFixture("ses_crash")
	digest := mustDigest(payload)
	// Pre-existing destination with IDENTICAL bytes (as revision.Write left it).
	writeFileFixture(t,
		filepath.Join(root, filepath.FromSlash(revision.Path("kc", "ses_crash", digest))),
		string(payload))
	plantLegacyExport(t, root, "kc", "ses_crash", "", payload, time.Now())

	repo := &syncrepo.Repo{Path: root}
	n, err := applyMigration(repo, false, true)
	if err != nil {
		t.Fatalf("interrupted-move completion failed: %v", err)
	}
	if n != 1 {
		t.Fatalf("migrated %d, want 1 (legacy removal completes the move)", n)
	}
	if _, err := os.Stat(filepath.Join(root, "opencode", "kc", "export", "ses_crash.json")); !os.IsNotExist(err) {
		t.Errorf("legacy copy should now be gone (err: %v)", err)
	}
}

func TestRunMigrationInvalidLegacySkippedWarnedAndLeft(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	validPayload := exportFixture("ses_ok")
	plantLegacyExport(t, root, "kv", "ses_ok", "", validPayload, time.Now())
	invalidPayload := []byte(`{"info":{"id":"ses_bad"`) // truncated JSON
	invalidPath := plantLegacyExport(t, root, "kv", "ses_bad", "", invalidPayload, time.Now())
	mismatchedPayload := exportFixture("ses_other") // info.id != filename stem
	mismatchedPath := plantLegacyExport(t, root, "kv", "ses_wrong", "", mismatchedPayload, time.Now())

	repo := &syncrepo.Repo{Path: root}
	var warnOut string
	n, err := func() (int, error) {
		var runN int
		var runErr error
		warnOut = captureStderr(t, func() { runN, runErr = applyMigration(repo, false, false) })
		return runN, runErr
	}()
	if err != nil {
		t.Fatalf("apply with invalid artifacts: %v", err)
	}
	if n != 1 {
		t.Fatalf("only the one valid export migrates, got %d", n)
	}
	for _, p := range []string{invalidPath, mismatchedPath} {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Errorf("invalid legacy file must never be deleted: %s (%v)", p, statErr)
		}
	}
	if rel := relToRoot(root, invalidPath); !strings.Contains(warnOut, "skipping invalid legacy export: "+rel) {
		t.Errorf("warning naming invalid path missing:\n%s", warnOut)
	}
	if !strings.Contains(warnOut, "not valid JSON") {
		t.Errorf("warning must name the reason:\n%s", warnOut)
	}
	if !strings.Contains(warnOut, relToRoot(root, mismatchedPath)) {
		t.Errorf("mismatched-id export must also be warned and kept:\n%s", warnOut)
	}
	// The commit contains only the successful migration.
	out, err := exec.Command("git", "-C", root, "ls-files").Output()
	if err != nil {
		t.Fatal(err)
	}
	listed := string(out)
	if strings.Contains(listed, "ses_bad") || strings.Contains(listed, "ses_wrong") {
		t.Errorf("invalid artifacts must stay out of history:\n%s", listed)
	}
	if !strings.Contains(listed, "sessions/ses_ok/revisions/") {
		t.Errorf("successful migration not committed:\n%s", listed)
	}
}

// End-to-end through the CLI surface: flag parsing, config wiring, summaries.
func TestCmdMigrateLayoutDryRunAndApply(t *testing.T) {
	isolateDeviceEnv(t)
	root := filepath.Join(os.Getenv("HOME"), "sync-repo")
	initTestGitRepo(t, root)
	plantLegacyExport(t, root, "ke", "ses_cli", "", exportFixture("ses_cli"), time.Now())
	writeTestConfig(t, root)
	resetMigrationGuard()

	var dryOut string
	func() {
		var err error
		dryOut = captureStdout(t, func() { err = cmdMigrateLayout([]string{"--dry-run"}) })
		if err != nil {
			t.Errorf("dry-run cmd: %v", err)
		}
	}()
	if !strings.Contains(dryOut, "would migrate 1 artifact(s)") {
		t.Errorf("dry-run summary missing:\n%s", dryOut)
	}
	if _, err := os.Stat(filepath.Join(root, "opencode", "ke", "export", "ses_cli.json")); err != nil {
		t.Errorf("dry-run must keep the legacy file: %v", err)
	}

	var applyOut string
	func() {
		var err error
		applyOut = captureStdout(t, func() { err = cmdMigrateLayout(nil) })
		if err != nil {
			t.Errorf("apply cmd: %v", err)
		}
	}()
	if !strings.Contains(applyOut, "migrated 1 artifact(s)") {
		t.Errorf("apply summary missing:\n%s", applyOut)
	}
	if got := commitCount(t, root); got != 1 {
		t.Errorf("apply produced %d commits, want 1", got)
	}

	// Unknown flags are rejected loudly.
	if err := cmdMigrateLayout([]string{"--bogus"}); err == nil {
		t.Error("unknown migrate-layout flag must fail")
	}
}

func TestMigrateIfNeededSilentFreshJoin(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	plantLegacyExport(t, root, "kf", "ses_fj1", "", exportFixture("ses_fj1"), time.Now())
	plantLegacyExport(t, root, "kf", "ses_fj2", "", exportFixture("ses_fj2"), time.Now())
	repo := &syncrepo.Repo{Path: root}

	var out string
	err := func() error {
		var runErr error
		out = captureStdout(t, func() { runErr = migrateIfNeeded(repo) })
		return runErr
	}()
	if err != nil {
		t.Fatalf("silent migration: %v", err)
	}
	lines := nonEmptyLines(out)
	if len(lines) != 1 || lines[0] != "migrated 2 legacy artifact(s) to revisions layout" {
		t.Fatalf("exactly ONE notice line required, got %q", out)
	}
	if got := commitCount(t, root); got != 1 {
		t.Errorf("silent migration must commit once, got %d commits", got)
	}
	if _, err := os.Stat(filepath.Join(root, "opencode", "kf", "export", "ses_fj1.json")); !os.IsNotExist(err) {
		t.Errorf("legacy file survived silent migration: %v", err)
	}
}

func TestMigrateIfNeededMixedStateHintsOnly(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	plantLegacyExport(t, root, "kg", "ses_mixed_legacy", "", exportFixture("ses_mixed_legacy"), time.Now())
	dataA := exportFixture("ses_mixed_new")
	writeRevisionFixture(t, root, "kg", "ses_mixed_new", "", "(untitled)", time.Now(), "1.18.18", dataA)
	repo := &syncrepo.Repo{Path: root}

	var out string
	err := func() error {
		var runErr error
		out = captureStdout(t, func() { runErr = migrateIfNeeded(repo) })
		return runErr
	}()
	if err != nil {
		t.Fatalf("mixed-state check: %v", err)
	}
	lines := nonEmptyLines(out)
	if len(lines) != 1 ||
		lines[0] != "hint: run agent-sync migrate-layout to finish migrating 1 remaining legacy artifact(s)" {
		t.Fatalf("exactly ONE hint line required, got %q", out)
	}
	// Mixed state changes NOTHING.
	if _, err := os.Stat(filepath.Join(root, "opencode", "kg", "export", "ses_mixed_legacy.json")); err != nil {
		t.Errorf("mixed state must keep the legacy file in place: %v", err)
	}
	if got := commitCount(t, root); got != 0 {
		t.Errorf("hint-only run must not commit, got %d commits", got)
	}
}

func TestMigrateIfNeededSilentOnCleanRepo(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	writeRevisionFixture(t, root, "kh", "ses_clean", "", "(untitled)", time.Now(), "1.18.18", exportFixture("ses_clean"))
	repo := &syncrepo.Repo{Path: root}

	var out string
	err := func() error {
		var runErr error
		out = captureStdout(t, func() { runErr = migrateIfNeeded(repo) })
		return runErr
	}()
	if err != nil {
		t.Fatalf("clean repo check: %v", err)
	}
	if out != "" {
		t.Errorf("clean repo must print nothing, got %q", out)
	}
	if got := commitCount(t, root); got != 0 {
		t.Errorf("clean repo must not commit, got %d commits", got)
	}
}

// Integration through receive: the fresh-join notice appears exactly once per
// process even though resume composes receive (guard), and post-pull state is
// what gets classified (Remote empty here → no pull, straight to migration).
func TestCmdReceiveRunsFreshJoinMigrationOnce(t *testing.T) {
	isolateDeviceEnv(t)
	syncRepo := filepath.Join(os.Getenv("HOME"), "sync-repo")
	initTestGitRepo(t, syncRepo)
	plantLegacyExport(t, syncRepo, "kj", "ses_recv_mig", "", exportFixture("ses_recv_mig"), time.Now())
	writeTestConfig(t, syncRepo) // no remote → PullForced skipped
	resetMigrationGuard()

	var out string
	func() {
		var err error
		out = captureStdout(t, func() { err = cmdReceive(nil) })
		if err != nil {
			t.Errorf("receive: %v", err)
		}
	}()
	if got := strings.Count(out, "migrated 1 legacy artifact(s) to revisions layout"); got != 1 {
		t.Fatalf("notice line count = %d, want exactly 1:\n%s", got, out)
	}
	if _, err := os.Stat(filepath.Join(syncRepo, "opencode", "kj", "export", "ses_recv_mig.json")); !os.IsNotExist(err) {
		t.Errorf("legacy file must be gone after receive-triggered migration: %v", err)
	}

	// Dry-run receive must NOT migrate anything.
	resetMigrationGuard()
	plantLegacyExport(t, syncRepo, "kj2", "ses_recv_dry", "", exportFixture("ses_recv_dry"), time.Now())
	func() {
		var err error
		out = captureStdout(t, func() { err = cmdReceive([]string{"--dry-run"}) })
		if err != nil {
			t.Errorf("receive --dry-run: %v", err)
		}
	}()
	if strings.Contains(out, "migrated") {
		t.Errorf("dry-run receive must not migrate:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(syncRepo, "opencode", "kj2", "export", "ses_recv_dry.json")); err != nil {
		t.Errorf("dry-run receive must keep the legacy file: %v", err)
	}
}

// nonEmptyLines splits s and drops blank lines for exact-output assertions.
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
