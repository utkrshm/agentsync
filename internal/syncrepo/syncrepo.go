package syncrepo

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SyncMeta is the per-device metadata stored in the sync repo's working tree
// (.sync-meta.json). v0.1 keeps it minimal: schema version, device id, and
// the set of session IDs this device has already imported (so `receive` only
// imports new sessions).
type SyncMeta struct {
	SchemaVersion int    `json:"schema_version"`
	DeviceID      string `json:"device_id"`
}

// Repo wraps a sync repo working tree and provides git operations that
// always shell out to the system git binary (AGENTS.md tech stack).
type Repo struct {
	Path string
}

// Open returns a Repo for the given working-tree path.
func Open(path string) *Repo { return &Repo{Path: path} }

// Exists reports whether the working tree is an initialized git repo.
func (r *Repo) Exists() bool {
	_, err := os.Stat(filepath.Join(r.Path, ".git"))
	return err == nil
}

// git runs `git -C r.Path <args...>` and returns combined output.
func (r *Repo) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", r.Path}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Init creates the working tree directory, initializes a git repo, and sets
// the default branch to main.
func (r *Repo) Init() error {
	if err := os.MkdirAll(r.Path, 0o700); err != nil {
		return err
	}
	if _, err := r.git("init", "-b", "main"); err != nil {
		return err
	}
	// Ensure .sync-meta.json exists so it's tracked from the start.
	if err := r.WriteMeta(SyncMeta{SchemaVersion: 1, DeviceID: newDeviceID()}); err != nil {
		return err
	}
	return nil
}

// SetRemote configures (adds or updates) the origin remote URL.
func (r *Repo) SetRemote(url string) error {
	if _, err := r.git("remote", "add", "origin", url); err != nil {
		// Already exists → update instead.
		if _, err2 := r.git("remote", "set-url", "origin", url); err2 != nil {
			return err
		}
	}
	return nil
}

// HasRemote reports whether an origin remote is configured.
func (r *Repo) HasRemote() bool {
	_, err := r.git("remote", "get-url", "origin")
	return err == nil
}

// Commit stages all changes and creates a timestamped, versioned commit.
// version is a monotonically increasing integer; the message format is:
//
//	sync: <tool> <session-id> v<version> <ISO-timestamp>
func (r *Repo) Commit(tool, sessionID, ts string) (version int, err error) {
	if _, err := r.git("add", "-A"); err != nil {
		return 0, err
	}
	version, err = r.nextVersion()
	if err != nil {
		return 0, err
	}
	msg := fmt.Sprintf("sync: %s %s v%d %s", tool, sessionID, version, ts)
	if _, err := r.git("commit", "-m", msg); err != nil {
		return version, err
	}
	return version, nil
}

// nextVersion derives the commit version from the existing commit count.
func (r *Repo) nextVersion() (int, error) {
	out, err := r.git("rev-list", "--count", "HEAD")
	if err != nil {
		// No commits yet → this is version 1.
		return 1, nil
	}
	var n int
	if _, err := fmt.Sscanf(out, "%d", &n); err != nil {
		return 1, nil
	}
	return n + 1, nil
}

// Push pulls fast-forward first (trigger 1, SPEC-DOC.md §5.2) then pushes.
// If the remote has diverged, the ff-merge fails and we return the error
// rather than force-pushing (AGENTS.md invariant #9).
func (r *Repo) Push() error {
	if err := r.PullFastForward(); err != nil {
		return fmt.Errorf("pre-push fast-forward merge failed (remote likely diverged — refusing to force-push): %w", err)
	}
	if _, err := r.git("push", "origin", "HEAD"); err != nil {
		return err
	}
	return nil
}

// PullFastForward fetches and merges --ff-only. Returns an error if the
// remote has diverged (single-writer assumption violated). It is a no-op when
// the local branch is already up to date or ahead of the remote.
func (r *Repo) PullFastForward() error {
	if _, err := r.git("fetch", "origin"); err != nil {
		return err
	}
	// No remote HEAD (fresh/empty remote) → nothing to pull.
	if _, err := r.git("rev-parse", "--verify", "origin/HEAD"); err != nil {
		return nil
	}
	// If the remote tip is already an ancestor of local HEAD, we're up to
	// date or ahead → no merge needed (a `--ff-only` merge would wrongly
	// error on "not possible to fast-forward" when local is merely ahead).
	if _, err := r.git("merge-base", "--is-ancestor", "origin/HEAD", "HEAD"); err == nil {
		return nil
	}
	// Remote is ahead of local → fast-forward. If it can't, the remote has
	// diverged from our history → fail loudly, never force-push/auto-merge
	// (AGENTS.md invariant #9).
	if _, err := r.git("merge", "--ff-only", "origin/HEAD"); err != nil {
		return fmt.Errorf("fast-forward merge failed (remote likely diverged): %w", err)
	}
	return nil
}

// PullForced is the synchronous pre-write-back pull (SPEC-DOC.md §5.2,
// trigger 4). Unlike the periodic/deferred pull paths it blocks and returns
// an error to the caller — resuming against stale state is unsafe, so this
// must not be silently deferred. It is a no-op when the remote is empty or
// local is already up to date/ahead (same semantics as PullFastForward).
func (r *Repo) PullForced() error {
	if err := r.PullFastForward(); err != nil {
		return fmt.Errorf("pre-write-back pull failed (refusing to write against stale state): %w", err)
	}
	return nil
}

// ReadMeta loads .sync-meta.json from the working tree.
func (r *Repo) ReadMeta() (SyncMeta, error) {
	var m SyncMeta
	p := filepath.Join(r.Path, ".sync-meta.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}

// WriteMeta persists .sync-meta.json in the working tree.
func (r *Repo) WriteMeta(m SyncMeta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.Path, ".sync-meta.json"), data, 0o600)
}

// TouchMeta updates the device's last-sync timestamp inside .sync-meta.json.
func (r *Repo) TouchMeta() error {
	m, err := r.ReadMeta()
	if err != nil {
		// Fresh repo / missing meta: seed with defaults.
		m = SyncMeta{SchemaVersion: 1, DeviceID: newDeviceID()}
	}
	m.SchemaVersion = 1
	if m.DeviceID == "" {
		m.DeviceID = newDeviceID()
	}
	return r.WriteMeta(m)
}

func newDeviceID() string {
	return fmt.Sprintf("dev-%d", time.Now().UnixNano())
}
