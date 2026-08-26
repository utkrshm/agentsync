package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentsync/internal/revision"
)

func mustDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeFileFixture creates parent dirs then writes content to path.
func writeFileFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// exportFixture returns a minimal valid export document for id.
func exportFixture(id string) []byte {
	data, err := json.Marshal(map[string]any{
		"info": map[string]any{
			"id":        id,
			"projectID": "prj_" + id,
			"directory": "/d/" + id,
			"version":   "1.18.18",
			"title":     "title-" + id,
		},
		"messages": []any{},
	})
	if err != nil {
		panic(err)
	}
	return data
}

func filterBy(refs []revisionRef, pred func(revisionRef) bool) []revisionRef {
	var out []revisionRef
	for _, r := range refs {
		if pred(r) {
			out = append(out, r)
		}
	}
	return out
}

// buildMixedLayoutTree plants legacy flat-layout artifacts (with and without
// import-meta) plus revisions-layout payloads in one repo fixture.
func buildMixedLayoutTree(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()

	// syncrepo.Exists gates on an initialized repo (.git present); CLI-level
	// tests route requireConfig at this fixture and must not trip that gate
	// (receive's conflictReceiveEnv plants .git for the same reason).
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}

	// Legacy: export + import-meta sidecar.
	writeFileFixture(t,
		filepath.Join(repo, "opencode", "kb", "export", "ses_b.json"),
		string(exportFixture("ses_b")))
	writeFileFixture(t,
		filepath.Join(repo, "opencode", "kb", "import-meta", "ses_b.json"),
		`{"id":"ses_b","projectID":"prj_ses_b","directory":"/d/ses_b","title":"legacy-title-b"}`)

	// Legacy: export without any sidecar (older install).
	writeFileFixture(t,
		filepath.Join(repo, "opencode", "kb", "export", "ses_nometa.json"),
		string(exportFixture("ses_nometa")))

	// Revisions layout: two revisions of ses_a under key ka; only the first
	// carries a sidecar — with the full captured-provenance set (device,
	// capture time, producer version) exactly as send/Mirror write them.
	dataA1 := exportFixture("ses_a")
	d1 := mustDigest(dataA1)
	revDir := filepath.Join(repo, "opencode", "ka", "sessions", "ses_a", "revisions")
	writeFileFixture(t, filepath.Join(revDir, d1+".json"), string(dataA1))
	meta := revision.Meta{
		SchemaVersion:     revision.SchemaVersion,
		OriginalSessionID: "ses_a",
		Digest:            d1,
		SourceDeviceID:    "dev-a",
		CapturedAt:        time.Date(2026, 8, 19, 8, 15, 0, 0, time.UTC),
		ProducerVersion:   "1.18.18",
		Status:            revision.StatusCaptured,
		Title:             "title-ses_a",
	}
	blob, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	writeFileFixture(t, filepath.Join(revDir, d1+".meta.json"), string(blob))

	dataA2 := []byte(`{"info":{"id":"ses_a","projectID":"p","directory":"/d/ses_a","version":"1.18.18"},"v":2}`)
	writeFileFixture(t, filepath.Join(revDir, mustDigest(dataA2)+".json"), string(dataA2))

	// Junk that must never appear as a payload ref.
	writeFileFixture(t, filepath.Join(revDir, "notes.txt"), "ignore me")
	return repo
}

func TestFindRevisionsCoversBothLayouts(t *testing.T) {
	repo := buildMixedLayoutTree(t)

	refs, err := findRevisions(repo)
	if err != nil {
		t.Fatal(err)
	}

	var gotOrder []string
	for _, ref := range refs {
		gotOrder = append(gotOrder, ref.SessionID+"@"+ref.Key)
	}
	wantOrder := []string{
		"ses_a@ka", // two revisions of one session
		"ses_a@ka",
		"ses_b@kb",
		"ses_nometa@kb",
	}
	if strings.Join(gotOrder, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("walker order = %v, want %v", gotOrder, wantOrder)
	}

	// ses_b: legacy with import-meta -> synthetic migrated Meta.
	bRefs := filterBy(refs, func(r revisionRef) bool { return r.SessionID == "ses_b" })
	if len(bRefs) != 1 || !bRefs[0].Legacy {
		t.Fatalf("expected one legacy ref for ses_b, got %#v", bRefs)
	}
	payloadB, err := os.ReadFile(bRefs[0].PayloadPath)
	if err != nil {
		t.Fatal(err)
	}
	if bRefs[0].Digest != mustDigest(payloadB) {
		t.Errorf("legacy digest not computed from bytes: %s", bRefs[0].Digest)
	}
	m := bRefs[0].Meta
	if m == nil {
		t.Fatal("legacy ref with import-meta must carry a synthetic Meta")
	}
	if m.Status != revision.StatusMigrated || m.SchemaVersion != revision.SchemaVersion {
		t.Errorf("synthetic meta status/schema wrong: %#v", m)
	}
	if m.Title != "legacy-title-b" || m.ProjectID != "prj_ses_b" ||
		m.Directory != "/d/ses_b" || m.Digest != bRefs[0].Digest ||
		m.OriginalSessionID != "ses_b" {
		t.Errorf("synthetic meta mapping wrong: %#v", m)
	}

	// ses_nometa: no sidecar knowledge -> Meta nil, ref still usable.
	nmRefs := filterBy(refs, func(r revisionRef) bool { return r.SessionID == "ses_nometa" })
	if len(nmRefs) != 1 {
		t.Fatalf("expected exactly one ses_nometa ref, got %d", len(nmRefs))
	}
	if nmRefs[0].Meta != nil {
		t.Errorf("legacy ref without import-meta must have nil Meta, got %#v", nmRefs[0].Meta)
	}

	// ses_a: revisions layout, two digests, sidecar on rev one only.
	aRefs := filterBy(refs, func(r revisionRef) bool { return r.SessionID == "ses_a" })
	if len(aRefs) != 2 {
		t.Fatalf("expected both ses_a revisions, got %d", len(aRefs))
	}
	withMeta := 0
	for _, ref := range aRefs {
		if ref.Legacy {
			t.Errorf("revisions-layout ref flagged legacy: %#v", ref)
		}
		if !strings.Contains(filepath.ToSlash(ref.PayloadPath), "sessions/ses_a/revisions/") &&
			!strings.Contains(ref.PayloadPath, filepath.Join("sessions", "ses_a", "revisions")) {
			t.Errorf("payload path outside revisions dir: %q", ref.PayloadPath)
		}
		if ref.Meta != nil {
			withMeta++
			if ref.Meta.Digest != ref.Digest {
				t.Errorf("sidecar digest mismatch: meta=%s file=%s", ref.Meta.Digest, ref.Digest)
			}
			if ref.Meta.Status != revision.StatusCaptured {
				t.Errorf("sidecar status = %q, want captured", ref.Meta.Status)
			}
		}
	}
	if withMeta != 1 {
		t.Errorf("exactly one ses_a revision should have loaded its sidecar, got %d", withMeta)
	}
}

// TestFindRevisionsMissingTreeYieldsNothing covers fresh clones.
func TestFindRevisionsMissingTreeYieldsNothing(t *testing.T) {
	refs, err := findRevisions(t.TempDir())
	if err != nil {
		t.Fatalf("missing opencode/ tree must not error: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected no refs, got %#v", refs)
	}
}

// TestSortAndDedupeCollapsesExactDuplicates pins deterministic ordering and
// duplicate collapse independent of the filesystem walk.
func TestSortAndDedupeCollapsesExactDuplicates(t *testing.T) {
	sharedMeta := &revision.Meta{Digest: "aa"}
	withMeta := revisionRef{SessionID: "s1", Key: "k", Digest: "aa", Meta: sharedMeta}
	noMeta := revisionRef{SessionID: "s1", Key: "k", Digest: "aa"}
	other := revisionRef{SessionID: "s1", Key: "k", Digest: "bb"}
	in := []revisionRef{other, withMeta, noMeta, other, withMeta}
	got := sortAndDedupe(in)
	want := []revisionRef{
		{SessionID: "s1", Key: "k", Digest: "aa"},
		{SessionID: "s1", Key: "k", Digest: "aa", Meta: sharedMeta},
		{SessionID: "s1", Key: "k", Digest: "bb"},
	}
	if len(got) != len(want) {
		t.Fatalf("dedupe produced %d refs, want %d: %#v", len(got), len(want), got)
	}
	// Order between the two "aa" variants is unspecified (equal sort keys);
	// assert multiset membership instead of positions.
	sawMeta, sawNoMeta, sawBB := false, false, false
	for _, r := range got {
		switch {
		case r.Digest == "aa" && r.Meta != nil:
			sawMeta = true
			if r.Meta != sharedMeta {
				t.Errorf("meta pointer not preserved: %#v", r.Meta)
			}
		case r.Digest == "aa" && r.Meta == nil:
			sawNoMeta = true
		case r.Digest == "bb":
			sawBB = true
		}
	}
	if !sawMeta || !sawNoMeta || !sawBB {
		t.Errorf("dedupe lost a variant: meta=%v nometa=%v bb=%v (%#v)", sawMeta, sawNoMeta, sawBB, got)
	}
}

// Regression: canonical keys may contain slashes (_unmapped/<path>); a
// single-level key scan made such sessions invisible to every consumer.
func TestFindRevisionsNestedSlashKeys(t *testing.T) {
	base := t.TempDir()
	syncRepo := filepath.Join(base, "repo")
	writeRevisionFixture(t, syncRepo, "_unmapped/home/dev/hi/bye", "ses_nested", "", "(untitled)",
		time.Date(2026, 8, 21, 21, 8, 56, 0, time.UTC), "1.18.18", exportFixture("ses_nested"))
	// A plain single-segment key must keep working alongside it.
	writeRevisionFixture(t, syncRepo, "plain-key", "ses_plain", "", "(untitled)",
		time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC), "1.18.18", exportFixture("ses_plain"))

	refs, err := findRevisions(syncRepo)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, r := range refs {
		keys[r.Key+"/"+r.SessionID] = true
	}
	if !keys["_unmapped/home/dev/hi/bye/ses_nested"] {
		t.Errorf("nested-slash key missing from walker: %#v", keys)
	}
	if !keys["plain-key/ses_plain"] {
		t.Errorf("plain key lost by depth-agnostic walk: %#v", keys)
	}
}
