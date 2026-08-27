package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/config"
	"agentsync/internal/conflict"
	"agentsync/internal/receivestate"
	"agentsync/internal/repoindex"
	"agentsync/internal/session"
	"agentsync/internal/syncrepo"
)

const recoverUsage = `agent-sync recover — restore one chosen revision of a conflicted session

Usage:
  agent-sync recover <session-id> [--revision <digest-prefix>] [--dry-run]

Same-session conflicts are never restored automatically: every preserved
revision stays archive-only until you explicitly pick one. recover pulls
the sync repo first (refusing stale state), lists the session's
revisions — digest prefix, source device, capture time, producer
version — and runs the NORMAL guarded write-back pipeline (process
guard, exact version pin, project patch, verification) for the chosen
digest only. All sibling revisions are re-marked archive-only per clone;
nothing is deleted.

recover is re-runnable to SWITCH the active revision: the association
moves to the newly imported one while previously recovered copies stay
in OpenCode storage (one-to-one association, reported as degraded when a
single recovery imports into multiple clones).

  --revision <prefix>   pick directly by unique digest prefix instead of
                        the interactive picker; on a single-revision
                        session it also forces a manual re-store
  --both                RESERVED and permanently refused for now:
                        duplicating one session into two requires new
                        session IDs (stage-5 duplication mechanism),
                        which is unproven against live OpenCode storage.
                        Recover one revision, or restore different
                        revisions on different devices/clones.
  --dry-run             print what would be restored; change nothing
`

// bothFlagError is the permanent fail-closed answer to --both. Recovering
// BOTH revisions into one clone is impossible safely today: they share the
// original session id, so importing both collides with OpenCode's
// one-session-ID model (docs/session-conflict-handling-plan.md appendix).
func bothFlagError() error {
	return fmt.Errorf("--both is reserved: safely recovering both revisions needs new session IDs " +
		"(the stage-5 duplication mechanism), which is unproven against live OpenCode storage — " +
		"recover ONE revision instead, or restore different revisions on different devices/clones")
}

// singleRevisionNotice is printed when the named session has no conflict at
// all — recovery only applies to multi-revision groups.
func singleRevisionNotice(sessionID string) string {
	return fmt.Sprintf("%s has a single revision — nothing to recover", sessionID)
}

// pickRevision resolves a user-supplied digest prefix against the distinct
// digests of one conflict group. Exactly one prefix match succeeds; zero or
// several matches return an error listing the candidate digests so the user
// can be more specific without re-running the picker.
func pickRevision(prefix string, digests []string) (string, error) {
	prefix = strings.ToLower(prefix)
	var matches []string
	for _, d := range digests {
		if strings.HasPrefix(d, prefix) {
			matches = append(matches, d)
		}
	}
	switch {
	case len(matches) == 1:
		return matches[0], nil
	case len(matches) > 1:
		return "", fmt.Errorf("--revision %q is ambiguous — it matches %d revisions:\n  %s",
			prefix, len(matches), strings.Join(matches, "\n  "))
	default:
		return "", fmt.Errorf("--revision %q matches none of this session's revisions:\n  %s",
			prefix, strings.Join(digests, "\n  "))
	}
}

// archiveSiblings returns the digests of every OTHER revision in the group —
// all of them are re-marked archive-only per clone after the chosen one is
// written back, so their preserved state survives the switch explicitly
// rather than by absence of an outcome record.
func archiveSiblings(chosenDigest string, revisions []conflict.Revision) []string {
	var out []string
	for _, r := range revisions {
		if r.Digest != chosenDigest {
			out = append(out, r.Digest)
		}
	}
	return out
}

// digest12 renders a digest for picker rows and summaries: a fixed 12-char
// prefix plus an ellipsis when truncated (full digests stay in filenames).
func digest12(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12] + "…"
}

// buildWriteBackAdapter wires the OpenCode adapter from config. Package-level
// var so tests can inject a fixture adapter without a real opencode binary
// (AGENTS.md coding conventions).
var buildWriteBackAdapter = func(cfg config.Config) *opencode.Adapter {
	ad := opencode.NewAdapter()
	ad.TrustedPath = cfg.Producer.TrustedPath
	ad.StrictCheck = cfg.Producer.StrictCheck
	return ad
}

// pickRevisionInteractive presents the numbered picker over a conflicted
// group's surviving DEVICE HEADS (superseded mid-chain revisions stay hidden;
// they remain reachable via --revision <digest-prefix>), using the same bufio
// pattern as resume's session picker, and returns the chosen digest. Blank
// input aborts cleanly with ok=false.
func pickRevisionInteractive(gi conflictGroup) (digest string, ok bool, err error) {
	header := fmt.Sprintf("\n%s has %d preserved revision heads:",
		gi.Group.SessionID, len(gi.Group.Heads))
	if gi.Group.Superseded > 0 {
		header += fmt.Sprintf(" (+%d older superseded — pickable via --revision)", gi.Group.Superseded)
	}
	fmt.Println(header)
	for i, rev := range gi.Group.Heads {
		ref, found := primaryRef(gi.Refs, rev.Digest)
		if !found {
			continue // unreachable: group members come from these refs
		}
		fmt.Printf("  %2d) %s  %s  %s\n", i+1,
			digest12(ref.Digest), deviceHumanLabel(ref.Meta), capturedLabel(ref.Meta))
	}
	fmt.Print("\nSelect revision to recover (number, or blank to abort): ")
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return "", false, sc.Err()
	}
	sel := strings.TrimSpace(sc.Text())
	if sel == "" {
		return "", false, nil
	}
	var n int
	if _, perr := fmt.Sscanf(sel, "%d", &n); perr != nil || n < 1 || n > len(gi.Group.Heads) {
		return "", false, fmt.Errorf("invalid selection %q", sel)
	}
	return gi.Group.Heads[n-1].Digest, true, nil
}

// recordArchiveOnlyForPaths explicitly marks every digest×path outcome
// archive-only. Read-before-write keeps repeat recover runs from churning
// timestamps when a sibling is already recorded (same discipline as receive's
// conflict archiving). Sibling marking NEVER touches busy/failed retry state
// of the CHOSEN revision — it only closes its own digests' lifecycle.
func recordArchiveOnlyForPaths(local *receivestate.Store, digest, sessionID string, paths []string) {
	for _, path := range paths {
		previous, ok, err := local.Get(digest, path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "recover: action=read-state digest=%s path=%q status=error error=%q\n", digest, path, err)
			continue
		}
		if ok && previous.Status == receivestate.StatusArchiveOnly {
			continue
		}
		if err := local.Put(receivestate.Outcome{
			ArtifactDigest: digest,
			SessionID:      sessionID,
			CandidatePath:  path,
			Status:         receivestate.StatusArchiveOnly,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "recover: action=record digest=%s path=%q status=error error=%q\n", digest, path, err)
		}
	}
}

// cmdRecover implements agent-sync recover <session-id>.
//
// The chosen revision goes through exactly receive's guarded single-group
// pipeline (candidate validation → ValidateArtifact version pin → broadcast
// write-back with per-clone process guard), then every sibling digest is
// explicitly re-marked archive-only per candidate. Previous outcome records
// do NOT gate the attempt: recover is explicit human intent to (re)store a
// specific revision, including switching away from a verified sibling or
// retrying past a stale backoff (AGENTS.md invariant #8 still applies —
// degraded broadcasts are reported as such by reportBroadcast).
func cmdRecover(args []string) error {
	sessionID := ""
	revPrefix := ""
	revSet := false
	dryRun := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--revision":
			if i+1 >= len(args) {
				return fmt.Errorf("--revision requires a digest prefix")
			}
			revPrefix = args[i+1]
			revSet = true
			i++
		case "--both":
			// Fail closed ALWAYS, before any state inspection: the duplication
			// mechanism that could make --both safe is unproven (stage 5).
			return bothFlagError()
		case "--dry-run":
			dryRun = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown recover flag %q", args[i])
			}
			if sessionID != "" {
				return fmt.Errorf("unexpected argument %q — recover takes exactly one session id", args[i])
			}
			sessionID = args[i]
		}
	}
	if sessionID == "" {
		return fmt.Errorf("recover requires a session id")
	}

	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	repo := syncrepo.Open(cfg.Sync.RepoPath)
	if !repo.Exists() {
		return fmt.Errorf("sync repo not initialized at %s — run `agent-sync init`", cfg.Sync.RepoPath)
	}
	// Same pre-write-back pull safety as receive: resuming decisions against
	// stale sync state would restore a revision another device superseded.
	if cfg.Sync.Remote != "" {
		if err := repo.PullForced(); err != nil {
			return fmt.Errorf("sync-repo pull: %w", err)
		}
	}

	refs, err := findRevisions(repo.Path)
	if err != nil {
		return err
	}
	groups := buildConflictGroups(refs)

	var matched []conflictGroup
	for _, gi := range groups {
		if gi.Group.SessionID == sessionID {
			matched = append(matched, gi)
		}
	}
	if len(matched) == 0 {
		return fmt.Errorf("no stored revisions found for session %q — run `agent-sync revisions list` to see what is stored", sessionID)
	}
	if len(matched) > 1 {
		keys := make([]string, 0, len(matched))
		for _, gi := range matched {
			keys = append(keys, gi.Group.Key)
		}
		return fmt.Errorf("session %q exists under multiple project keys (%s) — refusing to guess which one you mean; inspect with `agent-sync revisions list`",
			sessionID, strings.Join(keys, ", "))
	}
	gi := matched[0]
	key := gi.Group.Key

	digests := make([]string, 0, len(gi.Group.Revisions))
	for _, rev := range gi.Group.Revisions {
		digests = append(digests, rev.Digest)
	}

	// Not conflicted: say so unless --revision explicitly targets the single
	// digest, which permits a manual re-store (e.g. after a botched clone).
	if !gi.Group.Conflicted && !revSet {
		fmt.Println(singleRevisionNotice(sessionID))
		return nil
	}
	var chosen string
	if revSet {
		chosen, err = pickRevision(revPrefix, digests)
		if err != nil {
			return err
		}
	} else {
		picked, ok, perr := pickRevisionInteractive(gi)
		if perr != nil {
			return perr
		}
		if !ok {
			fmt.Println("Aborted.")
			return nil
		}
		chosen = picked
	}

	ref, ok := primaryRef(gi.Refs, chosen)
	if !ok {
		return fmt.Errorf("internal: no ref found for chosen digest %s", chosen) // unreachable
	}
	title := ""
	if ref.Meta != nil {
		title = ref.Meta.Title
	}
	siblings := archiveSiblings(chosen, gi.Group.Revisions)

	idx, err := openRepoIndex()
	if err != nil {
		return err
	}
	candidates, err := idx.Resolve(session.CanonicalKey(key))
	if err != nil {
		return fmt.Errorf("resolve local clones for %s: %w", key, err)
	}
	if len(candidates) == 0 {
		fmt.Printf("Archived only: %s (%s) has no local clone for %s. Run agent-sync index after cloning.\n", sessionID, title, key)
		return nil
	}
	paths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if err := repoindex.ValidateCandidate(session.CanonicalKey(key), candidate.LocalPath); err != nil {
			fmt.Fprintf(os.Stderr, "recover: action=revalidate session=%s path=%q status=skip error=%q\n", sessionID, candidate.LocalPath, err)
			continue
		}
		paths = append(paths, candidate.LocalPath)
	}
	if len(paths) == 0 {
		return fmt.Errorf("no valid local clone of %s remains for %s — refresh the index with `agent-sync index` and retry", key, sessionID)
	}

	ad := buildWriteBackAdapter(cfg)
	s := &session.Session{ID: sessionID, Tool: session.ToolOpenCode, CanonicalKey: session.CanonicalKey(key), PayloadPath: ref.PayloadPath}
	if verr := ad.ValidateArtifact(s); verr != nil {
		return fmt.Errorf("artifact check failed before write-back: %w (nothing was changed)", verr)
	}
	if dryRun {
		fmt.Printf("DRY RUN: would recover %s of %s (%s) into %d validated clone(s):\n", digest12(chosen), sessionID, title, len(paths))
		for _, p := range paths {
			fmt.Printf("  %s\n", p)
		}
		fmt.Printf("DRY RUN: would mark %d sibling revision(s) archive-only per clone%s\n",
			len(siblings), siblingListSuffix(siblings))
		return nil
	}

	statePath, err := receivestate.DefaultPath()
	if err != nil {
		return err
	}
	local, err := receivestate.Open(statePath)
	if err != nil {
		return err
	}

	fmt.Printf("Recovering %s of %s (%s) into %d clone(s)...\n", digest12(chosen), sessionID, title, len(paths))
	res := ad.BroadcastWriteBack(s, paths)
	recordBroadcast(local, chosen, sessionID, res)
	reportBroadcast(res, sessionID, key)

	for _, sibDigest := range siblings {
		recordArchiveOnlyForPaths(local, sibDigest, sessionID, paths)
	}

	fmt.Printf("Recovered %s of %s.\n", digest12(chosen), sessionID)
	if len(res.Imported) > 1 {
		fmt.Printf("WARNING: recovered into %d clones — OpenCode's session↔project model is one-to-one, so the association moved to the LAST-imported clone.\n", len(res.Imported))
	}
	if len(siblings) > 0 {
		fmt.Printf("Sibling revision(s) remain preserved/archive-only:%s — nothing was deleted.\n", siblingListSuffix(siblings))
	}
	fmt.Printf("Re-run `agent-sync recover %s` any time to switch revisions: the active association moves to the newly imported one; earlier copies stay in OpenCode storage.\n", sessionID)
	return nil
}

// siblingListSuffix renders sibling digests for summary/dry-run lines:
// " (a1b2c3d4e5f6…, 9e8d7c6b5a49…)" or "" when there are none.
func siblingListSuffix(siblings []string) string {
	if len(siblings) == 0 {
		return ""
	}
	parts := make([]string, 0, len(siblings))
	for _, d := range siblings {
		parts = append(parts, digest12(d))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}
