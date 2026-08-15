// OpenCode write-back adapter (IMPLEMENTATION-PLAN.md §5, Phase 3).
//
// Write-back makes a synced session resumable on this device: it imports the
// mirrored export into OpenCode and patches the project_id/directory columns
// so the session lands on the correct local project (Phase 0 findings §4–5).
//
// Safety per AGENTS.md invariants #2 and #8:
//   - Every candidate local path is guarded independently with a UID-scoped,
//     best-effort process check (never a lock) before import.
//   - Because OpenCode's session↔project model is one-to-one and the session
//     ID is preserved on import, broadcasting across multiple clones of the
//     same repo "moves" the association to the last-imported clone rather
//     than duplicating it. That degraded outcome must be reported as exactly
//     what it is, not as full success.
package opencode

import (
	"fmt"

	"agentsync/internal/session"
)

// Compile-time assertion: the capture Adapter also implements WriteBacker.
var _ session.WriteBacker = (*Adapter)(nil)

// WriteBack implements session.WriteBacker: import the mirrored export into
// OpenCode targeting the given local project directory, then patch the
// project association. The caller has already guard-checked this candidate.
func (a *Adapter) WriteBack(s *session.Session, targetLocalPath string) error {
	if s.PayloadPath == "" {
		return fmt.Errorf("session %s has no mirrored export to write back", s.ID)
	}
	if err := a.Import(s.PayloadPath); err != nil {
		return err
	}
	if a.PatchImport != nil {
		return a.PatchImport(s.PayloadPath, targetLocalPath, string(s.CanonicalKey))
	}
	return nil
}

// IsToolRunning implements session.WriteBacker (per-candidate guard).
func (a *Adapter) IsToolRunning(targetLocalPath string) (bool, error) {
	return a.ProcessGuard(targetLocalPath)
}

// BroadcastResult summarizes a multi-candidate write-back attempt (SPEC-DOC.md
// §4.1, invariant #8).
type BroadcastResult struct {
	// Candidates is every local path resolved for the session's canonical key.
	Candidates []string
	// Imported are the candidates the session was successfully imported into.
	Imported []string
	// Busy are the candidates skipped because opencode was running there.
	Busy []string
	// Degraded is true when the import succeeded into multiple candidates but
	// OpenCode's one-to-one model means the session actually only resides in
	// the last-imported clone.
	Degraded bool
}

// BroadcastWriteBack attempts write-back independently against every candidate
// local path. A busy candidate is skipped without failing the others. It
// returns a BroadcastResult so the caller can log the outcome (including the
// degraded case) rather than just success/failure.
func (a *Adapter) BroadcastWriteBack(s *session.Session, candidates []string) BroadcastResult {
	var res BroadcastResult
	res.Candidates = candidates
	for _, cand := range candidates {
		running, err := a.ProcessGuard(cand)
		if err != nil {
			fmt.Printf("writeback: guard check failed for %s (%s): %v (skipping)\n", cand, s.ID, err)
			continue
		}
		if running {
			fmt.Printf("writeback: opencode running in %s — skipping %s (retry on next pull)\n", cand, s.ID)
			res.Busy = append(res.Busy, cand)
			continue
		}
		if err := a.WriteBack(s, cand); err != nil {
			fmt.Printf("writeback: import into %s failed for %s: %v (skipping)\n", cand, s.ID, err)
			continue
		}
		res.Imported = append(res.Imported, cand)
	}
	// Degraded outcome: >1 candidate import attempted and more than one
	// succeeded, but the underlying model is one-to-one → the session only
	// truly lives in the last-imported clone.
	res.Degraded = len(res.Imported) > 1
	return res
}
