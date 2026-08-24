package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentsync/internal/canonicalkey"
	"agentsync/internal/receivestate"
	"agentsync/internal/revision"
)

// captureStdout swaps os.Stdout for a pipe while fn runs, returning everything
// printed (cmdReceive reports through fmt.Printf directly).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		var sb strings.Builder
		_, _ = io.Copy(&sb, r)
		done <- sb.String()
	}()
	defer func() { os.Stdout = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return <-done
}

// isolateDeviceEnv redirects config/cache/home resolution into a throwaway
// directory so tests never touch real user state.
func isolateDeviceEnv(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	return base
}

func writeTestConfig(t *testing.T, repoPath string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agent-sync")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[sync]\nrepo_path = %q\n", repoPath)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// plantFakeClone builds a directory that passes repoindex.ValidateCandidate:
// a `.git` dir whose config carries an origin URL (parsed without shelling
// out by canonicalkey.Resolve).
func plantFakeClone(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "[remote \"origin\"]\n\turl = git@github.com:agentsync/fixture-project.git\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeRevisionFixture plants one immutable revision payload plus sidecar in
// the sync repo, mirroring what send/Mirror produce.
func writeRevisionFixture(t *testing.T, syncRepo, key, sid, alias, title string, captured time.Time, producer string, payload []byte) {
	t.Helper()
	digest := mustDigest(payload)
	revDir := filepath.Join(syncRepo, "opencode", key, "sessions", sid, "revisions")
	writeFileFixture(t, filepath.Join(revDir, digest+".json"), string(payload))
	deviceID := alias
	if deviceID == "" {
		deviceID = "dev-" + sid
	}
	meta := revision.Meta{
		SchemaVersion:     revision.SchemaVersion,
		OriginalSessionID: sid,
		Digest:            digest,
		SourceDeviceID:    deviceID,
		DeviceAlias:       alias,
		CapturedAt:        captured,
		ProducerVersion:   producer,
		Status:            revision.StatusCaptured,
		Title:             title,
	}
	blob, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	writeFileFixture(t, filepath.Join(revDir, digest+".meta.json"), string(blob))
}

func loadOutcomes(t *testing.T) []receivestate.Outcome {
	t.Helper()
	statePath, err := receivestate.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := receivestate.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	return outcomes
}

// conflictReceiveEnv builds one device env with a sync repo holding a
// two-revision conflict (ses_conf) plus an unrelated session with no local
// clone (ses_solo), and one indexed local clone matching ses_conf's key.
func conflictReceiveEnv(t *testing.T) (base, clone, key string) {
	t.Helper()
	base = isolateDeviceEnv(t)
	syncRepo := filepath.Join(base, "sync-repo")
	clones := filepath.Join(base, "clones")

	// syncrepo.Exists gates on an initialized repo (.git present).
	if err := os.MkdirAll(filepath.Join(syncRepo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	clone = plantFakeClone(t, clones, "fixture-worktree")
	key = string(canonicalkey.Resolve(clone))

	laptopTime := time.Date(2026, 8, 20, 14, 2, 11, 0, time.UTC)
	desktopTime := time.Date(2026, 8, 21, 9, 31, 45, 0, time.UTC)
	writeRevisionFixture(t, syncRepo, key, "ses_conf", "laptop", "Shared conversation", laptopTime, "1.18.18",
		exportFixture("ses_conf"))
	writeRevisionFixture(t, syncRepo, key, "ses_conf", "desktop", "Shared conversation", desktopTime, "1.18.18",
		[]byte(`{"info":{"id":"ses_conf","projectID":"prj_ses_conf","directory":"/d/ses_conf","version":"1.18.18"},"v":2}`))

	// A clean session whose canonical key has no local clone anywhere.
	writeRevisionFixture(t, syncRepo, "no-clone-key", "ses_solo", "", "(untitled)", laptopTime, "1.18.18",
		exportFixture("ses_solo"))

	writeTestConfig(t, syncRepo)

	idx, err := openRepoIndex()
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Scan(context.Background(), []string{clones}, nil); err != nil {
		t.Fatal(err)
	}
	return base, clone, key
}

// The core WS-B stage-3/4 behavior end to end: a conflicted session is
// reported explicitly, nothing is restored or marked verified/busy, every
// revision lands archive-only per digest×clone, and a repeated run produces
// the IDENTICAL report with zero state churn.
func TestCmdReceiveConflictGroupArchivesOnlyRepeatably(t *testing.T) {
	conflictReceiveEnv(t)

	var out1 string
	func() {
		var runErr error
		out1 = captureStdout(t, func() { runErr = cmdReceive(nil) })
		if runErr != nil {
			t.Errorf("receive run 1: %v", runErr)
		}
	}()

	for _, want := range []string{
		"CONFLICT: ses_conf has 2 preserved revisions — nothing restored",
		"laptop 2026-08-20 14:02 UTC",
		"desktop 2026-08-21 09:31 UTC",
		"last modified by desktop at 2026-08-21 09:31 UTC",
		"Run `agent-sync recover ses_conf` to restore a chosen revision.",
		"archive-only:",
		"Archived only: ses_solo",
	} {
		if !strings.Contains(out1, want) {
			t.Errorf("run 1 output missing %q:\n%s", want, out1)
		}
	}
	if strings.Contains(out1, "Writing back") || strings.Contains(out1, "would write back") {
		t.Errorf("conflicted session must never be written back:\n%s", out1)
	}

	outcomes := loadOutcomes(t)
	if len(outcomes) != 2 {
		t.Fatalf("want exactly 2 archive-only outcomes (one per digest×clone), got %d: %#v", len(outcomes), outcomes)
	}
	digests := map[string]bool{}
	for _, o := range outcomes {
		if o.Status != receivestate.StatusArchiveOnly {
			t.Errorf("status = %q, want archive-only (%#v)", o.Status, o)
		}
		if o.SessionID != "ses_conf" {
			t.Errorf("session = %q, want ses_conf", o.SessionID)
		}
		if o.Attempts != 0 || !o.NextAttempt.IsZero() {
			t.Errorf("archive-only must carry no retry state: %#v", o)
		}
		digests[o.ArtifactDigest] = true
	}
	if len(digests) != 2 {
		t.Errorf("outcomes must be keyed per DISTINCT digest, got %d: %#v", len(digests), outcomes)
	}

	// Repeat: identical report, zero state churn.
	var out2 string
	func() {
		var runErr error
		out2 = captureStdout(t, func() { runErr = cmdReceive(nil) })
		if runErr != nil {
			t.Errorf("receive run 2: %v", runErr)
		}
	}()
	if out2 != out1 {
		t.Errorf("repeat run diverged.\nrun1:\n%s\nrun2:\n%s", out1, out2)
	}
	after := loadOutcomes(t)
	for i := range outcomes {
		if i >= len(after) {
			t.Fatalf("outcome count changed between runs: %d -> %d", len(outcomes), len(after))
		}
		if !outcomes[i].LastAttempt.Equal(after[i].LastAttempt) ||
			outcomes[i].Attempts != after[i].Attempts ||
			outcomes[i].Status != after[i].Status {
			t.Errorf("state churn on repeat run:\nbefore %#v\nafter  %#v", outcomes[i], after[i])
		}
	}
}

// Dry-run prints the conflict report and what WOULD happen, but changes
// nothing — no receive-state file may appear.
func TestCmdReceiveDryRunLeavesNoTrace(t *testing.T) {
	conflictReceiveEnv(t)

	var out string
	func() {
		var runErr error
		out = captureStdout(t, func() { runErr = cmdReceive([]string{"--dry-run"}) })
		if runErr != nil {
			t.Errorf("dry run: %v", runErr)
		}
	}()

	for _, want := range []string{
		"CONFLICT: ses_conf has 2 preserved revisions — nothing restored",
		"DRY RUN: would mark 2 revision(s) of ses_conf archive-only; nothing restored.",
		"Dry run complete; no OpenCode or local receive state was changed.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}

	statePath, err := receivestate.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Errorf("dry run must not create %s (stat err: %v)", statePath, statErr)
	}
}

func TestRecordConflictArchiveOnlyPreservesRecoveredRevision(t *testing.T) {
	base, clone, _ := conflictReceiveEnv(t)

	refs, err := findRevisions(filepath.Join(base, "sync-repo"))
	if err != nil {
		t.Fatal(err)
	}
	var conflicted conflictGroup
	found := false
	for _, g := range buildConflictGroups(refs) {
		if g.Group.Conflicted && g.Group.SessionID == "ses_conf" {
			conflicted = g
			found = true
			break
		}
	}
	if !found || len(conflicted.Group.Revisions) != 2 {
		t.Fatalf("expected one 2-revision conflicted group for ses_conf")
	}

	statePath, err := receivestate.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	local, err := receivestate.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := openRepoIndex()
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a prior explicit `recover` of the first revision at the clone.
	first := conflicted.Group.Revisions[0].Digest
	if err := local.Put(receivestate.Outcome{
		ArtifactDigest: first,
		SessionID:      conflicted.Group.SessionID,
		CandidatePath:  clone,
		Status:         receivestate.StatusVerified,
	}); err != nil {
		t.Fatal(err)
	}

	var out string
	out = captureStdout(t, func() { recordConflictArchiveOnly(local, idx, conflicted) })

	got, ok, err := local.Get(first, clone)
	if err != nil || !ok {
		t.Fatalf("recovered outcome missing: ok=%v err=%v", ok, err)
	}
	if got.Status != receivestate.StatusVerified {
		t.Errorf("recover's verified outcome must survive receive, got %q", got.Status)
	}
	if !strings.Contains(out, "restored") {
		t.Errorf("expected a 'restored (kept)' line, got:\n%s", out)
	}
	sibling := conflicted.Group.Revisions[1].Digest
	sib, ok, err := local.Get(sibling, clone)
	if err != nil || !ok {
		t.Fatalf("sibling outcome missing: ok=%v err=%v", ok, err)
	}
	if sib.Status != receivestate.StatusArchiveOnly {
		t.Errorf("sibling must stay archive-only, got %q", sib.Status)
	}
}
