// OpenCode write-back adapter (IMPLEMENTATION-PLAN.md §5, Phase 3).
package opencode

import (
	"fmt"

	"agentsync/internal/session"
)

// Compile-time assertion: the capture Adapter also implements WriteBacker.
var _ session.WriteBacker = (*Adapter)(nil)

// ValidateArtifact checks the export version against the installed tool without
// modifying OpenCode state. It is used by both dry-run and write-back.
func (a *Adapter) ValidateArtifact(s *session.Session) error {
	if s.PayloadPath == "" {
		return fmt.Errorf("session %s has no mirrored export to write back", s.ID)
	}
	info, err := readExportInfo(s.PayloadPath)
	if err != nil {
		return err
	}
	if info.Version == "" {
		return fmt.Errorf("session %s export has no OpenCode version; refusing undocumented write-back", s.ID)
	}
	if a.ToolVersion == nil {
		return fmt.Errorf("installed OpenCode version check is not configured")
	}
	installed, err := a.ToolVersion()
	if err != nil {
		return fmt.Errorf("read installed OpenCode version: %w", err)
	}
	if normalizeVersion(installed) != normalizeVersion(info.Version) {
		return fmt.Errorf("OpenCode version mismatch: export is %s, installed is %s; refusing write-back", info.Version, installed)
	}
	return nil
}

// WriteBack imports the artifact from the resolved target directory, applies
// the association patch, and verifies that the session is associated with the
// target. The caller has already applied the current-user process guard.
func (a *Adapter) WriteBack(s *session.Session, targetLocalPath string) error {
	if err := a.ValidateArtifact(s); err != nil {
		return err
	}
	var err error
	if a.ImportInto != nil {
		err = a.ImportInto(s.PayloadPath, targetLocalPath)
	} else if a.Import != nil {
		// Compatibility for old injected fixtures. Production adapters always
		// provide ImportInto.
		err = a.Import(s.PayloadPath)
	} else {
		err = fmt.Errorf("OpenCode import is not configured")
	}
	if err != nil {
		return err
	}
	if a.PatchImport != nil {
		if err := a.PatchImport(s.PayloadPath, targetLocalPath, string(s.CanonicalKey)); err != nil {
			return err
		}
	}
	if a.VerifyImport != nil {
		if err := a.VerifyImport(s.PayloadPath, targetLocalPath); err != nil {
			return err
		}
	}
	return nil
}

// IsToolRunning implements session.WriteBacker (per-candidate guard).
func (a *Adapter) IsToolRunning(targetLocalPath string) (bool, error) {
	return a.ProcessGuard(targetLocalPath)
}

// CandidateFailure preserves a retryable failure per clone instead of
// collapsing a partial broadcast into a global session failure.
type CandidateFailure struct {
	Path  string
	Error string
}

// BroadcastResult summarizes a multi-candidate write-back attempt.
type BroadcastResult struct {
	Candidates []string
	Imported   []string
	Busy       []string
	Failed     []CandidateFailure
	Degraded   bool
}

// BroadcastWriteBack attempts every candidate independently. A busy or failed
// candidate stays pending in device-local receive state and is retried later.
func (a *Adapter) BroadcastWriteBack(s *session.Session, candidates []string) BroadcastResult {
	res := BroadcastResult{Candidates: append([]string(nil), candidates...)}
	for _, cand := range candidates {
		running, err := a.ProcessGuard(cand)
		if err != nil {
			res.Failed = append(res.Failed, CandidateFailure{Path: cand, Error: fmt.Sprintf("guard check: %v", err)})
			continue
		}
		if running {
			res.Busy = append(res.Busy, cand)
			continue
		}
		if err := a.WriteBack(s, cand); err != nil {
			res.Failed = append(res.Failed, CandidateFailure{Path: cand, Error: err.Error()})
			continue
		}
		res.Imported = append(res.Imported, cand)
	}
	// OpenCode preserves the session ID but has a one-to-one association, so
	// multiple successful imports move the association to the last clone.
	res.Degraded = len(res.Imported) > 1
	return res
}
