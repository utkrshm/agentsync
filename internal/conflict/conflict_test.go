package conflict

import (
	"fmt"
	"reflect"
	"testing"
)

func rev(key, sid, digest string) Revision {
	return Revision{Key: key, SessionID: sid, Digest: digest}
}

func groupSig(g Group) string {
	sig := fmt.Sprintf("%s/%s conflicted=%v revisions=[", g.Key, g.SessionID, g.Conflicted)
	for i, r := range g.Revisions {
		if i > 0 {
			sig += " "
		}
		sig += r.Digest
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

// Two distinct digests under one session identity must be flagged as a
// conflict and both revisions retained (plan §3 "mark multiple different
// digests as a logical session conflict").
func TestDetectFlagsConflictedSession(t *testing.T) {
	got := Detect([]Revision{
		rev("k1", "ses_x", "77b2e0"),
		rev("k1", "ses_x", "a3f9c1"),
	})
	want := []string{"k1/ses_x conflicted=true revisions=[77b2e0 a3f9c1]"}
	if !reflect.DeepEqual(sigs(got), want) {
		t.Fatalf("got %v, want %v", sigs(got), want)
	}
}

// Identical digests are idempotent duplicates: they collapse onto one entry
// and must NOT be reported as a conflict.
func TestDetectCollapsesIdenticalDigests(t *testing.T) {
	got := Detect([]Revision{
		rev("k1", "ses_x", "aaaa"),
		rev("k1", "ses_x", "aaaa"),
	})
	want := []string{"k1/ses_x conflicted=false revisions=[aaaa]"}
	if !reflect.DeepEqual(sigs(got), want) {
		t.Fatalf("got %v, want %v", sigs(got), want)
	}
}

// A duplicate digest mixed with a distinct one leaves exactly two entries.
func TestDetectDedupesAmongDistinctDigests(t *testing.T) {
	got := Detect([]Revision{
		rev("k", "s", "bb"),
		rev("k", "s", "aa"),
		rev("k", "s", "bb"),
		rev("k", "s", "cc"),
		rev("k", "s", "aa"),
	})
	want := []string{"k/s conflicted=true revisions=[aa bb cc]"}
	if !reflect.DeepEqual(sigs(got), want) {
		t.Fatalf("got %v, want %v", sigs(got), want)
	}
}

// Groups come out ordered by Key then SessionID regardless of input order,
// and the same digest in DIFFERENT sessions is not collapsed across groups.
func TestDetectOrdersGroupsDeterministically(t *testing.T) {
	got := Detect([]Revision{
		rev("k2", "s1", "dd"),
		rev("k1", "s2", "dd"), // same digest as k2/s1 — separate identity
		rev("k1", "s1", "cc"),
		rev("k1", "s1", "aa"),
		rev("k1", "s2", "ee"),
	})
	expected := []string{
		// k1/s2 holds two distinct digests; that one of them also appears
		// under k2/s1 does not merge the identities.
		"k1/s1 conflicted=true revisions=[aa cc]",
		"k1/s2 conflicted=true revisions=[dd ee]",
		"k2/s1 conflicted=false revisions=[dd]",
	}
	if !reflect.DeepEqual(sigs(got), expected) {
		t.Fatalf("got %v, want %v", sigs(got), expected)
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
