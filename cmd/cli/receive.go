package main

import (
	"fmt"
	"os"
	"time"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/receivestate"
	"agentsync/internal/repoindex"
	"agentsync/internal/session"
	"agentsync/internal/syncrepo"
)

const receiveUsage = `agent-sync receive — pull the sync repo and restore pending sessions locally

Usage:
  agent-sync receive [--dry-run]

Fast-forward pulls the sync repo (diverged history is refused, never
auto-merged or force-pushed), then writes back every new export into
local clones of its project, resolved via the repo-index cache.

Same-session conflicts are detected BEFORE any write-back: a session with
multiple distinct preserved revisions is reported explicitly and nothing is
restored — every revision stays archive-only until you choose one with
"agent-sync recover <session-id>".

Per-clone safety guards: UID-scoped check that opencode is not running
(busy clones retry later with backoff), exact opencode version pinning,
optional trusted-path/binary-drift checks ([producer] config), the
project_id/directory patch after import, and verification against the
live database.

Outcomes are tracked per artifact digest + clone in
~/.cache/agent-sync/receive-state.json. Busy and failed clones retry on
later runs; conflicted sessions are marked archive-only per revision and
never retried automatically. When one session imports into multiple
clones, the degraded one-to-one association outcome is reported explicitly.

  --dry-run    print what would be written back (including would-be
               archive-only conflict verdicts); change nothing
`

// cmdReceive pulls the sync repo and writes back any new OpenCode sessions
// into local clones of the matching project, applying the
// project_id/directory patch.
//
// Sessions are processed as CONFLICT GROUPS, not raw refs: revisions are
// grouped by canonical key + original session id first, and detection runs
// BEFORE any write-back (docs/session-conflict-handling-plan.md §3). A
// conflicted group is never restored — every revision is recorded
// archive-only per digest×clone and an explicit report names each one.
// Write-back for clean groups is broadcast across EVERY local clone resolved
// for a session's canonical key (SPEC-DOC.md §4.1); a clone where opencode is
// currently running is skipped and retried on the next pull (AGENTS.md
// invariant #2).
func cmdReceive(args []string) error {
	dryRun := false
	for _, arg := range args {
		switch arg {
		case "--dry-run":
			dryRun = true
		default:
			return fmt.Errorf("unknown receive flag %q", arg)
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
	if cfg.Sync.Remote != "" {
		if err := repo.PullForced(); err != nil {
			return fmt.Errorf("sync-repo pull: %w", err)
		}
	}
	// Fresh-join migration evaluates the POST-pull state (docs/hardening-plan.md
	// WS-B "Migration semantics"): a pulled-but-unmigrated legacy-only repo is
	// migrated silently with a one-line notice; mixed-state repos only get a
	// hint. Skipped under --dry-run because silent migration commits.
	if !dryRun {
		if err := migrateIfNeededOnce(repo); err != nil {
			return err
		}
	}
	idx, err := openRepoIndex()
	if err != nil {
		return err
	}
	statePath, err := receivestate.DefaultPath()
	if err != nil {
		return err
	}
	local, err := receivestate.Open(statePath)
	if err != nil {
		return err
	}
	refs, err := findRevisions(repo.Path)
	if err != nil {
		return err
	}
	groups := buildConflictGroups(refs)
	ad := opencode.NewAdapter()
	ad.TrustedPath = cfg.Producer.TrustedPath
	ad.StrictCheck = cfg.Producer.StrictCheck
	attempted := 0
	blocked := 0
	for _, gi := range groups {
		sessionID := gi.Group.SessionID
		key := gi.Group.Key

		// Conflicted: report explicitly, restore nothing, mark archive-only.
		// A conflict never advances any acknowledgement to verified or to
		// busy/failed retry semantics (plan §4).
		if gi.Group.Conflicted {
			for _, line := range conflictReport(gi) {
				fmt.Println(line)
			}
			if dryRun {
				fmt.Printf("DRY RUN: would mark %d revision(s) of %s archive-only; nothing restored.\n",
					len(gi.Group.Revisions), sessionID)
				continue
			}
			recordConflictArchiveOnly(local, idx, gi)
			continue
		}

		// Single-revision group: normal guarded write-back flow.
		ref, ok := primaryRef(gi.Refs, gi.Group.Revisions[0].Digest)
		if !ok {
			continue // unreachable: group members derive from these refs
		}
		digest := ref.Digest
		title := ""
		if ref.Meta != nil {
			title = ref.Meta.Title
		}
		candidates, err := idx.Resolve(session.CanonicalKey(key))
		if err != nil {
			fmt.Fprintf(os.Stderr, "receive: action=resolve session=%s status=error error=%q\n", sessionID, err)
			continue
		}
		if len(candidates) == 0 {
			fmt.Printf("Archived only: %s (%s) has no local clone for %s. Run agent-sync index after cloning.\n", sessionID, title, key)
			continue
		}
		paths := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			if err := repoindex.ValidateCandidate(session.CanonicalKey(key), candidate.LocalPath); err != nil {
				fmt.Fprintf(os.Stderr, "receive: action=revalidate session=%s path=%q status=skip error=%q\n", sessionID, candidate.LocalPath, err)
				if !dryRun {
					_ = local.Put(receivestate.Outcome{ArtifactDigest: digest, SessionID: sessionID, CandidatePath: candidate.LocalPath, Status: receivestate.StatusFailed, LastError: err.Error()})
				}
				continue
			}
			previous, ok, err := local.Get(digest, candidate.LocalPath)
			if err != nil {
				return err
			}
			if ok && shouldSkip(previous) {
				fmt.Printf("Already processed: %s at %s (%s).\n", sessionID, candidate.LocalPath, previous.Status)
				continue
			}
			if ok && (previous.Status == receivestate.StatusBusy || previous.Status == receivestate.StatusFailed) &&
				!previous.NextAttempt.IsZero() && time.Now().UTC().Before(previous.NextAttempt) {
				fmt.Printf("Retry deferred: %s at %s until %s (%s).\n", sessionID, candidate.LocalPath, previous.NextAttempt.UTC().Format(time.RFC3339), previous.Status)
				continue
			}
			paths = append(paths, candidate.LocalPath)
		}
		if len(paths) == 0 {
			continue
		}
		s := &session.Session{ID: sessionID, Tool: session.ToolOpenCode, CanonicalKey: session.CanonicalKey(key), PayloadPath: ref.PayloadPath}
		if err := ad.ValidateArtifact(s); err != nil {
			blocked++
			fmt.Fprintf(os.Stderr, "receive: action=validate session=%s status=failed error=%q\n", sessionID, err)
			if !dryRun {
				for _, path := range paths {
					_ = local.Put(receivestate.Outcome{ArtifactDigest: digest, SessionID: sessionID, CandidatePath: path, Status: receivestate.StatusFailed, LastError: err.Error()})
				}
			}
			continue
		}
		if dryRun {
			fmt.Printf("DRY RUN: would write back %s (%s) to %d validated clone(s).\n", sessionID, title, len(paths))
			continue
		}
		attempted++
		fmt.Printf("Writing back %s (%s) to %d clone(s)...\n", sessionID, title, len(paths))
		res := ad.BroadcastWriteBack(s, paths)
		recordBroadcast(local, digest, sessionID, res)
		reportBroadcast(res, sessionID, key)
	}
	if hint := metaRepairHint(refs); hint != "" {
		fmt.Println(hint)
	}
	if dryRun {
		fmt.Println("Dry run complete; no OpenCode or local receive state was changed.")
	} else if blocked > 0 {
		return fmt.Errorf("%d session(s) were blocked before write-back; resolve the reported compatibility or artifact errors and retry", blocked)
	} else if attempted == 0 {
		fmt.Println("No pending sessions required write-back.")
	}
	return nil
}

// recordConflictArchiveOnly marks every revision of a conflicted group
// archive-only, keyed per digest×candidate-path. It resolves candidates via
// the repo-index cache like any other session, but NEVER imports anything and
// NEVER writes busy/failed retry state — a conflict supersedes stale retry
// outcomes instead of feeding them.
//
// Detection must be repeatable without state churn: outcomes that already say
// archive-only for that digest+path are read before writing and skipped, so
// repeated runs print identical reports and leave last-attempt timestamps
// untouched (docs/session-conflict-handling-plan.md §3).
func recordConflictArchiveOnly(local *receivestate.Store, idx *repoindex.DB, gi conflictGroup) {
	sessionID := gi.Group.SessionID
	key := session.CanonicalKey(gi.Group.Key)
	candidates, err := idx.Resolve(key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "receive: action=resolve session=%s status=error error=%q\n", sessionID, err)
		return
	}
	if len(candidates) == 0 {
		title := ""
		if ref, ok := primaryRef(gi.Refs, gi.Group.Revisions[0].Digest); ok && ref.Meta != nil {
			title = ref.Meta.Title
		}
		fmt.Printf("Archived only: %s (%s) has no local clone for %s. Run agent-sync index after cloning.\n", sessionID, title, gi.Group.Key)
		return
	}
	for _, rev := range gi.Group.Revisions {
		for _, candidate := range candidates {
			if err := repoindex.ValidateCandidate(key, candidate.LocalPath); err != nil {
				fmt.Fprintf(os.Stderr, "receive: action=revalidate session=%s path=%q status=skip error=%q\n", sessionID, candidate.LocalPath, err)
				continue
			}
			previous, ok, err := local.Get(rev.Digest, candidate.LocalPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "receive: action=read-state session=%s path=%q status=error error=%q\n", sessionID, candidate.LocalPath, err)
				continue
			}
			if ok && (previous.Status == receivestate.StatusVerified || previous.Status == receivestate.StatusDegraded) {
				// A human explicitly restored this revision via `recover`.
				// Receive must never downgrade that decision back to
				// archive-only — it only reports the sibling situation.
				fmt.Printf("  restored: %s at %s (kept; recover chose this revision)\n", shortDigest(rev.Digest), candidate.LocalPath)
				continue
			}
			if !ok || previous.Status != receivestate.StatusArchiveOnly {
				if err := local.Put(receivestate.Outcome{
					ArtifactDigest: rev.Digest,
					SessionID:      sessionID,
					CandidatePath:  candidate.LocalPath,
					Status:         receivestate.StatusArchiveOnly,
				}); err != nil {
					fmt.Fprintf(os.Stderr, "receive: action=record path=%q status=error error=%q\n", candidate.LocalPath, err)
					continue
				}
			}
			fmt.Printf("  archive-only: %s at %s (conflict; not restored)\n", shortDigest(rev.Digest), candidate.LocalPath)
		}
	}
}

// reportBroadcast prints the write-back outcome, explicitly logging the
// degraded case (SPEC-DOC.md §4.1, AGENTS.md invariant #8).
func reportBroadcast(res opencode.BroadcastResult, sessionID, key string) {
	for _, b := range res.Busy {
		fmt.Printf("  busy: %s — opencode running there, skipped (retry on next pull)\n", b)
	}
	for _, i := range res.Imported {
		fmt.Printf("  verified in %s\n", i)
	}
	for _, failure := range res.Failed {
		fmt.Printf("  failed: %s — %s (will retry)\n", failure.Path, failure.Error)
	}
	if res.Degraded {
		fmt.Printf("  WARNING: %s imported into %d clones, but OpenCode's session↔project model is one-to-one — "+
			"the session is resumable in the LAST-imported clone only, not all of them (%s).\n",
			sessionID, len(res.Imported), key)
	}
	if len(res.Imported) == 0 && len(res.Busy) == 0 && len(res.Failed) == 0 {
		fmt.Printf("  no clone could be written back for %s\n", sessionID)
	}
}

func recordBroadcast(local *receivestate.Store, digest, sessionID string, res opencode.BroadcastResult) {
	for _, path := range res.Busy {
		if err := local.Put(receivestate.Outcome{ArtifactDigest: digest, SessionID: sessionID, CandidatePath: path, Status: receivestate.StatusBusy}); err != nil {
			fmt.Fprintf(os.Stderr, "receive: action=record path=%q status=error error=%q\n", path, err)
		}
	}
	for _, failure := range res.Failed {
		if err := local.Put(receivestate.Outcome{ArtifactDigest: digest, SessionID: sessionID, CandidatePath: failure.Path, Status: receivestate.StatusFailed, LastError: failure.Error}); err != nil {
			fmt.Fprintf(os.Stderr, "receive: action=record path=%q status=error error=%q\n", failure.Path, err)
		}
	}
	for i, path := range res.Imported {
		status := receivestate.StatusVerified
		if res.Degraded && i < len(res.Imported)-1 {
			status = receivestate.StatusDegraded
		}
		if err := local.Put(receivestate.Outcome{ArtifactDigest: digest, SessionID: sessionID, CandidatePath: path, Status: status}); err != nil {
			fmt.Fprintf(os.Stderr, "receive: action=record path=%q status=error error=%q\n", path, err)
		}
	}
}

// openRepoIndex opens the repo-index cache DB.
func openRepoIndex() (*repoindex.DB, error) {
	p, err := repoindex.DefaultPath()
	if err != nil {
		return nil, err
	}
	return repoindex.Open(p)
}
