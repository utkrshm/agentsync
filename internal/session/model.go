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

// WatchEvent is what the watcher hands to an adapter's OnChange after a
// debounced change on one of the adapter's WatchPaths. It identifies the path
// that changed so the adapter can decide whether it is one it cares about.
type WatchEvent struct {
	Path string // absolute path of the changed file/directory
}

// Adapter is the interface every tool adapter implements (IMPLEMENTATION-PLAN.md
// §1). The daemon, watch core, and syncrepo are written against this interface,
// never against a specific tool.
//
// Write-back is optional — not all adapters implement it. Detect support with a
// type assertion to WriteBacker.
type Adapter interface {
	Name() ToolKind
	WatchPaths() ([]string, error)              // paths to attach fsnotify watches to
	OnChange(event WatchEvent) ([]Session, error) // produce Sessions for a debounced change
	Mirror(s *Session, repoRoot string) error   // write into sync repo layout
}

// WriteBacker is implemented by adapters that support write-back (OpenCode —
// Phase 3, Codex CLI — Phase 6). Claude Code does not in v1.
type WriteBacker interface {
	WriteBack(s *Session, targetLocalPath string) error
	IsToolRunning(targetLocalPath string) (bool, error) // safety guard —
	// MUST filter by owning UID == current process UID, not just process
	// name/path match. The tool may run on a shared multi-user machine;
	// an unscoped scan can false-positive on another user's process.
}
