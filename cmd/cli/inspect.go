package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"agentsync/internal/syncrepo"
)

const revisionsUsage = `agent-sync revisions — inspect stored session revisions

Usage:
  agent-sync revisions list [--project <key>] [--session <id>] [--json]

Lists every discoverable revision across BOTH storage layouts
(legacy opencode/<key>/export/<sid>.json and immutable
opencode/<key>/sessions/<sid>/revisions/<digest>.json), sorted by
project key, session id, digest. Read-only inspection — nothing is
restored, migrated, or committed.

  --project <key>   only revisions under this canonical project key
  --session <id>    only revisions of this original session id
  --json            emit a machine-readable JSON array instead of the
                    human table
`

const conflictsUsage = `agent-sync conflicts — report same-session conflict groups

Usage:
  agent-sync conflicts [--json]

Groups stored revisions by canonical project key + original session id
and reports every group. A group holding more than one DISTINCT content
digest is a conflict: receive never restores it automatically, and every
revision of it stays archive-only until you choose one with
"agent-sync recover <session-id>". This command is pure reporting and
always exits 0; use --json for machine-readable output.
`

// revisionJSON is one row of `revisions list --json`. Absent sidecar
// knowledge (legacy exports without import-meta) renders as empty strings —
// absence is expressed, never guessed (AGENTS.md invariant #4).
type revisionJSON struct {
	Key             string `json:"key"`
	SessionID       string `json:"session_id"`
	Digest          string `json:"digest"`
	Device          string `json:"device"`
	Alias           string `json:"alias"`
	CapturedAt      string `json:"captured_at"`
	ProducerVersion string `json:"producer_version"`
	Status          string `json:"status"`
	Source          string `json:"source"` // "legacy" | "revision"
	PayloadPath     string `json:"payload_path"`
}

// conflictRevisionJSON is one entry of a conflict group's revision list in
// JSON output.
type conflictRevisionJSON struct {
	Digest     string `json:"digest"`
	Device     string `json:"device"`
	CapturedAt string `json:"captured_at"`
}

// conflictGroupJSON is one element of `conflicts --json`: every known
// revision of one identity plus its verdict.
type conflictGroupJSON struct {
	Key        string                 `json:"key"`
	SessionID  string                 `json:"session_id"`
	Revisions  []conflictRevisionJSON `json:"revisions"`
	Conflicted bool                   `json:"conflicted"`
}

// cmdRevisions dispatches the inspection subcommands ("list" today).
func cmdRevisions(args []string) error {
	if len(args) == 0 || args[0] != "list" {
		return fmt.Errorf("unknown revisions subcommand %q — want: agent-sync revisions list", subCommandName(args))
	}
	return cmdRevisionsList(args[1:])
}

func subCommandName(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// cmdRevisionsList implements agent-sync revisions list. findRevisions is
// already sorted by (Key, SessionID, Digest); filters narrow that output,
// they never reorder it.
func cmdRevisionsList(args []string) error {
	project := ""
	sessionID := ""
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project":
			if i+1 >= len(args) {
				return fmt.Errorf("--project requires a canonical key")
			}
			project = args[i+1]
			i++
		case "--session":
			if i+1 >= len(args) {
				return fmt.Errorf("--session requires a session id")
			}
			sessionID = args[i+1]
			i++
		case "--json":
			asJSON = true
		default:
			return fmt.Errorf("unknown revisions list flag %q", args[i])
		}
	}

	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	repo := syncrepo.Open(cfg.Sync.RepoPath)
	if !repo.Exists() {
		return fmt.Errorf("sync repo not initialized at %s — run `agent-sync init`", cfg.Sync.RepoPath)
	}
	refs, err := findRevisions(repo.Path)
	if err != nil {
		return err
	}

	rows := make([]revisionJSON, 0, len(refs))
	for _, ref := range refs {
		if project != "" && ref.Key != project {
			continue
		}
		if sessionID != "" && ref.SessionID != sessionID {
			continue
		}
		row := revisionJSON{
			Key:         ref.Key,
			SessionID:   ref.SessionID,
			Digest:      ref.Digest,
			PayloadPath: ref.PayloadPath,
			Source:      sourceLabel(ref),
		}
		if ref.Meta != nil {
			row.Device = ref.Meta.SourceDeviceID
			row.Alias = ref.Meta.DeviceAlias
			if !ref.Meta.CapturedAt.IsZero() {
				row.CapturedAt = ref.Meta.CapturedAt.UTC().Format(time.RFC3339)
			}
			row.ProducerVersion = ref.Meta.ProducerVersion
			row.Status = ref.Meta.Status
		}
		rows = append(rows, row)
	}

	if asJSON {
		data, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	if len(rows) == 0 {
		fmt.Println("No revisions found.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tSESSION\tDIGEST12\tDEVICE\tCAPTURED\tSTATUS\tSRC")
	for _, r := range rows {
		device := r.Alias
		if device == "" {
			device = r.Device
		}
		captured := r.CapturedAt
		if captured == "" {
			captured = "(unknown)"
		}
		status := r.Status
		if status == "" {
			status = "-"
		}
		// Human tables shorten timestamps; --json keeps full RFC3339.
		capturedShort := captured
		if t, perr := time.Parse(time.RFC3339, captured); perr == nil {
			capturedShort = shortTime(t)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Key, r.SessionID, digest12(r.Digest), device, capturedShort, status, r.Source)
	}
	return tw.Flush()
}

// sourceLabel names which storage layout produced a ref.
func sourceLabel(ref revisionRef) string {
	if ref.Legacy {
		return "legacy"
	}
	return "revision"
}

// cmdConflicts implements agent-sync conflicts. Pure reporting: detection
// over the walker, explicit per-conflict reports in receive's style, always
// exit 0 — a conflict is a state to surface, not an error to crash on.
func cmdConflicts(args []string) error {
	asJSON := false
	for _, arg := range args {
		switch arg {
		case "--json":
			asJSON = true
		default:
			return fmt.Errorf("unknown conflicts flag %q", arg)
		}
	}

	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	repo := syncrepo.Open(cfg.Sync.RepoPath)
	if !repo.Exists() {
		return fmt.Errorf("sync repo not initialized at %s — run `agent-sync init`", cfg.Sync.RepoPath)
	}
	refs, err := findRevisions(repo.Path)
	if err != nil {
		return err
	}
	groups := buildConflictGroups(refs)

	conflictedCount, cleanCount := 0, 0
	out := make([]conflictGroupJSON, 0, len(groups))
	for _, gi := range groups {
		entry := conflictGroupJSON{
			Key:        gi.Group.Key,
			SessionID:  gi.Group.SessionID,
			Revisions:  make([]conflictRevisionJSON, 0, len(gi.Group.Revisions)),
			Conflicted: gi.Group.Conflicted,
		}
		for _, rev := range gi.Group.Revisions {
			ref, ok := primaryRef(gi.Refs, rev.Digest)
			capturedAt := ""
			device := ""
			if ok && ref.Meta != nil && !ref.Meta.CapturedAt.IsZero() {
				capturedAt = ref.Meta.CapturedAt.UTC().Format(time.RFC3339)
			}
			if ok {
				device = deviceLabel(ref.Meta)
			}
			entry.Revisions = append(entry.Revisions, conflictRevisionJSON{
				Digest: rev.Digest, Device: device, CapturedAt: capturedAt,
			})
		}
		if gi.Group.Conflicted {
			conflictedCount++
		} else {
			cleanCount++
		}
		out = append(out, entry)
	}

	if asJSON {
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	for _, entry := range out {
		if !entry.Conflicted {
			fmt.Printf("clean: %s (%d revision(s))\n", entry.SessionID, len(entry.Revisions))
			continue
		}
		gi := findConflictGroup(groups, entry.Key, entry.SessionID)
		for _, line := range conflictReport(gi) {
			fmt.Println(line)
		}
	}
	if hint := metaRepairHint(refs); hint != "" {
		fmt.Println(hint)
	}
	fmt.Printf("%d conflicted session(s), %d clean\n", conflictedCount, cleanCount)
	return nil
}

// findConflictGroup re-finds one group by identity for report reuse.
func findConflictGroup(groups []conflictGroup, key, sessionID string) conflictGroup {
	for _, gi := range groups {
		if gi.Group.Key == key && gi.Group.SessionID == sessionID {
			return gi
		}
	}
	return conflictGroup{} // unreachable: entries derive from these groups
}
