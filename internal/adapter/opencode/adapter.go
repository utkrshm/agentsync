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
	// LastMirroredTime is the maximum time_updated (millis, Unix epoch) of any
	// session already mirrored. Sessions with time_updated <= this are skipped.
	LastMirroredTime int64 `json:"last_mirrored_time"`
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
	// StateFile is the path to the watermark state file.
	StateFile func() (string, error)

	lastMirrored int64 // cached watermark after the first read
}

// NewAdapter returns an Adapter wired to the real opencode CLI.
func NewAdapter() *Adapter {
	return &Adapter{
		DataDir:     DataDir,
		Export:      Export,
		QueryRecent: dbQuery,
		StateFile:   stateFilePath,
	}
}

// changedSession is a row from the session table.
type changedSession struct {
	ID          string `json:"id"`
	TimeUpdated int64  `json:"time_updated"`
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
	// Ignore events for paths we don't care about (e.g. a stray file in the
	// data dir); OnChange is triggered for the DB and its WAL/SHM siblings.
	rows, err := a.QueryRecent(fmt.Sprintf(
		`SELECT id, time_updated FROM session WHERE time_updated > %d ORDER BY time_updated ASC`,
		a.lastMirrored))
	if err != nil {
		return nil, err
	}
	var changed []changedSession
	if err := json.Unmarshal(rows, &changed); err != nil {
		return nil, fmt.Errorf("parse db query result: %w", err)
	}

	var out []session.Session
	for _, c := range changed {
		if c.ID == "" {
			continue
		}
		// Defensive filter in addition to the SQL predicate: honor the
		// watermark even if the query result is broader than expected
		// (unknown/renamed columns in a future opencode release).
		if c.TimeUpdated <= a.lastMirrored {
			continue
		}
		s, err := a.exportOne(c)
		if err != nil {
			// A single malformed session must not stop the sweep.
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
func (a *Adapter) exportOne(c changedSession) (*session.Session, error) {
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
	key := canonicalkey.Resolve(info.Directory)
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
// layout and write the receive-side import-meta. Also advances the per-device
// watermark so OnChange won't re-export this session.
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

	// Advance the watermark: max of the last mirror time and this session's.
	if t := s.LastModified.UnixMilli(); t > a.lastMirrored {
		a.lastMirrored = t
	}
	return a.saveState()
}

// loadState reads the watermark from the state file once.
func (a *Adapter) loadState() error {
	if a.lastMirrored != 0 {
		return nil
	}
	p, err := a.StateFile()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // first run — no watermark, export everything
		}
		return err
	}
	var st WatchState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("parse watch state %s: %w", p, err)
	}
	a.lastMirrored = st.LastMirroredTime
	return nil
}

// saveState persists the watermark.
func (a *Adapter) saveState() error {
	p, err := a.StateFile()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(WatchState{LastMirroredTime: a.lastMirrored}, "", "  ")
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