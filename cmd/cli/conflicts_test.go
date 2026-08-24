package main

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"agentsync/internal/receivestate"
	"agentsync/internal/revision"
)

func metaRef(key, sid, digest, path string, m *revision.Meta) revisionRef {
	return revisionRef{SessionID: sid, Key: key, Digest: digest, PayloadPath: path, Meta: m}
}

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// conflictReportLine must name digest prefix, alias-or-id device, capture
// time and producer version — and degrade to "(unknown)" rather than guess
// when sidecar knowledge is missing (legacy refs, absent/corrupt meta).
func TestConflictReportLine(t *testing.T) {
	full := &revision.Meta{
		DeviceAlias:     "laptop",
		SourceDeviceID:  "dev-internal-id",
		CapturedAt:      ts(t, "2026-08-20T14:02:11Z"),
		ProducerVersion: "1.18.18",
	}
	noAlias := &revision.Meta{
		SourceDeviceID:  "dev-9876",
		CapturedAt:      ts(t, "2026-08-21T09:31:45Z"),
		ProducerVersion: "1.18.19",
	}
	noTimes := &revision.Meta{DeviceAlias: "desktop"}

	tests := []struct {
		name string
		ref  revisionRef
		want string
	}{
		{
			name: "alias preferred over device id",
			ref:  metaRef("k", "ses_x", "a3f9c1deadbeef", "/r/a3f9c1.json", full),
			want: "a3f9c1… laptop 2026-08-20 14:02 UTC",
		},
		{
			name: "device id when no alias",
			ref:  metaRef("k", "ses_x", "77b2e0cafe0000", "/r/77b2e0.json", noAlias),
			want: "77b2e0… dev-9876 2026-08-21 09:31 UTC",
		},
		{
			name: "nil meta degrades every field",
			ref:  metaRef("k", "ses_x", "f00d00", "/legacy/f00d00.json", nil),
			want: "f00d00 (unknown) (unknown)",
		},
		{
			name: "zero captured_at treated as unknown",
			ref:  metaRef("k", "ses_x", "beef01", "/r/beef01.json", noTimes),
			want: "beef01 desktop (unknown)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := conflictReportLine(tc.ref); got != tc.want {
				t.Fatalf("conflictReportLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShortDigest(t *testing.T) {
	tests := []struct{ in, want string }{
		{"a3f9c1deadbeef", "a3f9c1…"},
		{"abc123", "abc123"},   // at prefix length: shown whole
		{"abc1234", "abc123…"}, // one over: truncated
		{"a", "a"},
		{"", "?"},
	}
	for _, tc := range tests {
		if got := shortDigest(tc.in); got != tc.want {
			t.Errorf("shortDigest(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Grouping over walker refs: same digest via legacy + revisions layout
// collapses to one clean group; two distinct digests flag a conflict; groups
// order by key then session id.
func TestBuildConflictGroupsOverRefs(t *testing.T) {
	m1 := &revision.Meta{Title: "one"}
	m2 := &revision.Meta{Title: "two"}
	refs := []revisionRef{
		metaRef("kb", "ses_b", "dd", "/r/kb/b/dd.json", m2),
		metaRef("ka", "ses_a", "aa", "/r/ka/a/aa.json", m1),
		metaRef("ka", "ses_a", "bb", "/r/ka/a/bb.json", nil),
		metaRef("kc", "ses_c", "cc", "/legacy/kc/cc.json", m1),
		metaRef("kc", "ses_c", "cc", "/r/kc/c/cc.json", m1), // same bytes, other layout
		metaRef("ka", "ses_z", "ee", "/r/ka/z/ee.json", nil),
	}
	got := buildConflictGroups(refs)

	type sig struct {
		key       string
		sid       string
		digests   []string
		conflict  bool
		refCounts int
	}
	var gotSigs []sig
	for _, g := range got {
		s := sig{key: g.Group.Key, sid: g.Group.SessionID, conflict: g.Group.Conflicted, refCounts: len(g.Refs)}
		for _, r := range g.Group.Revisions {
			s.digests = append(s.digests, r.Digest)
		}
		gotSigs = append(gotSigs, s)
	}
	want := []sig{
		{key: "ka", sid: "ses_a", digests: []string{"aa", "bb"}, conflict: true, refCounts: 2},
		{key: "ka", sid: "ses_z", digests: []string{"ee"}, conflict: false, refCounts: 1},
		{key: "kb", sid: "ses_b", digests: []string{"dd"}, conflict: false, refCounts: 1},
		{key: "kc", sid: "ses_c", digests: []string{"cc"}, conflict: false, refCounts: 2},
	}
	if !reflect.DeepEqual(gotSigs, want) {
		t.Fatalf("groups = %+v\nwant %+v", gotSigs, want)
	}
}

// primaryRef must pick the most informative ref for a digest deterministically:
// real sidecar first, then legacy-with-synthetic-meta, then bare revisions
// payload, then bare legacy; payload path breaks remaining ties.
func TestPrimaryRefRanking(t *testing.T) {
	realSidecar := metaRef("k", "s", "aa", "/r/new.json", &revision.Meta{})
	syntheticMeta := metaRef("k", "s", "aa", "/legacy/export.json", &revision.Meta{})
	syntheticMeta.Legacy = true
	bareNew := revisionRef{SessionID: "s", Key: "k", Digest: "aa", PayloadPath: "/r/bare-new.json"}
	bareLegacy := revisionRef{SessionID: "s", Key: "k", Digest: "aa", PayloadPath: "/legacy/bare.json", Legacy: true}

	tests := []struct {
		name string
		refs []revisionRef
		want string
	}{
		{name: "real sidecar beats synthetic meta", refs: []revisionRef{syntheticMeta, realSidecar}, want: "/r/new.json"},
		{name: "real sidecar beats bare layouts", refs: []revisionRef{bareNew, bareLegacy, realSidecar}, want: "/r/new.json"},
		{name: "synthetic meta beats bare new", refs: []revisionRef{bareNew, syntheticMeta}, want: "/legacy/export.json"},
		{name: "bare new beats bare legacy", refs: []revisionRef{bareLegacy, bareNew}, want: "/r/bare-new.json"},
		{name: "path tiebreak within rank", refs: []revisionRef{
			{SessionID: "s", Key: "k", Digest: "aa", PayloadPath: "/z.json"},
			{SessionID: "s", Key: "k", Digest: "aa", PayloadPath: "/a.json"},
		}, want: "/a.json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref, ok := primaryRef(tc.refs, "aa")
			if !ok {
				t.Fatal("primaryRef returned !ok")
			}
			if filepath.ToSlash(ref.PayloadPath) != tc.want {
				t.Fatalf("picked %q, want %q", filepath.ToSlash(ref.PayloadPath), tc.want)
			}
		})
	}

	if _, ok := primaryRef([]revisionRef{{Digest: "zz"}}, "aa"); ok {
		t.Error("missing digest must return ok=false")
	}
}

// The extended skip set: verified/degraded plus all four conflict-handling
// verdicts close an outcome; busy/failed stay retryable.
func TestShouldSkipTerminalStatuses(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{receivestate.StatusVerified, true},
		{receivestate.StatusDegraded, true},
		{receivestate.StatusArchiveOnly, true},
		{receivestate.StatusConflicted, true},
		{receivestate.StatusPreserved, true},
		{receivestate.StatusDuplicated, true},
		{receivestate.StatusBusy, false},
		{receivestate.StatusFailed, false},
		{"", false},
		{"mysterious-future-status", false},
	}
	for _, tc := range tests {
		t.Run("status="+tc.status, func(t *testing.T) {
			got := shouldSkip(receivestate.Outcome{Status: tc.status})
			if got != tc.want {
				t.Fatalf("shouldSkip(%q) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// The full report shape is pinned so repeated receive runs print identical,
// actionable output.
func TestConflictReportShape(t *testing.T) {
	laptop := &revision.Meta{DeviceAlias: "laptop", CapturedAt: ts(t, "2026-08-20T14:02:11Z"), ProducerVersion: "1.18.18"}
	desktop := &revision.Meta{SourceDeviceID: "dev-desktop", CapturedAt: ts(t, "2026-08-21T09:31:45Z"), ProducerVersion: "1.18.18"}
	refs := []revisionRef{
		metaRef("k", "ses_x", "77b2e0fff", "/r/77b2e0.json", desktop),
		metaRef("k", "ses_x", "a3f9c1aaa", "/r/a3f9c1.json", laptop),
	}
	groups := buildConflictGroups(refs)
	lines := conflictReport(groups[0])
	want := []string{
		"CONFLICT: ses_x has 2 preserved revisions — nothing restored",
		// Digest-sorted: "77b2e0…" < "a3f9c1…" lexicographically.
		"  77b2e0… dev-desktop 2026-08-21 09:31 UTC",
		"  a3f9c1… laptop 2026-08-20 14:02 UTC",
		"  last modified by dev-desktop at 2026-08-21 09:31 UTC",
		"Run `agent-sync recover ses_x` to restore a chosen revision.",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("report:\n%s\nwant:\n%s", stringsJoin(lines), stringsJoin(want))
	}
}

func stringsJoin(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

func TestShortTime(t *testing.T) {
	ts := time.Date(2026, 8, 24, 7, 50, 7, 0, time.UTC)
	if got := shortTime(ts); got != "2026-08-24 07:50 UTC" {
		t.Errorf("shortTime = %q", got)
	}
	if got := shortTime(time.Time{}); got != "(unknown)" {
		t.Errorf("zero time should be (unknown), got %q", got)
	}
}

func TestDeviceHumanLabelMigrated(t *testing.T) {
	m := &revision.Meta{Status: "migrated"}
	if got := deviceHumanLabel(m); got != "migrated" {
		t.Errorf("migrated sidecar should label migrated, got %q", got)
	}
	if got := deviceHumanLabel(&revision.Meta{}); got != "(unknown)" {
		t.Errorf("captured-but-unattributed should stay (unknown), got %q", got)
	}
}

func TestLastModifiedLine(t *testing.T) {
	refs := []revisionRef{
		metaRef("k", "ses_x", "a3f9c1aaa", "/r/a.json",
			&revision.Meta{DeviceAlias: "laptop", CapturedAt: ts(t, "2026-08-20T14:02:11Z")}),
		metaRef("k", "ses_x", "77b2e0fff", "/r/b.json",
			&revision.Meta{SourceDeviceID: "dev-desktop", CapturedAt: ts(t, "2026-08-21T09:31:45Z")}),
	}
	gi := buildConflictGroups(refs)[0]
	want := "  last modified by dev-desktop at 2026-08-21 09:31 UTC"
	if got := lastModifiedLine(gi); got != want {
		t.Errorf("lastModifiedLine = %q, want %q", got, want)
	}

	orphan := []revisionRef{metaRef("k", "ses_y", "beef01", "/r/c.json", nil)}
	gi2 := buildConflictGroups(orphan)[0]
	if got := lastModifiedLine(gi2); got != "  last modified at an unrecorded time" {
		t.Errorf("unrecorded case = %q", got)
	}
}

func TestMetaRepairHint(t *testing.T) {
	full := []revisionRef{metaRef("k", "s", "d1", "/r/a.json",
		&revision.Meta{SourceDeviceID: "dev"})}
	if h := metaRepairHint(full); h != "" {
		t.Errorf("fully attributed refs must not hint, got %q", h)
	}
	orphaned := append(full, metaRef("k", "s", "d2", "/r/b.json", nil))
	if h := metaRepairHint(orphaned); h == "" {
		t.Error("missing-meta ref must produce a repair hint")
	}
}
