package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// revisionsListEnv builds a mixed-layout sync repo (legacy exports with and
// without import-meta plus revisions-layout payloads) with config wired.
func revisionsListEnv(t *testing.T) string {
	t.Helper()
	isolateDeviceEnv(t)
	repo := buildMixedLayoutTree(t)
	writeTestConfig(t, repo)
	return repo
}

func runCmdCapture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = fn() })
	return out, err
}

// The --json shape contract: every walker ref renders as one object with the
// exact field set, honest empty strings where no sidecar knowledge exists,
// and source naming its storage layout.
func TestRevisionsListJSONShapeOverMixedFixture(t *testing.T) {
	revisionsListEnv(t)

	out, err := runCmdCapture(t, func() error { return cmdRevisions([]string{"list", "--json"}) })
	if err != nil {
		t.Fatalf("revisions list --json: %v", err)
	}
	var rows []revisionJSON
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, out)
	}
	if len(rows) != 4 {
		t.Fatalf("want 4 rows over the mixed fixture, got %d:\n%s", len(rows), out)
	}

	bySource := map[string]int{}
	for _, r := range rows {
		bySource[r.Source]++
		switch r.Source {
		case "revision":
			if !strings.Contains(r.PayloadPath, filepath.Join("sessions", "ses_a", "revisions")) &&
				!strings.Contains(filepath.ToSlash(r.PayloadPath), "sessions/ses_a/revisions/") {
				t.Errorf("revision row payload path wrong: %s", r.PayloadPath)
			}
			if r.Key != "ka" || r.SessionID != "ses_a" {
				t.Errorf("revision row identity wrong: %#v", r)
			}
		case "legacy":
			if !strings.Contains(filepath.ToSlash(r.PayloadPath), "/export/") {
				t.Errorf("legacy row payload path wrong: %s", r.PayloadPath)
			}
		default:
			t.Fatalf("unknown source label %q", r.Source)
		}
	}
	if bySource["revision"] != 2 || bySource["legacy"] != 2 {
		t.Errorf("want 2 revision + 2 legacy rows, got %#v", bySource)
	}

	// Deterministic sort by (key, session, digest).
	if !(rows[0].Key == "ka" && rows[1].Key == "ka" && rows[0].Digest < rows[1].Digest &&
		rows[2].SessionID == "ses_b" && rows[3].SessionID == "ses_nometa") {
		t.Errorf("row ordering wrong:\n%#v", rows)
	}

	// Sidecar knowledge maps into fields; absence stays an empty string.
	var withSidecar, withoutSidecar bool
	for _, r := range rows {
		if r.Status == "captured" && r.Device == "dev-a" {
			withSidecar = true
			if r.CapturedAt == "" || r.ProducerVersion == "" {
				t.Errorf("sidecar-bearing row must expose captured_at/producer_version: %#v", r)
			}
		}
		if r.SessionID == "ses_nometa" {
			withoutSidecar = true
			if r.Device != "" || r.Alias != "" || r.CapturedAt != "" ||
				r.ProducerVersion != "" || r.Status != "" {
				t.Errorf("no-sidecar row must render absent knowledge as empty strings: %#v", r)
			}
		}
	}
	if !withSidecar || !withoutSidecar {
		t.Errorf("expected one captured row (dev-a sidecar) and one bare legacy row, got %#v", rows)
	}

	// Legacy export WITH import-meta carries the synthetic migrated status.
	var migratedSeen bool
	for _, r := range rows {
		if r.SessionID == "ses_b" && r.Status == "migrated" {
			migratedSeen = true
		}
	}
	if !migratedSeen {
		t.Errorf("ses_b (import-meta present) must report status migrated:\n%s", out)
	}
}

func TestRevisionsListFilters(t *testing.T) {
	revisionsListEnv(t)

	countRows := func(args ...string) int {
		t.Helper()
		// Row counting parses JSON, so the listing must be asked for
		// explicitly — the human table stays the default output.
		out, err := runCmdCapture(t, func() error {
			return cmdRevisions(append([]string{"list", "--json"}, args...))
		})
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		var rows []revisionJSON
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			t.Fatalf("%v: parse: %v\n%s", args, err, out)
		}
		return len(rows)
	}
	if got := countRows("--project", "ka"); got != 2 {
		t.Errorf("--project ka → %d rows, want 2", got)
	}
	if got := countRows("--session", "ses_b"); got != 1 {
		t.Errorf("--session ses_b → %d rows, want 1", got)
	}
	if got := countRows("--project", "ka", "--session", "ses_b"); got != 0 {
		t.Errorf("combined filters → %d rows, want 0", got)
	}

	// Empty filtered result is a JSON array, never null.
	out, err := runCmdCapture(t, func() error {
		return cmdRevisions([]string{"list", "--session", "nope", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("empty result = %q, want []", strings.TrimSpace(out))
	}
}

func TestRevisionsListHumanTable(t *testing.T) {
	revisionsListEnv(t)

	out, err := runCmdCapture(t, func() error { return cmdRevisions([]string{"list"}) })
	if err != nil {
		t.Fatalf("revisions list: %v", err)
	}
	for _, want := range []string{"KEY", "SESSION", "DIGEST12", "DEVICE", "CAPTURED", "STATUS", "SRC"} {
		if !strings.Contains(out, want) {
			t.Errorf("table header missing column %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "legacy") || !strings.Contains(out, "revision") {
		t.Errorf("SRC column values missing:\n%s", out)
	}
	if !strings.Contains(out, "(unknown)") {
		t.Errorf("sidecar-less rows must degrade CAPTURED/DEVICE to (unknown):\n%s", out)
	}
	if !strings.Contains(out, "dev-a") {
		t.Errorf("device id from sidecar missing:\n%s", out)
	}

	// Subcommand dispatch rejects junk loudly.
	if _, err := runCmdCapture(t, func() error { return cmdRevisions([]string{"bogus"}) }); err == nil {
		t.Error("unknown revisions subcommand must fail")
	}
	if _, err := runCmdCapture(t, func() error { return cmdRevisionsList([]string{"--bogus"}) }); err == nil {
		t.Error("unknown list flag must fail")
	}
}

func TestConflictsJSONShapeAndCounts(t *testing.T) {
	conflictReceiveEnv(t) // ses_conf conflicted ×2 digests, ses_solo clean

	out, err := runCmdCapture(t, func() error { return cmdConflicts([]string{"--json"}) })
	if err != nil {
		t.Fatalf("conflicts --json: %v", err)
	}
	var groups []conflictGroupJSON
	if err := json.Unmarshal([]byte(out), &groups); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, out)
	}
	if len(groups) != 2 {
		t.Fatalf("want 2 groups (1 conflicted + 1 clean), got %d:\n%s", len(groups), out)
	}

	var conflicted, clean *conflictGroupJSON
	for i := range groups {
		switch {
		case groups[i].Conflicted:
			conflicted = &groups[i]
		default:
			clean = &groups[i]
		}
	}
	if conflicted == nil || clean == nil {
		t.Fatalf("want exactly one conflicted and one clean group: %#v", groups)
	}
	if conflicted.Key != "fixture-key" && conflicted.Key == "" {
		t.Errorf("conflicted group key missing: %#v", conflicted)
	}
	if conflicted.SessionID != "ses_conf" || len(conflicted.Revisions) != 2 {
		t.Errorf("conflicted group wrong: %#v", conflicted)
	}
	for _, rev := range conflicted.Revisions {
		if rev.Digest == "" {
			t.Errorf("conflict revision digest required: %#v", rev)
		}
		if rev.Device == "" {
			t.Errorf("conflict revision device label required (alias or id): %#v", rev)
		}
		if rev.CapturedAt == "" {
			t.Errorf("conflict revision captured_at required: %#v", rev)
		}
	}
	if clean.Conflicted || clean.SessionID != "ses_solo" || len(clean.Revisions) != 1 {
		t.Errorf("clean group wrong: %#v", clean)
	}
}

func TestConflictsHumanReportAlwaysExitsZero(t *testing.T) {
	conflictReceiveEnv(t)

	out, err := runCmdCapture(t, func() error { return cmdConflicts(nil) })
	if err != nil {
		t.Fatalf("report tool must exit 0 even with conflicts: %v", err)
	}
	for _, want := range []string{
		"CONFLICT: ses_conf has 2 preserved revisions — nothing restored",
		"a1b2c3d4e5f6", // placeholder replaced below
	} {
		_ = want // handled after re-check
	}
	if !strings.Contains(out, "CONFLICT: ses_conf has 2 preserved revisions — nothing restored") {
		t.Errorf("conflicted session report missing:\n%s", out)
	}
	first, _ := sesConfDigests()
	if !strings.Contains(out, first[:6]+"…") {
		t.Errorf("conflict report should reuse receive's digest rendering:\n%s", out)
	}
	if !strings.Contains(out, "clean: ses_solo") {
		t.Errorf("clean sessions should be listed too:\n%s", out)
	}
	if !strings.Contains(out, "1 conflicted session(s), 1 clean") {
		t.Errorf("totals line missing:\n%s", out)
	}

	if _, err := runCmdCapture(t, func() error { return cmdConflicts([]string{"--bogus"}) }); err == nil {
		t.Error("unknown conflicts flag must fail")
	}
}
