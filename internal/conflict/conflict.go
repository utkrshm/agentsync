// Package conflict detects logical session conflicts: two or more distinct
// content revisions sharing one canonical-project + original-session identity
// (docs/session-conflict-handling-plan.md §3). Detection is pure grouping over
// already-discovered revisions — it never touches storage, and its output is
// deterministic so repeated receive runs produce identical reports.
package conflict

import "sort"

// Revision identifies one stored artifact revision by its canonical project
// key, original session id, and content digest.
type Revision struct {
	Key       string
	SessionID string
	Digest    string
}

// Group is every known revision of one (Key, SessionID) identity. Revisions
// are deduped by digest and sorted lexicographically. A group with more than
// one distinct digest is a logical conflict: nothing may be restored until a
// human explicitly picks a revision (the recover flow).
type Group struct {
	Key        string
	SessionID  string
	Revisions  []Revision // deduped by digest, sorted lexicographically
	Conflicted bool       // >1 distinct digest for this identity
}

// Detect groups revisions by (Key, SessionID). Groups are returned ordered by
// Key then SessionID; identical digests within a group collapse to one entry
// (idempotent duplicates of the same bytes), so a group is Conflicted exactly
// when it holds more than one distinct digest. The input slice is never
// mutated.
func Detect(revs []Revision) []Group {
	sorted := make([]Revision, len(revs))
	copy(sorted, revs)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		switch {
		case a.Key != b.Key:
			return a.Key < b.Key
		case a.SessionID != b.SessionID:
			return a.SessionID < b.SessionID
		default:
			return a.Digest < b.Digest
		}
	})

	out := make([]Group, 0)
	for i := 0; i < len(sorted); {
		j := i + 1
		for j < len(sorted) &&
			sorted[j].Key == sorted[i].Key &&
			sorted[j].SessionID == sorted[i].SessionID {
			j++
		}
		g := Group{Key: sorted[i].Key, SessionID: sorted[i].SessionID}
		for _, r := range sorted[i:j] {
			if n := len(g.Revisions); n > 0 && g.Revisions[n-1].Digest == r.Digest {
				continue // duplicate digest — same bytes seen again, not a conflict
			}
			g.Revisions = append(g.Revisions, r)
		}
		g.Conflicted = len(g.Revisions) > 1
		out = append(out, g)
		i = j
	}
	return out
}
