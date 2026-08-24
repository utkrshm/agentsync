package main

import (
	"fmt"
	"sort"
	"time"

	"agentsync/internal/conflict"
	"agentsync/internal/receivestate"
	"agentsync/internal/revision"
)

// conflictGroup pairs a detected conflict.Detect group with every walker ref
// that belongs to it, so reporting and payload selection can use full
// sidecar/path knowledge while grouping stays digest-based.
type conflictGroup struct {
	Group conflict.Group
	Refs  []revisionRef // all refs of this (Key, SessionID), walker order
}

// buildConflictGroups converts walker refs into deterministic conflict groups
// (docs/session-conflict-handling-plan.md §3): grouped by canonical key +
// original session id, identical digests deduped, ≥2 distinct digests flagged
// conflicted. Groups are ordered by Key then SessionID; refs keep walker order.
func buildConflictGroups(refs []revisionRef) []conflictGroup {
	crevs := make([]conflict.Revision, 0, len(refs))
	for _, r := range refs {
		crevs = append(crevs, conflict.Revision{Key: r.Key, SessionID: r.SessionID, Digest: r.Digest})
	}
	groups := conflict.Detect(crevs)

	byIdentity := make(map[string][]revisionRef, len(groups))
	for _, r := range refs {
		id := r.Key + "\x00" + r.SessionID
		byIdentity[id] = append(byIdentity[id], r)
	}
	out := make([]conflictGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, conflictGroup{
			Group: g,
			Refs:  byIdentity[g.Key+"\x00"+g.SessionID],
		})
	}
	return out
}

// primaryRef selects the most informative revisionRef for one digest within a
// group: sidecar knowledge first, revisions layout over legacy exports, then
// payload path as tiebreaker — so repeated runs always report and write back
// from the same artifact.
func primaryRef(refs []revisionRef, digest string) (revisionRef, bool) {
	best := -1
	for i := range refs {
		if refs[i].Digest != digest {
			continue
		}
		if best == -1 || betterRef(refs[i], refs[best]) {
			best = i
		}
	}
	if best == -1 {
		return revisionRef{}, false
	}
	return refs[best], true
}

// betterRef ranks a over b: any Meta beats none, non-legacy beats legacy,
// lexicographic payload path breaks remaining ties deterministically.
func betterRef(a, b revisionRef) bool {
	switch {
	case refRank(a) != refRank(b):
		return refRank(a) < refRank(b)
	default:
		return a.PayloadPath < b.PayloadPath
	}
}

func refRank(r revisionRef) int {
	switch {
	case !r.Legacy && r.Meta != nil:
		return 0 // revisions layout with real sidecar
	case r.Legacy && r.Meta != nil:
		return 1 // legacy export with synthetic migrated meta
	case !r.Legacy:
		return 2 // revisions layout, missing/corrupt sidecar
	default:
		return 3 // legacy export without import-meta
	}
}

// shouldSkip reports whether a previously recorded outcome already closes the
// write-back lifecycle for a digest×path pair: verified/degraded imports and
// all conflict-handling verdicts (archive-only, conflicted, preserved,
// duplicated) must never be re-attempted by receive. Only busy/failed stay in
// the retry loop.
func shouldSkip(previous receivestate.Outcome) bool {
	return receivestate.Terminal(previous.Status)
}

// shortDigest renders a digest for human reports: a fixed prefix plus an
// ellipsis, never enough to be an identifier of record (the full digest stays
// in filenames and state).
func shortDigest(digest string) string {
	const prefixLen = 6
	switch {
	case digest == "":
		return "?"
	case len(digest) <= prefixLen:
		return digest
	default:
		return digest[:prefixLen] + "…"
	}
}

// deviceLabel is the machine-facing device attribution used by --json output:
// alias, else durable device id, else "(unknown)". Kept separate from the
// human label so JSON contracts stay stable across presentation changes.
func deviceLabel(m *revision.Meta) string {
	if m == nil {
		return "(unknown)"
	}
	if m.DeviceAlias != "" {
		return m.DeviceAlias
	}
	if m.SourceDeviceID != "" {
		return m.SourceDeviceID
	}
	return "(unknown)"
}

// deviceHumanLabel is the report-facing variant: it distinguishes a revision
// whose origin predates provenance (migrated — permanently unknowable without
// guessing, invariant #4) from one whose sidecar was never written (usually a
// crash-interrupted capture — repairable by re-sending the session).
func deviceHumanLabel(m *revision.Meta) string {
	if m == nil {
		return "(unknown)"
	}
	if m.DeviceAlias != "" {
		return m.DeviceAlias
	}
	if m.SourceDeviceID != "" {
		return m.SourceDeviceID
	}
	if m.Status == "migrated" {
		return "migrated"
	}
	return "(unknown)"
}

// shortTime renders capture times for humans: minute precision in UTC. Full
// RFC3339 remains available in all --json output.
func shortTime(t time.Time) string {
	if t.IsZero() {
		return "(unknown)"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// capturedLabel renders the sidecar capture time; absent knowledge prints
// "(unknown)" rather than a misleading zero time.
func capturedLabel(m *revision.Meta) string {
	if m == nil || m.CapturedAt.IsZero() {
		return "(unknown)"
	}
	return shortTime(m.CapturedAt)
}

// conflictReportLine is one indented revision row of a conflict report:
//
//	a3f9c1… laptop 2026-08-20 14:02 UTC
//
// Producer versions were dropped from reports as visual noise; they remain in
// `revisions list`. Legacy or sidecar-less revisions degrade to "(unknown)" /
// "migrated" instead of guessing provenance (AGENTS.md invariant #4).
func conflictReportLine(ref revisionRef) string {
	return fmt.Sprintf("%s %s %s",
		shortDigest(ref.Digest), deviceHumanLabel(ref.Meta), capturedLabel(ref.Meta))
}

// lastModifiedLine summarizes which device produced the newest known
// revision of a conflicted session. Derived from existing sidecars only.
func lastModifiedLine(gi conflictGroup) string {
	newest := newestRevisionRef(gi.Refs)
	if newest.Meta == nil || newest.Meta.CapturedAt.IsZero() {
		return "  last modified at an unrecorded time"
	}
	return fmt.Sprintf("  last modified by %s at %s",
		deviceHumanLabel(newest.Meta), shortTime(newest.Meta.CapturedAt))
}

// metaRepairHint returns a one-line remediation hint when any revision lacks
// sidecar metadata; empty string when every ref is fully attributed.
func metaRepairHint(refs []revisionRef) string {
	for _, r := range refs {
		if r.Meta == nil || r.Meta.SourceDeviceID == "" {
			return "hint: re-sending a session repairs missing revision metadata."
		}
	}
	return ""
}

// conflictReport builds the explicit multi-line report printed for a
// conflicted session: header, one line per preserved revision (digest-sorted,
// matching group.Revisions order), and the actionable recovery hint.
// Detection is passive — nothing is restored here — so the report is stable
// across runs.
func conflictReport(gi conflictGroup) []string {
	lines := []string{fmt.Sprintf(
		"CONFLICT: %s has %d preserved revisions — nothing restored",
		gi.Group.SessionID, len(gi.Group.Revisions))}
	for _, rev := range gi.Group.Revisions {
		ref, ok := primaryRef(gi.Refs, rev.Digest)
		if !ok {
			continue // unreachable: group members come from these refs
		}
		lines = append(lines, "  "+conflictReportLine(ref))
	}
	lines = append(lines, lastModifiedLine(gi))
	lines = append(lines, fmt.Sprintf(
		"Run `agent-sync recover %s` to restore a chosen revision.",
		gi.Group.SessionID))
	return lines
}

// newestRevisionRef picks which revision's metadata labels a resume entry,
// anchors the conflict report's "last modified" summary, and which revision
// confirmNewestConflictResume offers: the latest CapturedAt among
// sidecar-bearing refs; when no sidecar records a usable time, the last ref
// in lexicographic digest order as the deterministic fallback. Ties on equal
// timestamps keep the first in walker order (digest-sorted), so the choice is
// repeatable.
func newestRevisionRef(refs []revisionRef) revisionRef {
	var best revisionRef
	var bestTime time.Time
	found := false
	for _, r := range refs {
		if r.Meta == nil || r.Meta.CapturedAt.IsZero() {
			continue
		}
		if !found || r.Meta.CapturedAt.After(bestTime) {
			best, bestTime, found = r, r.Meta.CapturedAt, true
		}
	}
	if found {
		return best
	}
	fallback := append([]revisionRef(nil), refs...)
	sort.Slice(fallback, func(i, j int) bool {
		a, b := fallback[i], fallback[j]
		switch {
		case a.Digest != b.Digest:
			return a.Digest < b.Digest
		default:
			return a.PayloadPath < b.PayloadPath
		}
	})
	if len(fallback) == 0 {
		return revisionRef{}
	}
	return fallback[len(fallback)-1]
}
