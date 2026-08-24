package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/config"
	"agentsync/internal/conflict"
	"agentsync/internal/receivestate"
)

// feedStdin replaces os.Stdin with a pipe delivering input, restored on
// cleanup — the picker reads through bufio.Scanner(os.Stdin) like resume's.
func feedStdin(t *testing.T, input string) {
	t.Helper()
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
}

// sesConfDigests returns the two distinct digests of ses_conf as planted by
// conflictReceiveEnv, sorted lexicographically (the group's revision order).
func sesConfDigests() (first, second string) {
	d1 := mustDigest(exportFixture("ses_conf"))
	d2 := mustDigest([]byte(`{"info":{"id":"ses_conf","projectID":"prj_ses_conf","directory":"/d/ses_conf","version":"1.18.18"},"v":2}`))
	first, second = d1, d2
	if first > second {
		first, second = second, first
	}
	return first, second
}

// fakeWriteBackAdapter records imports and satisfies every injectable shell-out
// point hermetically (no real opencode binary involved).
func fakeWriteBackAdapter(t *testing.T, imports *[]string) *opencode.Adapter {
	t.Helper()
	base := t.TempDir()
	return &opencode.Adapter{
		ImportInto: func(exportPath, targetDir string) error {
			if imports != nil {
				*imports = append(*imports, targetDir)
			}
			return nil
		},
		PatchImport:  func(exportPath, targetDir, projectKey string) error { return nil },
		ProcessGuard: func(targetPath string) (bool, error) { return false, nil },
		ToolVersion:  func() (string, error) { return "1.18.18", nil },
		BinaryPath:   func() (string, error) { return filepath.Join(base, "fake-opencode"), nil },
		Fingerprint:  func() (string, error) { return strings.Repeat("ab", 32), nil },
		ProducerStateFile: func() (string, error) {
			return filepath.Join(base, "producer.json"), nil
		},
		VerifyImport: func(exportPath, targetDir string) error { return nil },
	}
}

func useFakeAdapter(t *testing.T, ad *opencode.Adapter) {
	t.Helper()
	old := buildWriteBackAdapter
	buildWriteBackAdapter = func(cfg config.Config) *opencode.Adapter { return ad }
	t.Cleanup(func() { buildWriteBackAdapter = old })
}

func outcomeByDigest(t *testing.T) map[string]receivestate.Outcome {
	t.Helper()
	out := map[string]receivestate.Outcome{}
	for _, o := range loadOutcomes(t) {
		out[o.ArtifactDigest] = o
	}
	return out
}

func TestPickRevision(t *testing.T) {
	digests := []string{
		"a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",
		"a1b2c3ffffffff0718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9",
		"9e8d7c6b5a493827150403020100ffeeddccbbaa99887766554433221100ff12",
	}
	tests := []struct {
		name       string
		prefix     string
		wantMatch  string   // "" when an error is expected
		errPart    string   // substring expected in the error message
		wantListed []string // digests the error must list; nil = all group digests
	}{
		{name: "unique short prefix", prefix: "9e8d", wantMatch: digests[2]},
		{name: "exact full digest", prefix: digests[0], wantMatch: digests[0]},
		{name: "uppercase prefix normalized", prefix: "9E8D", wantMatch: digests[2]},
		{
			// Only the two revisions actually matching the prefix are
			// candidates; the third (9e8d…) is not part of this ambiguity.
			name:       "ambiguous shared prefix lists candidates",
			prefix:     "a1b2c3",
			errPart:    "ambiguous",
			wantListed: []string{digests[0], digests[1]},
		},
		{name: "matching none lists all group digests", prefix: "ffff", errPart: "matches none"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickRevision(tc.prefix, digests)
			if tc.errPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errPart) {
					t.Fatalf("pickRevision(%q) err = %v, want containing %q", tc.prefix, err, tc.errPart)
				}
				listed := tc.wantListed
				if listed == nil {
					listed = digests
				}
				for _, d := range listed {
					if !strings.Contains(err.Error(), d) {
						t.Errorf("error must list digest %s:\n%s", d, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantMatch {
				t.Fatalf("pickRevision(%q) = %s, want %s", tc.prefix, got, tc.wantMatch)
			}
		})
	}
}

func TestArchiveSiblingsExcludesChosenKeepsOrder(t *testing.T) {
	groupRevs := []conflict.Revision{
		{Key: "k", SessionID: "s", Digest: "bbb"},
		{Key: "k", SessionID: "s", Digest: "aaa"},
		{Key: "k", SessionID: "s", Digest: "ccc"},
	}
	if got, want := archiveSiblings("aaa", groupRevs), []string{"bbb", "ccc"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("archiveSiblings = %v, want %v", got, want)
	}
	if got := archiveSiblings("solo", []conflict.Revision{{Key: "k", SessionID: "s", Digest: "x"}}); !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("every non-chosen revision is a sibling: %v", got)
	}
}

func TestBothFlagFailsClosedBeforeAnythingElse(t *testing.T) {
	err := cmdRecover([]string{"ses_x", "--both"})
	if err == nil {
		t.Fatal("--both must NEVER proceed")
	}
	msg := err.Error()
	for _, want := range []string{"--both is reserved", "stage-5 duplication mechanism", "ONE revision", "different devices/clones"} {
		if !strings.Contains(msg, want) {
			t.Errorf("--both refusal missing %q: %s", want, msg)
		}
	}
	// Fails closed even combined with --dry-run.
	if err := cmdRecover([]string{"ses_x", "--both", "--dry-run"}); err == nil {
		t.Error("--both + --dry-run must still refuse")
	}
}

func TestSingleRevisionNotice(t *testing.T) {
	if got, want := singleRevisionNotice("ses_x"), "ses_x has a single revision — nothing to recover"; got != want {
		t.Fatalf("singleRevisionNotice = %q, want %q", got, want)
	}
}

func TestDigest12(t *testing.T) {
	long := strings.Repeat("ab", 32)
	if got := digest12(long); got != long[:12]+"…" {
		t.Errorf("digest12(long) = %q", got)
	}
	if got := digest12("abc"); got != "abc" {
		t.Errorf("digest12(short) = %q", got)
	}
}

func TestCmdRecoverUnknownSessionActionable(t *testing.T) {
	isolateDeviceEnv(t)
	syncRepo := filepath.Join(os.Getenv("HOME"), "sync-repo")
	if err := os.MkdirAll(filepath.Join(syncRepo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestConfig(t, syncRepo)

	err := cmdRecover([]string{"ses_missing"})
	if err == nil || !strings.Contains(err.Error(), `no stored revisions found for session "ses_missing"`) {
		t.Fatalf("err = %v, want actionable unknown-session error", err)
	}
	if !strings.Contains(err.Error(), "agent-sync revisions list") {
		t.Errorf("error must point at inspection command: %v", err)
	}
}

func TestCmdRecoverMultipleKeysRefused(t *testing.T) {
	isolateDeviceEnv(t)
	syncRepo := filepath.Join(os.Getenv("HOME"), "sync-repo")
	if err := os.MkdirAll(filepath.Join(syncRepo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := exportFixture("ses_twice")
	now := time.Now()
	writeRevisionFixture(t, syncRepo, "key-one", "ses_twice", "", "", now, "1.18.18", payload)
	writeRevisionFixture(t, syncRepo, "key-two", "ses_twice", "", "", now, "1.18.18", payload)
	writeTestConfig(t, syncRepo)

	err := cmdRecover([]string{"ses_twice"})
	if err == nil || !strings.Contains(err.Error(), "multiple project keys") {
		t.Fatalf("err = %v, want multi-key refusal", err)
	}
	if !strings.Contains(err.Error(), "key-one") || !strings.Contains(err.Error(), "key-two") {
		t.Errorf("refusal must list both keys: %v", err)
	}
}

func TestCmdRecoverSingleRevisionSaysNothingToRecover(t *testing.T) {
	conflictReceiveEnv(t) // ses_solo is a single-revision session under no-clone-key

	var out string
	func() {
		var err error
		out = captureStdout(t, func() { err = cmdRecover([]string{"ses_solo"}) })
		if err != nil {
			t.Errorf("recover single-revision session: %v", err)
		}
	}()
	if !strings.Contains(out, "ses_solo has a single revision — nothing to recover") {
		t.Errorf("expected nothing-to-recover notice:\n%s", out)
	}
}

func TestCmdRecoverDryRunPlansAndMutatesNothing(t *testing.T) {
	_, clone, _ := conflictReceiveEnv(t)
	imports := []string{}
	useFakeAdapter(t, fakeWriteBackAdapter(t, &imports))
	feedStdin(t, "2\n")

	var out string
	func() {
		var err error
		out = captureStdout(t, func() { err = cmdRecover([]string{"ses_conf", "--dry-run"}) })
		if err != nil {
			t.Errorf("dry-run recover: %v", err)
		}
	}()

	if !strings.Contains(out, "DRY RUN: would recover") {
		t.Errorf("dry-run plan missing:\n%s", out)
	}
	if !strings.Contains(out, clone) {
		t.Errorf("plan should name the validated clone:\n%s", out)
	}
	if !strings.Contains(out, "would mark 1 sibling revision(s) archive-only") {
		t.Errorf("plan should mention siblings:\n%s", out)
	}
	if len(imports) != 0 {
		t.Errorf("dry-run must not import anything, got %v", imports)
	}
	statePath, err := receivestate.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Errorf("dry-run must not create receive state (stat err: %v)", statErr)
	}
}

func TestCmdRecoverBlankInputAbortsCleanly(t *testing.T) {
	conflictReceiveEnv(t)
	imports := []string{}
	useFakeAdapter(t, fakeWriteBackAdapter(t, &imports))
	feedStdin(t, "\n")

	var out string
	func() {
		var err error
		out = captureStdout(t, func() { err = cmdRecover([]string{"ses_conf"}) })
		if err != nil {
			t.Errorf("aborting recover: %v", err)
		}
	}()
	if !strings.Contains(out, "Aborted.") {
		t.Errorf("blank input must abort cleanly:\n%s", out)
	}
	if len(imports) != 0 {
		t.Errorf("abort must not import: %v", imports)
	}
}

// The core recovery contract: picker choice goes through the guarded pipeline,
// lands verified per digest×clone, and EVERY sibling is explicitly re-marked
// archive-only per clone so preserved state survives the switch.
func TestCmdRecoverRestoresChosenAndArchivesSiblings(t *testing.T) {
	_, clone, _ := conflictReceiveEnv(t)
	imports := []string{}
	useFakeAdapter(t, fakeWriteBackAdapter(t, &imports))
	feedStdin(t, "1\n")

	var out string
	func() {
		var err error
		out = captureStdout(t, func() { err = cmdRecover([]string{"ses_conf"}) })
		if err != nil {
			t.Errorf("recover: %v", err)
		}
	}()

	if len(imports) != 1 || imports[0] != clone {
		t.Fatalf("imports = %v, want exactly [%s]", imports, clone)
	}
	first, second := sesConfDigests()
	byDigest := outcomeByDigest(t)
	chosen, ok := byDigest[first]
	if !ok || chosen.Status != receivestate.StatusVerified || chosen.SessionID != "ses_conf" {
		t.Fatalf("chosen revision outcome = %#v (found=%v), want verified for %s", chosen, ok, first)
	}
	sibling, ok := byDigest[second]
	if !ok || sibling.Status != receivestate.StatusArchiveOnly || sibling.CandidatePath != clone {
		t.Fatalf("sibling outcome = %#v (found=%v), want archive-only at %s", sibling, ok, clone)
	}
	for _, want := range []string{
		"Recovered " + digest12(first),
		"remain preserved/archive-only",
		"agent-sync recover ses_conf",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

// Re-running recover to SWITCH revisions moves the association: the new choice
// verifies while every other revision (including the previously recovered one)
// returns to archive-only.
func TestCmdRecoverSwitchMovesAssociation(t *testing.T) {
	_, clone, _ := conflictReceiveEnv(t)
	imports := []string{}
	useFakeAdapter(t, fakeWriteBackAdapter(t, &imports))
	feedStdin(t, "1\n")
	func() {
		var err error
		captureStdout(t, func() { err = cmdRecover([]string{"ses_conf"}) })
		if err != nil {
			t.Errorf("first recover: %v", err)
		}
	}()

	feedStdin(t, "2\n")
	var out string
	func() {
		var err error
		out = captureStdout(t, func() { err = cmdRecover([]string{"ses_conf"}) })
		if err != nil {
			t.Errorf("switching recover: %v", err)
		}
	}()

	if len(imports) != 2 || imports[1] != clone {
		t.Fatalf("imports across runs = %v, want two writes to %s", imports, clone)
	}
	first, second := sesConfDigests()
	byDigest := outcomeByDigest(t)
	if o := byDigest[second]; o.Status != receivestate.StatusVerified {
		t.Errorf("newly chosen revision = %q, want verified", o.Status)
	}
	if o := byDigest[first]; o.Status != receivestate.StatusArchiveOnly {
		t.Errorf("previously recovered revision = %q, want back to archive-only", o.Status)
	}
	if !strings.Contains(out, digest12(second)) {
		t.Errorf("switch summary should name the newly recovered digest:\n%s", out)
	}
}

// Recovering one revision into MULTIPLE clones reports the degraded one-to-one
// association honestly (AGENTS.md invariant #8): exactly one clone verifies,
// the other records degraded, and siblings stay archive-only in both clones.
func TestCmdRecoverDegradedWhenMultipleClonesImport(t *testing.T) {
	base, _, _ := conflictReceiveEnv(t)
	clonesDir := filepath.Join(base, "clones")
	plantFakeClone(t, clonesDir, "second-worktree")
	idx, err := openRepoIndex()
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Scan(context.Background(), []string{clonesDir}, nil); err != nil {
		t.Fatal(err)
	}
	imports := []string{}
	useFakeAdapter(t, fakeWriteBackAdapter(t, &imports))
	feedStdin(t, "1\n")

	var out string
	func() {
		var runErr error
		out = captureStdout(t, func() { runErr = cmdRecover([]string{"ses_conf"}) })
		if runErr != nil {
			t.Errorf("multi-clone recover: %v", runErr)
		}
	}()

	if !strings.Contains(out, "WARNING: recovered into 2 clones") ||
		!strings.Contains(out, "LAST-imported clone") {
		t.Errorf("degraded note missing:\n%s", out)
	}
	if len(imports) != 2 {
		t.Fatalf("imports = %v, want both clones written", imports)
	}
	first, second := sesConfDigests()
	verified, degraded, siblingArchive := 0, 0, 0
	for _, o := range loadOutcomes(t) {
		if o.ArtifactDigest == second {
			if o.Status == receivestate.StatusArchiveOnly {
				siblingArchive++
			}
			continue
		}
		if o.ArtifactDigest != first {
			continue
		}
		switch o.Status {
		case receivestate.StatusVerified:
			verified++
		case receivestate.StatusDegraded:
			degraded++
		}
	}
	if verified != 1 || degraded != 1 || siblingArchive != 2 {
		t.Errorf("want 1 verified + 1 degraded chosen-digest outcomes and sibling archive-only in BOTH clones; got verified=%d degraded=%d siblingArchive=%d",
			verified, degraded, siblingArchive)
	}
}
