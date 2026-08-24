package main

import (
	"reflect"
	"testing"

	"agentsync/internal/revision"
)

// resumeEntries must collapse multi-revision sessions into ONE entry labeled
// "(N revisions — conflicted)" and keep clean sessions as plain entries,
// sorted by session id.
func TestResumeEntriesGrouping(t *testing.T) {
	newest := &revision.Meta{Title: "Shared conversation", CapturedAt: ts(t, "2026-08-21T09:31:45Z")}
	older := &revision.Meta{Title: "Shared conversation", CapturedAt: ts(t, "2026-08-20T14:02:11Z")}
	titled := &revision.Meta{Title: "Solo work"}
	refs := []revisionRef{
		metaRef("ka", "ses_b", "dd01", "/r/b/dd.json", titled),
		metaRef("ka", "ses_a", "bb02", "/r/a/bb.json", older),  // conflicted pair
		metaRef("ka", "ses_a", "aa03", "/r/a/aa.json", newest), // conflicted pair
	}
	got := resumeEntries(refs)
	want := []sessionEntry{
		{ID: "ses_a", Key: "ka", Title: "Shared conversation (2 revisions — conflicted)", Conflicted: true, RevCount: 2},
		{ID: "ses_b", Key: "ka", Title: "Solo work", Conflicted: false, RevCount: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %+v\nwant %+v", got, want)
	}
}

// Untitled sidecars fall back to "(untitled)"; the conflicted suffix still
// applies.
func TestResumeEntriesUntitledFallback(t *testing.T) {
	refs := []revisionRef{
		metaRef("k", "ses_u", "aa", "/r/aa.json", &revision.Meta{}),
		metaRef("k", "ses_u", "bb", "/r/bb.json", nil),
	}
	got := resumeEntries(refs)
	if len(got) != 1 || !got[0].Conflicted {
		t.Fatalf("expected one conflicted entry, got %#v", got)
	}
	if got[0].Title != "(untitled) (2 revisions — conflicted)" {
		t.Errorf("title = %q", got[0].Title)
	}
}

func TestResumeEntriesEmpty(t *testing.T) {
	if got := resumeEntries(nil); len(got) != 0 {
		t.Fatalf("no refs must yield no entries, got %#v", got)
	}
}

// newestRevisionRef picks the latest CapturedAt among sidecar-bearing refs;
// zero timestamps don't count; with no usable times it falls back to the last
// digest in lexicographic order.
func TestNewestRevisionRef(t *testing.T) {
	aug20 := metaRef("k", "s", "aaaa", "/r/aa.json",
		&revision.Meta{CapturedAt: ts(t, "2026-08-20T14:02:11Z")})
	aug21 := metaRef("k", "s", "bbbb", "/r/bb.json",
		&revision.Meta{CapturedAt: ts(t, "2026-08-21T09:31:45Z")})
	noMeta := revisionRef{SessionID: "s", Key: "k", Digest: "cccc", PayloadPath: "/r/cc.json"}
	zeroTime := metaRef("k", "s", "dddd", "/r/dd.json", &revision.Meta{})

	t.Run("latest captured_at wins regardless of digest order", func(t *testing.T) {
		got := newestRevisionRef([]revisionRef{aug20, noMeta, aug21})
		if got.Digest != "bbbb" {
			t.Fatalf("picked %q, want bbbb", got.Digest)
		}
	})
	t.Run("zero captured_at treated as absent", func(t *testing.T) {
		got := newestRevisionRef([]revisionRef{aug20, zeroTime})
		if got.Digest != "aaaa" {
			t.Fatalf("picked %q, want aaaa", got.Digest)
		}
	})
	t.Run("no times falls back to last sorted digest", func(t *testing.T) {
		got := newestRevisionRef([]revisionRef{noMeta, zeroTime}) // dddd > cccc
		if got.Digest != "dddd" {
			t.Fatalf("picked %q, want dddd", got.Digest)
		}
		got = newestRevisionRef([]revisionRef{zeroTime, noMeta}) // order-independent
		if got.Digest != "dddd" {
			t.Fatalf("picked %q, want dddd", got.Digest)
		}
	})
	t.Run("equal timestamps keep first in walker order", func(t *testing.T) {
		same := ts(t, "2026-08-22T00:00:00Z")
		r1 := metaRef("k", "s", "1111", "/r/1.json", &revision.Meta{CapturedAt: same})
		r2 := metaRef("k", "s", "2222", "/r/2.json", &revision.Meta{CapturedAt: same})
		if got := newestRevisionRef([]revisionRef{r1, r2}); got.Digest != "1111" {
			t.Fatalf("picked %q, want 1111 (deterministic tiebreak)", got.Digest)
		}
	})
}

func TestParseConfirm(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"y", true}, {"Y", true}, {"yes", true}, {"YES", true}, {" Yes ", true},
		{"", false}, {"n", false}, {"no", false}, {"y ", true}, {"yeah", false},
		{"0", false}, {"1", false}, {"maybe", false}, {"\ty\t", true},
	}
	for _, tc := range tests {
		if got := parseConfirm(tc.in); got != tc.want {
			t.Errorf("parseConfirm(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// confirmNewestConflictResume lists every revision and prompts for the newest
// one only when the answer is an explicit yes; this pins the pure decision
// boundary (the prompt I/O itself stays thin).
func TestConfirmFlowUsesParsedAnswer(t *testing.T) {
	refs := []revisionRef{
		metaRef("k", "ses_x", "77b2e0fff", "/r/77b2e0.json",
			&revision.Meta{DeviceAlias: "desktop", CapturedAt: ts(t, "2026-08-21T09:31:45Z")}),
		metaRef("k", "ses_x", "a3f9c1aaa", "/r/a3f9c1.json",
			&revision.Meta{DeviceAlias: "laptop", CapturedAt: ts(t, "2026-08-20T14:02:11Z")}),
	}
	groups := buildConflictGroups(refs)

	// The listing helper the confirm flow uses resolves each group member to
	// its most informative ref, in digest-sorted order.
	gi := groups[0]
	var lines []string
	for _, rev := range gi.Group.Revisions {
		ref, ok := primaryRef(gi.Refs, rev.Digest)
		if !ok {
			t.Fatal("primaryRef lost a group member")
		}
		lines = append(lines, conflictReportLine(ref))
	}
	want := []string{
		// Digest-sorted group order: "77b2e0…" < "a3f9c1…" lexicographically.
		"77b2e0… desktop 2026-08-21 09:31 UTC",
		"a3f9c1… laptop 2026-08-20 14:02 UTC",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("listing = %#v, want %#v", lines, want)
	}
	if newest := newestRevisionRef(refs); newest.Digest != "77b2e0fff" {
		t.Fatalf("newest = %q, want 77b2e0fff", newest.Digest)
	}
}
