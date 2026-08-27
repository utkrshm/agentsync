package conflict

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func rev(key, sid, digest string) Revision {
	return Revision{Key: key, SessionID: sid, Digest: digest}
}

func devRev(key, sid, digest, device string, at time.Time) Revision {
	return Revision{Key: key, SessionID: sid, Digest: digest, Device: device, CapturedAt: at}
}

func ts(s string) time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return v
}

func groupSig(g Group) string {
	sig := fmt.Sprintf("%s/%s conflicted=%v superseded=%d revisions=[", g.Key, g.SessionID, g.Conflicted, g.Superseded)
	for i, r := range g.Revisions {
		if i > 0 {
			sig += " "
		}
		sig += r.Digest
	}
	sig += "] heads=["
	for i, h := range g.Heads {
		if i > 0 {
			sig += " "
		}
		sig += fmt.Sprintf("%s@%s", h.Digest, h.Device)
	}
	return sig + "]"
}

func sigs(groups []Group) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, groupSig(g))
	}
	return out
}

// THE live repro (machine-a, 2026-08-27): one device capturing the same
// session twice is linear progress — its newest capture is the head and the
// group stays clean with the older entry merely superseded.
func TestDetectCollapsesSameDeviceChain(t *testing.T) {
	got := Detect([]Revision{
		devRev("k1", "ses_x", "77b2e0", "dev-a", ts("2026-08-27T09:00:00Z")),
		devRev("k1", "ses_x", "a3f9c1", "dev-a", ts("2026-08-27T09:05:00Z")),
	})
	want := []string{"k1/ses_x conflicted=false superseded=1 revisions=[77b2e0 a3f9c1] heads=[a3f9c1@dev-a]"}
	if !reflect.DeepEqual(sigs(got), want) {
		t.Fatalf("got %v, want %v", sigs(got), want)
	}
}

// Three devices each contributing a two-capture chain: every mid-chain
// capture is ignored, the three newest disagree on content, and the verdict
// is a conflict over exactly those three heads.
func TestDetectThreeDevicesConflictOverHeads(t *testing.T) {
	got := Detect([]Revision{
		devRev("k", "s", "a-old", "dev-1", ts("2026-08-26T10:00:00Z")),
		devRev("k", "s", "a-new", "dev-1", ts("2026-08-27T10:00:00Z")),
		devRev("k", "s", "b-old", "dev-2", ts("2026-08-26T11:00:00Z")),
		devRev("k", "s", "b-new", "dev-2", ts("2026-08-27T11:00:00Z")),
		devRev("k", "s", "c-old", "dev-3", ts("2026-08-26T12:00:00Z")),
		devRev("k", "s", "c-new", "dev-3", ts("2026-08-27T12:00:00Z")),
	})
	want := []string{
		"k/s conflicted=true superseded=3 revisions=[a-new a-old b-new b-old c-new c-old] " +
			"heads=[a-new@dev-1 b-new@dev-2 c-new@dev-3]",
	}
	if !reflect.DeepEqual(sigs(got), want) {
		t.Fatalf("got %v, want %v", sigs(got), want)
	}
}

// Equal timestamps within one bucket have no newer/older evidence; the tie
// must resolve deterministically to the lexicographically greatest digest no
// matter how the input arrives.
func TestDetectTieTimestampsPicksGreaterDigest(t *testing.T) {
	same := ts("2026-08-22T00:00:00Z")
	inputs := [][]Revision{
		{devRev("k", "s", "aaaa", "dev-1", same), devRev("k", "s", "zzzz", "dev-1", same)},
		{devRev("k", "s", "zzzz", "dev-1", same), devRev("k", "s", "aaaa", "dev-1", same)},
	}
	want := []string{"k/s conflicted=false superseded=1 revisions=[aaaa zzzz] heads=[zzzz@dev-1]"}
	for i, in := range inputs {
		if got := sigs(Detect(in)); !reflect.DeepEqual(got, want) {
			t.Fatalf("input order %d: got %v, want %v", i, got, want)
		}
	}
}

// A zero-time capture carries no ordering evidence inside its bucket and must
// always lose to any timestamped sibling, even one with a smaller digest.
func TestDetectZeroTimeLosesToTimestamped(t *testing.T) {
	got := Detect([]Revision{
		devRev("k", "s", "0000", "dev-1", ts("2026-01-01T00:00:00Z")),
		devRev("k", "s", "ffff", "dev-1", time.Time{}),
		devRev("k", "s", "eeee", "dev-1", time.Time{}),
	})
	want := []string{"k/s conflicted=false superseded=2 revisions=[0000 eeee ffff] heads=[0000@dev-1]"}
	if !reflect.DeepEqual(sigs(got), want) {
		t.Fatalf("got %v, want %v", sigs(got), want)
	}
}

// Migrated/orphan rows ("" device) all share ONE unknown bucket whose newest
// entry is that chain's single head. Unattributed content therefore never
// collides against itself, while it still conflicts against a named device's
// head — we surface rather than guess (invariant #4).
func TestDetectUnknownBucketCollapsesAndConflictsAgainstNamedDevice(t *testing.T) {
	got := Detect([]Revision{
		devRev("k", "s", "m-old", "", ts("2026-08-20T00:00:00Z")),
		devRev("k", "s", "m-new", "", ts("2026-08-21T00:00:00Z")),
		devRev("k", "s", "h-new", "host-a", ts("2026-08-21T05:00:00Z")),
	})
	want := []string{"k/s conflicted=true superseded=1 revisions=[h-new m-new m-old] heads=[h-new@host-a m-new@]"}
	if !reflect.DeepEqual(sigs(got), want) {
		t.Fatalf("got %v, want %v", sigs(got), want)
	}
}

// Distinct devices holding identical bytes are idempotent duplicates of the
// same content: they dedup to one entry (and thus one bucket) — clean.
func TestDetectIdenticalDigestAcrossDevicesStaysClean(t *testing.T) {
	got := Detect([]Revision{
		devRev("k", "s", "aa", "dev-1", ts("2026-08-20T00:00:00Z")),
		devRev("k", "s", "aa", "dev-2", ts("2026-08-21T00:00:00Z")),
	})
	want := []string{"k/s conflicted=false superseded=0 revisions=[aa] heads=[aa@dev-1]"}
	if !reflect.DeepEqual(sigs(got), want) {
		t.Fatalf("got %v, want %v", sigs(got), want)
	}
}

// Unattributed legacy rows all share the unknown bucket: multiple distinct
// digests without provenance collapse to their newest-bucket head instead of
// reading as divergence.
func TestDetectUnattributedDigestsCollapse(t *testing.T) {
	got := Detect([]Revision{
		rev("k1", "ses_x", "77b2e0"),
		rev("k1", "ses_x", "a3f9c1"),
	})
	want := []string{"k1/ses_x conflicted=false superseded=1 revisions=[77b2e0 a3f9c1] heads=[a3f9c1@]"}
	if !reflect.DeepEqual(sigs(got), want) {
		t.Fatalf("got %v, want %v", sigs(got), want)
	}
}

// Groups come out ordered by Key then SessionID regardless of input order,
// and the same digest in DIFFERENT sessions is not collapsed across groups.
func TestDetectOrdersGroupsDeterministically(t *testing.T) {
	forward := []Revision{
		devRev("k2", "s1", "dd", "dev-x", ts("2026-08-25T00:00:00Z")),
		devRev("k1", "s2", "dd", "dev-y", ts("2026-08-25T00:00:00Z")), // same digest as k2/s1 — separate identity
		devRev("k1", "s1", "cc", "dev-z", ts("2026-08-24T00:00:00Z")),
		devRev("k1", "s1", "aa", "dev-w", ts("2026-08-23T00:00:00Z")),
		devRev("k1", "s2", "ee", "dev-v", ts("2026-08-22T00:00:00Z")),
	}
	reversed := append([]Revision(nil), forward...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	expected := []string{
		"k1/s1 conflicted=true superseded=0 revisions=[aa cc] heads=[aa@dev-w cc@dev-z]",
		"k1/s2 conflicted=true superseded=0 revisions=[dd ee] heads=[dd@dev-y ee@dev-v]",
		"k2/s1 conflicted=false superseded=0 revisions=[dd] heads=[dd@dev-x]",
	}
	cases := map[string][]Revision{"forward": forward, "reversed": reversed}
	for name, cas := range cases {
		if got := sigs(Detect(cas)); !reflect.DeepEqual(got, expected) {
			t.Fatalf("%s: got %v, want %v", name, got, expected)
		}
	}
}

func TestDetectEmptyInput(t *testing.T) {
	got := Detect(nil)
	if len(got) != 0 {
		t.Fatalf("empty input must yield no groups, got %v", got)
	}
	got = Detect([]Revision{})
	if len(got) != 0 {
		t.Fatalf("empty slice must yield no groups, got %v", got)
	}
}

// Detection must not mutate the caller's slice (receive reuses refs after
// grouping).
func TestDetectDoesNotMutateInput(t *testing.T) {
	in := []Revision{rev("k", "s", "bb"), rev("k", "s", "aa")}
	cp := append([]Revision(nil), in...)
	Detect(in)
	if !reflect.DeepEqual(in, cp) {
		t.Fatalf("input mutated: %v", in)
	}
}
