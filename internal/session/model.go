package session

import "time"

// ToolKind identifies which agent tool a session came from. v0.1 only
// implements OpenCode; the type exists so the layout and model stay stable
// for future adapters (IMPLEMENTATION-PLAN.md §1).
type ToolKind string

const (
	ToolOpenCode ToolKind = "opencode"
)

// CanonicalKey is the stable identity of a project, resolved per
// SPEC-DOC.md §4 (git remote URL → first-commit hash → alias → _unmapped).
type CanonicalKey string

// Session is the normalized model an adapter produces. The tool-specific
// payload is kept opaque (AGENTS.md invariant #4) — stored as a path to the
// mirrored file in the sync repo, not parsed into fields.
type Session struct {
	ID           string
	Tool         ToolKind
	CanonicalKey CanonicalKey
	LocalPath    string // absolute path this session lives at, on THIS device
	LastModified time.Time
	CommitHash   string // git HEAD of the local repo at mirror time (best-effort)
	PayloadPath  string // path to the mirrored file(s) in the sync repo
}
