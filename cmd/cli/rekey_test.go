package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentsync/internal/revision"
	"agentsync/internal/syncrepo"
)

// rekeyEnv builds an isolated device env whose sync repo is a REAL git
// repository, so commit count/message assertions exercise the actual scoped
// staging pipeline (AGENTS.md testing convention: boundary tests at the real
// I/O edge). The isolated HOME has no git identity of its own, so one is
// seeded hermetically instead of depending on the machine's global config.
func rekeyEnv(t *testing.T) (syncRepo string) {
	t.Helper()
	base := isolateDeviceEnv(t)
	writeFileFixture(t, filepath.Join(base, ".gitconfig"),
		"[user]\n\tname = AgentSync Test\n\temail = agentsync-test@example.com\n")
	syncRepo = filepath.Join(base, "sync-repo")
	repo := syncrepo.Open(syncRepo)
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	writeTestConfig(t, syncRepo)
	return syncRepo
}

// seedRevisionProject writes one session as BOTH storage layouts under key
// (immutable revisions plus legacy export/import-meta stragglers), then
// records a baseline commit so subsequent commits are countable.
func seedRevisionProject(t *testing.T, syncRepo, key, sid string) {
	t.Helper()
	payload := exportFixture(sid)
	meta := revision.Meta{
		SchemaVersion:     revision.SchemaVersion,
		OriginalSessionID: sid,
		Digest:            mustDigest(payload),
		SourceDeviceID:    "dev-seed",
		CapturedAt:        ts(t, "2026-08-25T10:00:00Z"),
		ProducerVersion:   "1.18.18",
		Status:            revision.StatusCaptured,
		Title:             "seed",
	}
	if _, err := revision.Write(syncRepo, key, sid, payload, meta); err != nil {
		t.Fatal(err)
	}
	writeFileFixture(t, filepath.Join(syncRepo, "opencode", key, "export", sid+".json"), string(payload))
	writeFileFixture(t, filepath.Join(syncRepo, "opencode", key, "import-meta", sid+".json"), `{"id":"`+sid+`","title":"legacy"}`)

	repo := syncrepo.Open(syncRepo)
	repo.ValidateArtifact = func(absPath, relPath string) error { return nil }
	tsStr := ts(t, "2026-08-25T11:00:00Z").Format("2006-01-02T15:04:05Z")
	if _, err := repo.Commit("opencode", "seed", tsStr); err != nil {
		t.Fatal(err)
	}
}

func rekeyCommitSubjects(t *testing.T, syncRepo string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", syncRepo, "log", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// The happy path moves sessions AND legacy stragglers under one commit, the
// old tree is gone, sidecars ride along untouched, and history records the
// relocation exactly once.
func TestCmdRekeyMovesWholeSubtreeInOneCommit(t *testing.T) {
	syncRepo := rekeyEnv(t)
	seedRevisionProject(t, syncRepo, "old-key", "ses_1")

	var out string
	var err error
	out, err = runCmdCapture(t, func() error { return cmdRekey([]string{"old-key", "new-key"}) })
	if err != nil {
		t.Fatalf("rekey: %v", err)
	}
	if !strings.Contains(out, "Rekeyed opencode/old-key -> opencode/new-key") ||
		!strings.Contains(out, "Push remains manual") {
		t.Errorf("summary missing from output:\n%s", out)
	}

	for _, moved := range []string{
		filepath.Join("opencode", "new-key", "sessions", "ses_1", "revisions", mustDigest(exportFixture("ses_1"))+".json"),
		filepath.Join("opencode", "new-key", "sessions", "ses_1", "revisions", mustDigest(exportFixture("ses_1"))+".meta.json"),
		filepath.Join("opencode", "new-key", "export", "ses_1.json"),
		filepath.Join("opencode", "new-key", "import-meta", "ses_1.json"),
	} {
		if _, statErr := os.Stat(filepath.Join(syncRepo, moved)); statErr != nil {
			t.Errorf("moved artifact missing: %s (%v)", moved, statErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(syncRepo, "opencode", "old-key")); !os.IsNotExist(statErr) {
		t.Errorf("old tree must be gone entirely (stat err: %v)", statErr)
	}

	sidecar, readErr := os.ReadFile(filepath.Join(syncRepo, "opencode", "new-key", "sessions", "ses_1",
		"revisions", mustDigest(exportFixture("ses_1"))+".meta.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var m revision.Meta
	if jerr := json.Unmarshal(sidecar, &m); jerr != nil || m.SourceDeviceID != "dev-seed" || m.CapturedAt.IsZero() {
		t.Errorf("sidecar must survive the move byte-honest: %q (%v)", sidecar, jerr)
	}

	subjects := rekeyCommitSubjects(t, syncRepo)
	if len(subjects) != 2 {
		t.Fatalf("want seed + exactly ONE rekey commit, got %v", subjects)
	}
	if subjects[0] != "rekey: old-key -> new-key" {
		t.Errorf("HEAD subject = %q, want %q", subjects[0], "rekey: old-key -> new-key")
	}

	refs, ferr := findRevisions(syncRepo)
	if ferr != nil {
		t.Fatal(ferr)
	}
	for _, r := range refs {
		if r.Key == "old-key" {
			t.Errorf("walker still reports old key after rekey: %#v", r)
		}
	}
}

// Nested-slash keys move correctly in both directions (AGENTS.md invariant 12
// regression net): deep-to-flat and _unmapped/<path>-to-permanent.
func TestCmdRekeyNestedSlashKeysBothDirections(t *testing.T) {
	syncRepo := rekeyEnv(t)
	seedRevisionProject(t, syncRepo, "team/server/app-one", "ses_deep")

	if err := cmdRekey([]string{"team/server/app-one", "gh-slug-app"}); err != nil {
		t.Fatalf("deep->flat rekey: %v", err)
	}
	if fi, statErr := os.Stat(filepath.Join(syncRepo, "opencode", "gh-slug-app", "sessions")); statErr != nil || !fi.IsDir() {
		t.Fatalf("deep tree not relocated to slash-free key (%v)", statErr)
	}

	seedRevisionProject(t, syncRepo, "_unmapped/home/user/proj", "ses_island")
	if err := cmdRekey([]string{"_unmapped/home/user/proj", "proj-permanent"}); err != nil {
		t.Fatalf("_unmapped adoption rekey: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(syncRepo, "opencode", "_unmapped", "home", "user", "proj")); !os.IsNotExist(statErr) {
		t.Errorf("island dir must be gone after adopting it (stat err: %v)", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(syncRepo, "opencode", "proj-permanent", "sessions")); statErr != nil {
		t.Errorf("island content missing at permanent key: %v", statErr)
	}
}

// Destination-existence is an absolute refusal — rekey never merges projects.
func TestCmdRekeyRefusesExistingDestination(t *testing.T) {
	syncRepo := rekeyEnv(t)
	seedRevisionProject(t, syncRepo, "src-key", "ses_x")
	if err := os.MkdirAll(filepath.Join(syncRepo, "opencode", "dest-key"), 0o700); err != nil {
		t.Fatal(err)
	}

	err := cmdRekey([]string{"src-key", "dest-key"})
	if err == nil || !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "never merges") {
		t.Fatalf("err = %v, want destination-exists refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(syncRepo, "opencode", "src-key", "sessions")); statErr != nil {
		t.Errorf("refusal must leave source untouched (%v)", statErr)
	}
}

// An _unmapped/ DESTINATION would recreate the very island rekey exists to
// eliminate — refuse before any move.
func TestCmdRekeyRefusesUnmappedDestination(t *testing.T) {
	syncRepo := rekeyEnv(t)
	seedRevisionProject(t, syncRepo, "src-key2", "ses_y")

	err := cmdRekey([]string{"src-key2", "_unmapped/tmp/other"})
	if err == nil || !strings.Contains(err.Error(), "_unmapped/") {
		t.Fatalf("err = %v, want unmapped-destination refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(syncRepo, "opencode", "src-key2", "sessions")); statErr != nil {
		t.Errorf("refusal must leave source untouched (%v)", statErr)
	}
}

// A missing source errors with actionable context listing known keys (which
// makes a post-rekey re-run converge into this clear no-op refusal path).
func TestCmdRekeyMissingSourceListsKnownKeys(t *testing.T) {
	syncRepo := rekeyEnv(t)
	seedRevisionProject(t, syncRepo, "known-a", "ses_a")
	seedRevisionProject(t, syncRepo, "known-b", "ses_b")

	err := cmdRekey([]string{"known-zz", "fresh-key"})
	if err == nil || !strings.Contains(err.Error(), "nothing to relocate") {
		t.Fatalf("err = %v, want missing-source error", err)
	}
	for _, want := range []string{"known-a", "known-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must list known key %s:\n%v", want, err)
		}
	}
}

// Renaming a key dir wholesale when ANOTHER project key physically lives
// inside it would silently relocate that other project too — refuse instead.
func TestCmdRekeyRefusesNestedForeignKeysInsideOld(t *testing.T) {
	syncRepo := rekeyEnv(t)
	seedRevisionProject(t, syncRepo, "parent-key", "ses_p")
	seedRevisionProject(t, syncRepo, "parent-key/nested-child", "ses_c")

	err := cmdRekey([]string{"parent-key", "renamed-parent"})
	if err == nil || !strings.Contains(err.Error(), "contains other project keys") {
		t.Fatalf("err = %v, want nested-key refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(syncRepo, "opencode", "parent-key", "nested-child", "sessions")); statErr != nil {
		t.Errorf("refusal must leave nested child intact (%v)", statErr)
	}
}

// Identical keys or escape-shaped keys never reach the filesystem.
func TestValidateKeyShapeAndIdentityGuards(t *testing.T) {
	for _, bad := range []string{"", "/abs", "../escape", "ok/../evil", "a//b", ".", "a/./b"} {
		if err := validateKeyShape(bad); err == nil {
			t.Errorf("validateKeyShape(%q) must fail", bad)
		}
	}
	for _, okKey := range []string{"plain", "team/server/app", "_unmapped/home/user/p"} {
		if err := validateKeyShape(okKey); err != nil {
			t.Errorf("validateKeyShape(%q) = %v, want ok", okKey, err)
		}
	}
	if !hasDirPrefix("a/b/c", "a/b") || hasDirPrefix("a/b/c", "a/b/c") || hasDirPrefix("a/bc", "a/b") {
		t.Error("hasDirPrefix segment semantics wrong")
	}
}
