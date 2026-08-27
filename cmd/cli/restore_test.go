package main

import (
	"path/filepath"
	"strings"
	"testing"

	"agentsync/internal/receivestate"
	"agentsync/internal/syncrepo"
)

// restoreFixtures builds a conflicted group over REAL walker refs, plus its
// sync repo handle and an open receive-state store. The fixture session has
// TWO device buckets; device A additionally carries a superseded mid-chain
// capture so head-vs-audit behavior is observable.
func restoreFixtures(t *testing.T) (gi conflictGroup, clone string, st *receivestate.Store, repo *syncrepo.Repo) {
	t.Helper()
	base, cl, key := conflictReceiveEnv(t)
	clone = cl
	syncRepo := filepath.Join(base, "sync-repo")

	writeRevisionFixture(t, syncRepo, key, "ses_conf", "", "", ts(t, "2026-08-19T00:00:00Z"), "1.18.18",
		[]byte(`{"info":{"id":"ses_conf","projectID":"prj_ses_conf","directory":"/d/ses_conf","version":"1.18.18"},"v":"old"}`))

	statePath, err := receivestate.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	st, err = receivestate.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}

	refs, ferr := findRevisions(syncRepo)
	if ferr != nil {
		t.Fatal(ferr)
	}
	for _, g := range buildConflictGroups(refs) {
		if g.Group.SessionID == "ses_conf" && g.Group.Conflicted {
			gi = g
			break
		}
	}
	return gi, clone, st, syncrepo.Open(syncRepo)
}

// Happy path: chosen head verifies per digest×clone; the OTHER bucket's head
// lands archive-only; superseded mid-chain digests gain no outcome records
// from sibling marking (their bucket's head covers them).
func TestRestoreRevisionVerifiesChosenAndArchivesSiblingHeads(t *testing.T) {
	gi, clone, st, repo := restoreFixtures(t)
	imports := []string{}
	ad := fakeWriteBackAdapter(t, &imports)

	chosen := gi.Group.Heads[0].Digest
	sibling := gi.Group.Heads[1].Digest

	restored, err := restoreRevision(repo, nil, st, ad, gi, chosen, []string{clone})
	if err != nil || !restored {
		t.Fatalf("restoreRevision restored=%v err=%v, want true/nil", restored, err)
	}
	o, ok, gerr := st.Get(chosen, clone)
	if gerr != nil || !ok || o.Status != receivestate.StatusVerified {
		t.Fatalf("chosen outcome = %#v ok=%v err=%v, want verified", o, ok, gerr)
	}
	sib, ok, gerr := st.Get(sibling, clone)
	if gerr != nil || !ok || sib.Status != receivestate.StatusArchiveOnly {
		t.Fatalf("sibling-head outcome = %#v ok=%v err=%v, want archive-only", sib, ok, gerr)
	}
	for _, r := range gi.Group.Revisions {
		isHead := false
		for _, h := range gi.Group.Heads {
			if h.Digest == r.Digest {
				isHead = true
			}
		}
		if isHead {
			continue
		}
		if _, exists, _ := st.Get(r.Digest, clone); exists {
			t.Errorf("superseded digest %s must not gain an outcome from sibling marking", r.Digest[:6])
		}
	}
}

// Busy targets never hard-fail the broadcast: the process guard marks the
// outcome busy for later retry and restored reports failure honestly.
func TestRestoreRevisionRecordsBusyTargetForRetry(t *testing.T) {
	gi, clone, st, repo := restoreFixtures(t)
	imports := []string{}
	ad := fakeWriteBackAdapter(t, &imports)
	ad.ProcessGuard = func(targetPath string) (bool, error) { return true, nil }

	chosen := gi.Group.Heads[0].Digest
	restored, err := restoreRevision(repo, nil, st, ad, gi, chosen, []string{clone})
	if err != nil {
		t.Fatalf("busy target must not be an error: %v", err)
	}
	if restored {
		t.Error("restored must be false when every target was busy")
	}
	o, ok, gerr := st.Get(chosen, clone)
	if gerr != nil || !ok || o.Status != receivestate.StatusBusy {
		t.Fatalf("outcome = %#v ok=%v err=%v, want busy (retried later)", o, ok, gerr)
	}
	if len(imports) != 0 {
		t.Errorf("busy run must not import, got %v", imports)
	}
}

// An artifact/version-pin block happens BEFORE any mutation: nothing is
// recorded, nothing imports, and the error says exactly that.
func TestRestoreRevisionBlocksBeforeAnyMutation(t *testing.T) {
	gi, clone, st, repo := restoreFixtures(t)
	imports := []string{}
	ad := fakeWriteBackAdapter(t, &imports)
	ad.ToolVersion = func() (string, error) { return "9.9.9-mismatch", nil }

	chosen := gi.Group.Heads[0].Digest
	_, err := restoreRevision(repo, nil, st, ad, gi, chosen, []string{clone})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v, want pre-write-back artifact block", err)
	}
	outcomes, lerr := st.List()
	if lerr != nil || len(outcomes) != 0 {
		t.Errorf("blocked run must record NO outcomes, got %#v (%v)", outcomes, lerr)
	}
	if len(imports) != 0 {
		t.Errorf("blocked run must not import, got %v", imports)
	}
}

// Targets that stop resolving to the canonical key are skipped; with none
// left valid the run refuses instead of writing into a wrong project.
func TestRestoreRevisionRefusesWhenNoValidTargetsRemain(t *testing.T) {
	gi, _, st, repo := restoreFixtures(t)
	imports := []string{}
	ad := fakeWriteBackAdapter(t, &imports)

	chosen := gi.Group.Heads[0].Digest
	_, err := restoreRevision(repo, nil, st, ad, gi, chosen, []string{"/definitely/not/a/clone"})
	if err == nil || !strings.Contains(err.Error(), "no valid local clone") {
		t.Fatalf("err = %v, want no-valid-target refusal", err)
	}
	outcomes, lerr := st.List()
	if lerr != nil || len(outcomes) != 0 {
		t.Errorf("refused run must record NO outcomes, got %#v (%v)", outcomes, lerr)
	}
}
