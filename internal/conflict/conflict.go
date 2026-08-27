// Package conflict detects logical session conflicts: two or more distinct
// content revisions sharing one canonical-project + original-session identity
// (docs/session-conflict-handling-plan.md §3). Detection is pure grouping over
// already-discovered revisions — it never touches storage, and its output is
// deterministic so repeated receive runs produce identical reports.
//
// Since the DetectV2 pipeline (docs/sync-rekey-collapse-plan.md Step 2),
// revisions are additionally attributed to their producing device before a
// verdict: within one (Key, SessionID) identity the revisions of each device
// form a chain, and only each chain's newest capture is its head. Linear
// single-device history therefore reads as progress, not divergence; a
// conflict means two DIFFERENT devices' heads disagree on content.
package conflict

import (
	"sort"
	"time"
)

// Revision identifies one stored artifact revision by its canonical project
// key, original session id, and content digest.
type Revision struct {
	Key        string
	SessionID  string
	Digest     string
	Device     string    // producing-device identity from the sidecar (SourceDeviceID); "" shares the single unknown bucket (migrated/orphan captures)
	CapturedAt time.Time // capture instant; ranks only WITHIN a device bucket — cross-device clock skew can never decide outcomes; zero sorts below any timestamped entry
}

// Group is every known revision of one (Key, SessionID) identity after
// per-device collapse. Revisions stays the full deduped audit view (sorted by
// digest), while Heads holds only each device chain's newest capture. A group
// with more than one head is a logical conflict: nothing may be restored until
// a human explicitly picks a revision (the recover flow).
type Group struct {
	Key        string
	SessionID  string
	Revisions  []Revision // every distinct-digest revision, sorted lexicographically by digest
	Heads      []Revision // newest entry of each device bucket, sorted lexicographically by digest
	Conflicted bool       // >1 head: distinct device chains disagree on content
	Superseded int        // len(Revisions)-len(Heads): mid-chain revisions superseded inside their own bucket
}

// Detect implements the DetectV2 pipeline:
//
//  1. group input by (Key, SessionID)
//  2. inside each group, bucket by Device ("" = one shared unknown bucket)
//  3. per bucket keep the newest CapturedAt — ties resolve to the
//     lexicographically greatest Digest, and zero-time entries always lose to
//     any timestamped one
//  4. heads := kept entries across buckets
//  5. conflicted := len(heads) >= 2 (heads come from different buckets by
//     construction)
//
// Groups are returned ordered by Key then SessionID; identical digests within
// a group collapse to one entry (idempotent duplicates of the same bytes).
// The input slice is never mutated.
func Detect(revs []Revision) []Group {
	sorted := make([]Revision, len(revs))
	copy(sorted, revs)
	sort.Slice(sorted, func(i, j int) bool {
		return lessIdentityThenCanonical(sorted[i], sorted[j])
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
				continue // duplicate digest — same bytes seen again, not new information
			}
			g.Revisions = append(g.Revisions, r)
		}
		g.Heads = bucketHeads(g.Revisions)
		g.Conflicted = len(g.Heads) >= 2
		g.Superseded = len(g.Revisions) - len(g.Heads)
		out = append(out, g)
		i = j
	}
	return out
}

// lessIdentityThenCanonical orders revisions first by identity (Key, then
// SessionID, then Digest). Same-digest duplicates keep clustering adjacent
// while the tail of the comparator picks the best CANONICAL representative for
// that digest: richest sidecar knowledge wins (known device above unknown,
// timestamped above bare), then alphabetical device and earlier capture time
// as deterministic last resorts. Sorting before dedup makes the surviving
// entry independent of input order.
func lessIdentityThenCanonical(a, b Revision) bool {
	switch {
	case a.Key != b.Key:
		return a.Key < b.Key
	case a.SessionID != b.SessionID:
		return a.SessionID < b.SessionID
	case a.Digest != b.Digest:
		return a.Digest < b.Digest
	}
	aKnownDev, bKnownDev := a.Device != "", b.Device != ""
	switch {
	case aKnownDev != bKnownDev:
		return aKnownDev
	case a.CapturedAt.IsZero() != b.CapturedAt.IsZero():
		return !a.CapturedAt.IsZero()
	case a.Device != b.Device:
		return a.Device < b.Device
	case !a.CapturedAt.Equal(b.CapturedAt):
		return a.CapturedAt.Before(b.CapturedAt)
	default:
		return false
	}
}

// bucketHeads collapses one identity's distinct revisions into their per-
// device heads: every device's newest CapturedAt survives (ties choose the
// lexicographically greatest Digest; zero-time entries always lose to
// timestamped ones), mid-chain entries are superseded. Heads are returned
// sorted by Digest so reports derived from them stay byte-identical across
// runs regardless of map iteration order.
func bucketHeads(revs []Revision) []Revision {
	newestByDevice := make(map[string]Revision)
	for _, r := range revs {
		cur, ok := newestByDevice[r.Device]
		if !ok || newerHead(r, cur) {
			newestByDevice[r.Device] = r
		}
	}
	heads := make([]Revision, 0, len(newestByDevice))
	for _, h := range newestByDevice {
		heads = append(heads, h)
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i].Digest < heads[j].Digest })
	return heads
}

// newerHead reports whether candidate a should displace b as its device
// bucket's head: timestamped entries always beat zero-time ones; among two
// timestamped entries the later CapturedAt wins, ties fall to the greater
// Digest; between two zero-time entries the greater Digest keeps the outcome
// deterministic even though neither carries ordering evidence.
func newerHead(a, b Revision) bool {
	aZero, bZero := a.CapturedAt.IsZero(), b.CapturedAt.IsZero()
	switch {
	case aZero != bZero:
		return !aZero
	case !aZero:
		if !a.CapturedAt.Equal(b.CapturedAt) {
			return a.CapturedAt.After(b.CapturedAt)
		}
		return a.Digest > b.Digest
	default:
		return a.Digest > b.Digest
	}
}
