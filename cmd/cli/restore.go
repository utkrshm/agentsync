package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/receivestate"
	"agentsync/internal/repoindex"
	"agentsync/internal/session"
	"agentsync/internal/syncrepo"
)

// restoreRevision is THE guarded write-back pipeline for one chosen revision
// of a conflict group (docs/sync-rekey-collapse-plan.md Step 4, "Guarded
// restore (shared refactor)"): recover and sync compose THIS function so
// their safety behavior can never drift apart.
//
// Pipeline, in order:
//  1. targets: callers usually pass explicit clone paths; with none supplied
//     the repo-index cache resolves them. Every target is re-validated right
//     here — the index/cache is a performance aid, never write-back authority.
//  2. artifact: the chosen digest's most informative ref is located, bound to
//     a session, and checked against the installed tool version plus
//     provenance pins BEFORE anything mutates (nothing is changed on failure).
//  3. write-back: BroadcastWriteBack applies the UID-scoped process guard per
//     target (busy targets are simply recorded and retried later — AGENTS.md
//     invariant #2) and imports with verification and the project patch.
//  4. bookkeeping: outcomes land per digest×path (busy/failed schedule retry;
//     imported verifies with honest degraded multi-clone reporting), and every
//     sibling DEVICE HEAD is re-marked archive-only with read-before-write
//     discipline so repeat runs do not churn timestamps.
//
// restored reports whether at least one target imported successfully. A
// partial run is NOT an error: reportBroadcast surfaces busy/failed rows and
// callers decide how loudly to summarize.
func restoreRevision(repo *syncrepo.Repo, idx *repoindex.DB,
	st *receivestate.Store, ad *opencode.Adapter,
	gi conflictGroup, digest string, targets []string) (bool, error) {

	key := gi.Group.Key
	sessionID := gi.Group.SessionID

	paths := append([]string(nil), targets...)
	if len(paths) == 0 {
		candidates, err := idx.Resolve(session.CanonicalKey(key))
		if err != nil {
			return false, fmt.Errorf("resolve local clones for %s: %w", key, err)
		}
		for _, cand := range candidates {
			paths = append(paths, cand.LocalPath)
		}
	}

	var valid []string
	for _, p := range paths {
		if verr := repoindex.ValidateCandidate(session.CanonicalKey(key), p); verr != nil {
			fmt.Fprintf(os.Stderr, "restore: action=revalidate session=%s path=%q status=skip error=%q\n",
				sessionID, p, verr)
			continue
		}
		valid = append(valid, p)
	}
	if len(valid) == 0 {
		return false, fmt.Errorf("no valid local clone of %s remains for %s — refresh the index with `agent-sync index` and retry",
			key, sessionID)
	}

	ref, ok := primaryRef(gi.Refs, digest)
	if !ok {
		return false, fmt.Errorf("internal: no ref found for digest %s", digest) // unreachable: group members derive from these refs
	}
	repoRoot, err := filepath.Abs(repo.Path)
	if err != nil {
		return false, err
	}
	payloadAbs, err := filepath.Abs(ref.PayloadPath)
	if err != nil {
		return false, err
	}
	if payload, rerr := filepath.Rel(repoRoot, payloadAbs); rerr != nil ||
		strings.HasPrefix(payload, ".."+string(filepath.Separator)) || payload == ".." {
		return false, fmt.Errorf("payload %s lives outside the sync repo %s — refusing write-back", payloadAbs, repoRoot)
	}

	s := &session.Session{
		ID:           sessionID,
		Tool:         session.ToolOpenCode,
		CanonicalKey: session.CanonicalKey(key),
		PayloadPath:  ref.PayloadPath,
	}
	if verr := ad.ValidateArtifact(s); verr != nil {
		return false, fmt.Errorf("artifact check failed before write-back: %w (nothing was changed)", verr)
	}

	res := ad.BroadcastWriteBack(s, valid)
	recordBroadcast(st, digest, sessionID, res)
	reportBroadcast(res, sessionID, key)

	// Sibling DEVICE HEADS stay explicitly preserved/archive-only per clone;
	// superseded mid-chain revisions are covered by their bucket's head
	// (DetectV2 collapse, docs/sync-rekey-collapse-plan.md Step 2).
	for _, h := range gi.Group.Heads {
		if h.Digest == digest {
			continue
		}
		recordArchiveOnlyForPaths(st, h.Digest, sessionID, valid)
	}
	return len(res.Imported) > 0, nil
}

// importedTargetCounts reports how many distinct clone paths recorded a
// success-shaped outcome (verified or degraded) for one digest. Recover uses
// this after restoreRevision to render its one-to-one degradation warning
// without re-running a broadcast.
func importedTargetCounts(local *receivestate.Store, digest string) int {
	outcomes, err := local.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: action=list-outcomes digest=%s status=error error=%q\n", digest, err)
		return 0
	}
	n := 0
	for _, o := range outcomes {
		if o.ArtifactDigest == digest &&
			(o.Status == receivestate.StatusVerified || o.Status == receivestate.StatusDegraded) {
			n++
		}
	}
	return n
}
