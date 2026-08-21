// OpenCode capture adapter (IMPLEMENTATION-PLAN.md §5, Phase 2).
//
// Change detection over SQLite: OpenCode 1.18.18 stores sessions in
// opencode.db only (Phase 0 findings) — there are no per-session JSONL files
// to tail. The daemon watches the DB files; OnChange then queries the DB via
// opencode's own `opencode db` command (read-only) for sessions whose
// time_updated is newer than the last-mirrored watermark, and exports each
// changed session via `opencode export`. Payloads never touch the DB directly
// — only through opencode's own CLI (AGENTS.md invariant #4).
//
// The watermark is stored per-device in a small state file under the config
// dir (each device's DB is its own; the sync repo is shared, so the watermark
// must not travel with it).
package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agentsync/internal/canonicalkey"
	"agentsync/internal/session"
)

// WatchState is the per-device watermark file shape.
type WatchState struct {
	// Sessions maps a session ID to the time_updated value successfully mirrored
	// for that specific session. A global maximum watermark loses out-of-order
	// updates from another session.
	Sessions map[string]int64 `json:"sessions"`
}

// Adapter implements session.Adapter for OpenCode capture. All shell-out
// points are injectable fields so tests can run against fixtures without a
// real opencode binary (AGENTS.md coding conventions).
type Adapter struct {
	// DataDir resolves the opencode data dir.
	DataDir func() (string, error)
	// Export runs `opencode export <id>` writing JSON to outPath.
	Export func(sessionID, outPath string) error
	// QueryRecent returns the JSON rows from `opencode db` for the given SQL.
	QueryRecent func(sql string) ([]byte, error)
	// Import is retained for fixture compatibility. New write-back uses
	// ImportInto, which receives the target clone directory.
	Import func(exportPath string) error
	// ImportInto runs `opencode import` with targetDir as its working directory.
	ImportInto func(exportPath, targetDir string) error
	// PatchImport applies the project_id/directory fix after import.
	// Injectable for tests (the real one touches the live opencode DB).
	PatchImport func(exportPath, targetDir, projectKey string) error
	// ProcessGuard reports whether opencode is running for a project dir
	// (UID-scoped, best-effort). Injectable for guard tests.
	ProcessGuard func(targetPath string) (bool, error)
	// ToolVersion returns the installed OpenCode version for fail-closed
	// compatibility checks before write-back.
	ToolVersion func() (string, error)
	// VerifyImport confirms the imported session is associated with targetDir.
	VerifyImport func(exportPath, targetDir string) error
	// ShouldCapture applies deny policy after resolving session metadata but
	// before exporting payload content. nil means allow all.
	ShouldCapture func(localPath string, key session.CanonicalKey) bool
	// StateFile is the path to the per-session capture state file.
	StateFile func() (string, error)

	acknowledged map[string]int64
	stateLoaded  bool
}

// CaptureAcknowledger is implemented by capture adapters whose durable source
// cursor must advance only after the sync-repo commit succeeds.
type CaptureAcknowledger interface {
	Acknowledge([]session.Session) error
}

// NewAdapter returns an Adapter wired to the real opencode CLI.
func NewAdapter() *Adapter {
	return &Adapter{
		DataDir:      DataDir,
		Export:       Export,
		QueryRecent:  dbQuery,
		Import:       Import,
		ImportInto:   ImportInto,
		PatchImport:  PatchImport,
		ProcessGuard: IsToolRunning,
		ToolVersion:  ToolVersion,
		VerifyImport: VerifyImport,
		StateFile:    stateFilePath,
	}
}

// changedSession is a row from the session table.
type changedSession struct {
	ID          string `json:"id"`
	TimeUpdated int64  `json:"time_updated"`
	Directory   string `json:"directory"`
}

// Name implements session.Adapter.
func (a *Adapter) Name() session.ToolKind { return session.ToolOpenCode }

// WatchPaths implements session.Adapter: the DB file plus its WAL/SHM
// siblings (all are written during a session).
func (a *Adapter) WatchPaths() ([]string, error) {
	dir, err := a.DataDir()
	if err != nil {
		return nil, err
	}
	db := filepath.Join(dir, "opencode.db")
	return []string{db, db + "-wal", db + "-shm"}, nil
}

// OnChange implements session.Adapter: find sessions updated since the
// watermark and export them. Each produced Session carries a temp-file payload
// that Mirror copies into the sync repo layout.
func (a *Adapter) OnChange(ev session.WatchEvent) ([]session.Session, error) {
	if err := a.loadState(); err != nil {
		return nil, err
	}
	// OpenCode uses one database for all projects. We query only session
	// metadata, evaluate policy, and only then export an allowed payload.
	// Per-session acknowledgement avoids losing an older update after another
	// session advanced a global timestamp watermark.
	rows, err := a.QueryRecent(`SELECT id, time_updated, directory FROM session ORDER BY time_updated ASC`)
	if err != nil {
		return nil, err
	}
	var changed []changedSession
	if err := json.Unmarshal(rows, &changed); err != nil {
		return nil, fmt.Errorf("parse db query result: %w", err)
	}

	var out []session.Session
	for _, c := range changed {
		if c.ID == "" || c.TimeUpdated <= a.acknowledged[c.ID] {
			continue
		}
		key := canonicalkey.Resolve(c.Directory)
		if a.ShouldCapture != nil && !a.ShouldCapture(c.Directory, key) {
			continue
		}
		s, err := a.exportOne(c, key)
		if err != nil {
			// A single malformed session must not stop the sweep. It remains
			// unacknowledged and will be retried by the next reconciliation.
			fmt.Fprintf(os.Stderr, "opencode: export %s: %v\n", c.ID, err)
			continue
		}
		out = append(out, *s)
	}
	return out, nil
}

// exportOne exports a single session to a temp file and builds the Session.
// The temp file is intentionally left in place: Mirror copies it into the sync
// repo layout, after which the caller may remove s.PayloadPath.
func (a *Adapter) exportOne(c changedSession, key session.CanonicalKey) (*session.Session, error) {
	tmp, err := os.CreateTemp("", "agentsync-export-*.json")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()

	if err := a.Export(c.ID, tmpPath); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	info, err := readExportInfo(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	if info.Directory == "" {
		return nil, fmt.Errorf("exported session %s has no directory", c.ID)
	}
	if key == "" {
		key = canonicalkey.Resolve(info.Directory)
	}
	return &session.Session{
		ID:           c.ID,
		Tool:         session.ToolOpenCode,
		CanonicalKey: key,
		LocalPath:    info.Directory,
		LastModified: time.UnixMilli(c.TimeUpdated),
		PayloadPath:  tmpPath, // temp file; Mirror copies it into the repo
	}, nil
}

// Mirror implements session.Adapter: copy the export into the sync repo
// layout and write the receive-side import-meta. Capture acknowledgement is a
// separate operation because the daemon must not advance its cursor until the
// sync-repo commit succeeds.
func (a *Adapter) Mirror(s *session.Session, repoRoot string) error {
	if err := a.loadState(); err != nil {
		return err
	}
	rel := filepath.Join("opencode", string(s.CanonicalKey), "export", s.ID+".json")
	dest := filepath.Join(repoRoot, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	if err := copyFile(s.PayloadPath, dest); err != nil {
		return err
	}
	s.PayloadPath = filepath.Join(repoRoot, rel)

	// Write import-meta so `receive` can apply the project_id/directory patch.
	info, err := readExportInfo(dest)
	if err != nil {
		return err
	}
	metaPath := filepath.Join(repoRoot, "opencode", string(s.CanonicalKey), "import-meta", s.ID+".json")
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o700); err != nil {
		return err
	}
	if err := writeImportMeta(metaPath, info); err != nil {
		return err
	}

	return nil
}

// Acknowledge advances the per-session capture cursor after the caller has
// durably committed the mirrored artifacts to the sync repository.
func (a *Adapter) Acknowledge(sessions []session.Session) error {
	if err := a.loadState(); err != nil {
		return err
	}
	if a.acknowledged == nil {
		a.acknowledged = map[string]int64{}
	}
	for _, s := range sessions {
		stamp := s.LastModified.UnixMilli()
		if stamp > a.acknowledged[s.ID] {
			a.acknowledged[s.ID] = stamp
		}
	}
	return a.saveState()
}

// loadState reads per-session acknowledgements from the state file once.
func (a *Adapter) loadState() error {
	if a.stateLoaded {
		return nil
	}
	p, err := a.StateFile()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			a.acknowledged = map[string]int64{}
			a.stateLoaded = true
			return nil
		}
		return err
	}
	var st WatchState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("parse watch state %s: %w", p, err)
	}
	if st.Sessions == nil {
		st.Sessions = map[string]int64{}
	}
	a.acknowledged = st.Sessions
	a.stateLoaded = true
	return nil
}

// saveState persists per-session acknowledgements.
func (a *Adapter) saveState() error {
	p, err := a.StateFile()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if a.acknowledged == nil {
		a.acknowledged = map[string]int64{}
	}
	data, err := json.MarshalIndent(WatchState{Sessions: a.acknowledged}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

// stateFilePath is the default per-device watermark location.
func stateFilePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent-sync", "opencode-watch.json"), nil
}

// dbQuery runs a read-only SQL query through opencode's own `db` command and
// returns its JSON output. No direct DB file access (invariant #4).
func dbQuery(sql string) ([]byte, error) {
	cmd := exec.Command(binName, "db", sql, "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		var stderr strings.Builder
		if ee, ok := err.(*exec.ExitError); ok {
			stderr.Write(ee.Stderr)
		}
		return nil, fmt.Errorf("opencode db: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
