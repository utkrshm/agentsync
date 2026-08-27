package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/canonicalkey"
	"agentsync/internal/config"
	"agentsync/internal/receivestate"
	"agentsync/internal/syncrepo"
)

// newFakeSyncAdapter serves hermetic export payloads and session rows for
// sync without any real opencode binary; every command-facing shell-out point
// of the adapter surface is injected.
func newFakeSyncAdapter(t *testing.T, exports map[string][]byte, rows []opencodeSessionRow) *opencode.Adapter {
	t.Helper()
	base := t.TempDir()
	return &opencode.Adapter{
		Export: func(sessionID, outPath string) error {
			payload, ok := exports[sessionID]
			if !ok {
				return fmt.Errorf("fake adapter: no export for %s", sessionID)
			}
			return os.WriteFile(outPath, payload, 0o600)
		},
		QueryRecent: func(sql string) ([]byte, error) {
			return json.Marshal(rows)
		},
		ImportInto:   func(exportPath, targetDir string) error { return nil },
		PatchImport:  func(exportPath, targetDir, projectKey string) error { return nil },
		ProcessGuard: func(targetPath string) (bool, error) { return false, nil },
		ToolVersion:  func() (string, error) { return "1.18.18", nil },
		BinaryPath:   func() (string, error) { return filepath.Join(base, "fake-opencode"), nil },
		Fingerprint:  func() (string, error) { return strings.Repeat("cd", 32), nil },
		ProducerStateFile: func() (string, error) {
			return filepath.Join(base, "producer.json"), nil
		},
		VerifyImport: func(exportPath, targetDir string) error { return nil },
	}
}

func useSyncAdapter(t *testing.T, ad *opencode.Adapter) {
	t.Helper()
	old := buildWriteBackAdapter
	buildWriteBackAdapter = func(cfg config.Config) *opencode.Adapter { return ad }
	t.Cleanup(func() { buildWriteBackAdapter = old })
}

func syncPayload(sid, dir string, variant int) []byte {
	return []byte(fmt.Sprintf(`{"info":{"id":%q,"projectID":"prj_%s","directory":%q,"version":"1.18.18"},"v":%d}`,
		sid, sid, dir, variant))
}

// syncEnv seeds an isolated device env with a REAL git sync repo, one indexed
// fixture clone, and (returned) its canonical key.
func syncEnv(t *testing.T) (base, clone, repoPath, key string) {
	t.Helper()
	base = isolateDeviceEnv(t)
	writeFileFixture(t, filepath.Join(base, ".gitconfig"),
		"[user]\n\tname = AgentSync Test\n\temail = agentsync-test@example.com\n")
	repoPath = filepath.Join(base, "sync-repo")
	if err := syncrepo.Open(repoPath).Init(); err != nil {
		t.Fatal(err)
	}
	writeTestConfig(t, repoPath)
	clone = plantFakeClone(t, filepath.Join(base, "clones"), "fixture-worktree")
	key = string(canonicalkey.Resolve(clone))
	return base, clone, repoPath, key
}

func aliasesFilePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agent-sync", "project-aliases.toml")
}

// The push phase digest-dedups against already-stored revisions, prints its
// summary BEFORE committing, and lands ONE batched commit for everything.
func TestCmdSyncPushDedupsAndBatchesIntoOneCommit(t *testing.T) {
	_, clone, repoPath, key := syncEnv(t)

	preStored := syncPayload("ses_a", clone, 1)
	writeRevisionFixture(t, repoPath, key, "ses_a", "", "Older capture",
		ts(t, "2026-08-25T08:00:00Z"), "1.18.18", preStored)
	if _, err := syncrepo.Open(repoPath).Commit("opencode", "seed", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	ad := newFakeSyncAdapter(t,
		map[string][]byte{
			"ses_a": preStored,                      // identical bytes → dedup, nothing pushed
			"ses_b": syncPayload("ses_b", clone, 1), // unseen → exactly one new revision
		},
		[]opencodeSessionRow{{ID: "ses_a", Directory: clone}, {ID: "ses_b", Directory: clone}})
	useSyncAdapter(t, ad)

	out, err := runCmdCapture(t, func() error { return cmdSync([]string{clone}) })
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pushing 1 session(s) (1 new revision(s))") {
		t.Errorf("summary wrong (ses_a must dedup):\n%s", out)
	}
	iPush := strings.Index(out, "pushing ")
	iCommit := strings.Index(out, "Committed v")
	if iPush == -1 || iCommit == -1 || iPush > iCommit {
		t.Errorf("push summary must print BEFORE the batch commit:\n%s", out)
	}
	subjects := rekeyCommitSubjects(t, repoPath)
	if len(subjects) != 2 || !strings.Contains(subjects[0], "sync-batch") {
		t.Fatalf("want exactly ONE batched commit on top of seed, got %v", subjects)
	}
	digest := mustDigest(syncPayload("ses_b", clone, 1))
	if _, serr := os.Stat(filepath.Join(repoPath, "opencode", key, "sessions", "ses_b", "revisions", digest+".json")); serr != nil {
		t.Errorf("new revision artifact missing: %v", serr)
	}
	if strings.Count(out, "Committed v") != 1 {
		t.Errorf("sync must never fan out into per-session commits:\n%s", out)
	}
	if !strings.Contains(out, "No remote configured — commit is local-only.") {
		t.Errorf("missing local-only remote note:\n%s", out)
	}
}

// A second identical run is a clean no-op: digest dedup blocks pushing,
// already-verified outcomes block re-importing, and the close-out reports all
// zeros. The first run exercises the real import-back path using a FOREIGN
// clone row (session whose live copy lives in another directory resolving to
// the same canonical key).
func TestCmdSyncSecondRunIsCleanNoOp(t *testing.T) {
	base, clone, repoPath, key := syncEnv(t)
	foreignCloneDir := plantFakeClone(t, filepath.Join(base, "clones"), "second-worktree")

	writeRevisionFixture(t, repoPath, key, "ses_remote", "", "Arrived via pull",
		ts(t, "2026-08-25T08:00:00Z"), "1.18.18", syncPayload("ses_remote", foreignCloneDir, 3))

	ad := newFakeSyncAdapter(t,
		map[string][]byte{"ses_own": syncPayload("ses_own", clone, 7)},
		[]opencodeSessionRow{{ID: "ses_own", Directory: clone}})
	useSyncAdapter(t, ad)

	out1, err := runCmdCapture(t, func() error { return cmdSync([]string{clone}) })
	if err != nil {
		t.Fatalf("run 1: %v\n%s", err, out1)
	}
	if !strings.Contains(out1, "done: pushed 1 session(s)/1 revision(s), imported 1, resolved 0 conflict(s), skipped 0") {
		t.Fatalf("run 1 close-out wrong:\n%s", out1)
	}
	out2, err := runCmdCapture(t, func() error { return cmdSync([]string{clone}) })
	if err != nil {
		t.Fatalf("run 2: %v\n%s", err, out2)
	}
	// The pre-existing ses_remote fixture folds into run 1's single batch
	// commit; run 2 adds NOTHING.
	if subjects := rekeyCommitSubjects(t, repoPath); len(subjects) != 1 {
		t.Fatalf("want exactly one batched commit across both runs, got %v", subjects)
	}
	if !strings.Contains(out2, "nothing new to push") ||
		!strings.Contains(out2, "done: pushed 0 session(s)/0 revision(s), imported 0, resolved 0 conflict(s), skipped 0") {
		t.Errorf("second run must be a clean no-op:\n%s", out2)
	}
}

// Row filtering resolves each row's directory individually, so a pinned alias
// pulls historical rows under the pinned key too.
func TestLocalSessionIDsResolvesPerRowIncludingPinnedAlias(t *testing.T) {
	isolateDeviceEnv(t)
	dirA := plantFakeClone(t, t.TempDir(), "remote-backed")

	pinnedDir := filepath.Join(os.Getenv("HOME"), "no-git-dir")
	os.MkdirAll(pinnedDir, 0o700)
	cfgDir, _ := config.Path()
	pinFile := filepath.Join(filepath.Dir(cfgDir), "project-aliases.toml")
	os.MkdirAll(filepath.Dir(pinFile), 0o700)
	if err := os.WriteFile(pinFile, []byte(fmt.Sprintf("%q = \"pinned-key\"\n", pinnedDir)), 0o600); err != nil {
		t.Fatal(err)
	}

	rows := []opencodeSessionRow{
		{ID: "r1", Directory: dirA},
		{ID: "r2", Directory: pinnedDir}, // aliased — same treatment as live captures
		{ID: "r3", Directory: "/some/unrelated/project"},
		{ID: "", Directory: dirA}, // blank ids never count
	}
	key := string(canonicalkey.Resolve(dirA))
	if got := localSessionIDs(rows, key); !reflect.DeepEqual(got, []string{"r1"}) {
		t.Errorf("key-filtered rows = %v, want [r1]", got)
	}
	if got := localSessionIDs(rows, "pinned-key"); !reflect.DeepEqual(got, []string{"r2"}) {
		t.Errorf("pinned-key-filtered rows = %v, want [r2]", got)
	}
}

// The pin writer produces EXACTLY the canonical alias parser's format and is
// idempotent on identical re-pin. Format compatibility is proven by running
// the PRODUCTION parser over our file, not by eyeballing quotes.
func TestAppendAliasPinFormatMatchesCanonicalParser(t *testing.T) {
	isolateDeviceEnv(t)
	dir := filepath.Join(os.Getenv("HOME"), "plain", "dirx")
	os.MkdirAll(dir, 0o700)
	path := aliasesFilePath(t)

	const alias = "pinned-name"
	if err := appendAliasPin(path, dir, alias); err != nil {
		t.Fatal(err)
	}
	data := mustRead(t, path)
	if !strings.Contains(string(data), fmt.Sprintf("%q = %q\n", dir, alias)) {
		t.Errorf("pin line format wrong:\n%s", data)
	}
	if got := string(canonicalkey.Resolve(dir)); got != alias {
		t.Errorf("canonical parser read %q from our pin", got)
	}
	if err := appendAliasPin(path, dir, alias); err != nil {
		t.Fatalf("identical re-pin must be idempotent: %v", err)
	}
	if c := strings.Count(string(mustRead(t, path)), fmt.Sprintf("%q =", dir)); c != 1 {
		t.Errorf("duplicate pin lines appeared (%d)", c)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TTY unmapped flow: answering the prompt pins a PERMANENT identity, and
// declining warns-and-continues WITHOUT writing anything.
func TestCmdSyncUnmappedTTYPinAndDecline(t *testing.T) {
	base, _, repoPath, _ := syncEnv(t)
	stray := filepath.Join(base, "stray-project")
	os.MkdirAll(stray, 0o700)
	aliasPath := aliasesFilePath(t)

	oldTTY, oldPrompt := stdinIsTTY, readPromptLine
	stdinIsTTY = func() bool { return true }
	defer func() { stdinIsTTY, readPromptLine = oldTTY, oldPrompt }()

	t.Run("decline keeps island but continues", func(t *testing.T) {
		readPromptLine = func(string) string { return "" }
		ad := newFakeSyncAdapter(t, map[string][]byte{}, nil)
		useSyncAdapter(t, ad)
		out, err := runCmdCapture(t, func() error { return cmdSync([]string{stray}) })
		if err != nil {
			t.Fatalf("declined sync must warn-and-continue, not fail: %v\n%s", err, out)
		}
		if !strings.Contains(out, "_unmapped/") || !strings.Contains(out, "warn:") {
			t.Errorf("expected acknowledged-island warning:\n%s", out)
		}
		if _, statErr := os.Stat(aliasPath); !os.IsNotExist(statErr) {
			t.Errorf("decline must NOT write an alias pin (stat: %v)", statErr)
		}
	})

	t.Run("prompt answer pins permanently", func(t *testing.T) {
		readPromptLine = func(string) string { return "pinned-project" }
		ad := newFakeSyncAdapter(t,
			map[string][]byte{"ses_pin": syncPayload("ses_pin", stray, 1)},
			[]opencodeSessionRow{{ID: "ses_pin", Directory: stray}})
		useSyncAdapter(t, ad)

		out, err := runCmdCapture(t, func() error { return cmdSync([]string{stray}) })
		if err != nil {
			t.Fatalf("pinning sync failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "identity pinned; nothing can be orphaned anymore") {
			t.Errorf("pin notice missing:\n%s", out)
		}
		line := fmt.Sprintf("%q = %q\n", stray, "pinned-project")
		if got := mustRead(t, aliasPath); !strings.Contains(string(got), line) {
			t.Errorf("alias file missing pin line %q:\n%s", line, got)
		}
		found := false
		refs, ferr := findRevisions(repoPath)
		if ferr != nil {
			t.Fatal(ferr)
		}
		for _, r := range refs {
			if r.Key == "pinned-project" && r.SessionID == "ses_pin" {
				found = true
			}
		}
		if !found {
			t.Error("revision did not land under the pinned key")
		}
	})
}

// Non-TTY runs get fix instructions WITHOUT interaction or writes. go test
// wires os.Stdin to /dev/null — a character device — so the TTY probe is
// explicitly forced false here to model a real non-terminal run.
func TestCmdSyncUnmappedNonTTYWarnsAndContinues(t *testing.T) {
	base, _, _, _ := syncEnv(t)
	stray := filepath.Join(base, "noninteractive")
	os.MkdirAll(stray, 0o700)
	aliasPath := aliasesFilePath(t)

	oldTTY := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	defer func() { stdinIsTTY = oldTTY }()

	ad := newFakeSyncAdapter(t, map[string][]byte{}, nil)
	useSyncAdapter(t, ad)
	out, err := runCmdCapture(t, func() error { return cmdSync([]string{stray}) })
	if err != nil {
		t.Fatalf("non-tty sync must continue: %v\n%s", err, out)
	}
	if !strings.Contains(out, "risk becoming an orphaned island") ||
		!strings.Contains(out, "project-aliases.toml") {
		t.Errorf("warn-and-continue block incomplete:\n%s", out)
	}
	if _, statErr := os.Stat(aliasPath); !os.IsNotExist(statErr) {
		t.Errorf("non-TTY flow must not write pins (stat: %v)", statErr)
	}
}

// --dry-run projects the full plan with identical counts while mutating
// NOTHING — the fs diff proves it down to the sync-repo internals.
func TestCmdSyncDryRunZeroMutationsWithIdenticalCounts(t *testing.T) {
	base, clone, repoPath, key := syncEnv(t)
	foreignCloneDir := plantFakeClone(t, filepath.Join(base, "clones"), "second-worktree")

	// Stored sessions: ses_a for the digest-dedup path (identical export
	// bytes exist in another same-key clone), ses_ghost stored with NO
	// OpenCode row anywhere → the genuine write-back candidate.
	writeRevisionFixture(t, repoPath, key, "ses_a", "", "Stored already",
		ts(t, "2026-08-25T08:00:00Z"), "1.18.18", syncPayload("ses_a", foreignCloneDir, 1))
	writeRevisionFixture(t, repoPath, key, "ses_ghost", "", "Ghost from far away",
		ts(t, "2026-08-25T07:00:00Z"), "1.18.18", syncPayload("ses_ghost", foreignCloneDir, 9))
	if _, err := syncrepo.Open(repoPath).Commit("opencode", "seed", "2026-08-25T09:00:00Z"); err != nil {
		t.Fatal(err)
	}

	ad := newFakeSyncAdapter(t,
		map[string][]byte{
			"ses_a":      syncPayload("ses_a", foreignCloneDir, 1), // dedup
			"ses_b":      syncPayload("ses_b", clone, 1),           // would push (new revision)
			"ses_remote": syncPayload("ses_remote", foreignCloneDir, 3),
		},
		[]opencodeSessionRow{
			{ID: "ses_a", Directory: clone},
			{ID: "ses_b", Directory: clone},
			{ID: "ses_remote", Directory: foreignCloneDir}, // present locally in another clone → NOT re-imported
			// no ses_ghost row anywhere: only storage knows it
		})
	useSyncAdapter(t, ad)

	before := snapshotTree(t, base)
	out, err := runCmdCapture(t, func() error { return cmdSync([]string{clone, "--dry-run"}) })
	if err != nil {
		t.Fatalf("dry-run sync: %v\n%s", err, out)
	}
	if after := snapshotTree(t, base); !reflect.DeepEqual(before, after) {
		var diff []string
		for k, v := range before {
			if after[k] != v {
				diff = append(diff, "changed/removed: "+k)
			}
		}
		for k := range after {
			if before[k] == "" {
				diff = append(diff, "added: "+k)
			}
		}
		t.Fatalf("dry-run mutated the filesystem: %v", diff)
	}
	for _, want := range []string{
		"DRY RUN: pushing 2 session(s) (2 new revision(s))",
		"DRY RUN: would write back ses_ghost (Ghost from far away)",
		"done: pushed 2 session(s)/2 revision(s), imported 1, resolved 0 conflict(s), skipped 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "identity pinned") || strings.Contains(out, "would prompt to pin") {
		t.Errorf("dry-run over a MAPPED folder must never reach identity handling:\n%s", out)
	}

	// The real run afterwards performs exactly what was projected.
	outReal, err := runCmdCapture(t, func() error { return cmdSync([]string{clone}) })
	if err != nil {
		t.Fatalf("real sync: %v\n%s", err, outReal)
	}
	if !strings.Contains(outReal, "pushing 2 session(s) (2 new revision(s))") ||
		!strings.Contains(outReal, "imported 1") {
		t.Errorf("real run diverged from dry-run plan:\n%s", outReal)
	}
}

// --dry-run over an unmapped folder reports pinning as a WOULD-action only,
// even though stdin pretends to be a TTY that would answer.
func TestCmdSyncDryRunUnmappedSaysWouldPromptToPin(t *testing.T) {
	base, _, _, _ := syncEnv(t)
	stray := filepath.Join(base, "unmapped-dry")
	os.MkdirAll(stray, 0o700)
	aliasPath := aliasesFilePath(t)

	oldTTY, oldPrompt := stdinIsTTY, readPromptLine
	stdinIsTTY = func() bool { return true }
	readPromptLine = func(string) string { t.Error("dry-run must never prompt"); return "" }
	defer func() { stdinIsTTY, readPromptLine = oldTTY, oldPrompt }()

	ad := newFakeSyncAdapter(t, map[string][]byte{}, nil)
	useSyncAdapter(t, ad)
	out, err := runCmdCapture(t, func() error { return cmdSync([]string{stray, "--dry-run"}) })
	if err != nil {
		t.Fatalf("dry-run unmapped: %v\n%s", err, out)
	}
	if !strings.Contains(out, "would prompt to pin") {
		t.Errorf("must report the would-prompt action:\n%s", out)
	}
	if _, statErr := os.Stat(aliasPath); !os.IsNotExist(statErr) {
		t.Errorf("dry-run must NOT write an alias pin (stat: %v)", statErr)
	}
}

// Picker parsing contract: numbers pick, S skips, blank aborts, garbage and
// out-of-range numbers are errors naming valid options.
func TestParseHeadChoice(t *testing.T) {
	tests := []struct {
		in     string
		max    int
		skip   bool
		abort  bool
		pick   int
		errHas string
	}{
		{"2", 3, false, false, 2, ""},
		{"3", 3, false, false, 3, ""},
		{"S", 3, true, false, 0, ""},
		{"s", 3, true, false, 0, ""},
		{"", 3, false, true, 0, ""},
		{"x", 3, false, false, 0, "invalid selection"},
		{"0", 3, false, false, 0, "invalid selection"},
		{"4", 3, false, false, 0, "invalid selection"},
		{"1x", 3, false, false, 0, "invalid selection"},
	}
	for _, tc := range tests {
		skip, abort, pick, err := parseHeadChoice(tc.in, tc.max)
		switch {
		case tc.errHas != "":
			if err == nil || !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("parseHeadChoice(%q) err=%v, want containing %q", tc.in, err, tc.errHas)
			}
		case tc.skip:
			if !skip || abort || err != nil {
				t.Errorf("parseHeadChoice(%q) => skip=%v abort=%v pick=%d err=%v, want skip", tc.in, skip, abort, pick, err)
			}
		case tc.abort:
			if !abort || skip || err != nil {
				t.Errorf("parseHeadChoice(%q) => skip=%v abort=%v pick=%d err=%v, want abort", tc.in, skip, abort, pick, err)
			}
		default:
			if pick != tc.pick || skip || abort || err != nil {
				t.Errorf("parseHeadChoice(%q) => skip=%v abort=%v pick=%d err=%v, want pick %d", tc.in, skip, abort, pick, err, tc.pick)
			}
		}
	}
}

// snapshotTree hashes every file under root into path→sha256 so ANY mutation
// (content OR presence, including deletions and new state files) fails the
// comparison. Mtimes are deliberately ignored.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(data)
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	return out
}

// A conflicted session is never auto-restored by sync: the report prints,
// answering [S]kip leaves BOTH device heads archive-only×target and adds the
// session to the unresolved list.
func TestCmdSyncConflictSkipLeavesEverythingPreserved(t *testing.T) {
	_, clone, repoPath, key := syncEnv(t)

	laptopBytes := syncPayload("ses_c", clone, 1)
	desktopBytes := syncPayload("ses_c", clone, 2)
	// Distinct devices (distinct buckets under DetectV2) keep BOTH heads.
	writeRevisionFixture(t, repoPath, key, "ses_c", "laptop", "Conflict L", ts(t, "2026-08-25T08:00:00Z"), "1.18.18", laptopBytes)
	writeRevisionFixture(t, repoPath, key, "ses_c", "desktop", "Conflict D", ts(t, "2026-08-25T09:00:00Z"), "1.18.18", desktopBytes)
	if _, err := syncrepo.Open(repoPath).Commit("opencode", "seed", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	// No OpenCode rows at all: nothing to push, storage-only conflicts remain.
	ad := newFakeSyncAdapter(t, map[string][]byte{}, nil)
	useSyncAdapter(t, ad)

	oldPrompt := readPromptLine
	readPromptLine = func(string) string { return "S" }
	defer func() { readPromptLine = oldPrompt }()

	out, err := runCmdCapture(t, func() error { return cmdSync([]string{clone}) })
	if err != nil {
		t.Fatalf("conflict sync: %v\n%s", err, out)
	}
	for _, want := range []string{
		"CONFLICT: ses_c has 2 preserved revisions",
		"done: pushed 0 session(s)/0 revision(s), imported 0, resolved 0 conflict(s), skipped 1",
		"agent-sync recover ses_c",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}

	statePath, _ := receivestate.DefaultPath()
	st, _ := receivestate.Open(statePath)
	for _, d := range []string{mustDigest(laptopBytes), mustDigest(desktopBytes)} {
		o, ok, gerr := st.Get(d, clone)
		if gerr != nil || !ok || o.Status != receivestate.StatusArchiveOnly {
			t.Errorf("digest %s… outcome = %#v ok=%v err=%v, want archive-only", d[:8], o, ok, gerr)
		}
	}
}
