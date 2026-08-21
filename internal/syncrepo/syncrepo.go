package syncrepo

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrNoChanges is returned by Commit when the working tree had nothing new to
// commit (e.g. a mirror batch whose exported files are byte-identical to what
// is already committed). Callers must treat it as a no-op, not a failure, and
// must not push.
var ErrNoChanges = errors.New("no changes to commit")

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
	// .sync-meta.json is device-local state (device id, offsets, caches); it
	// must never be committed to the shared sync repo. Without the gitignore,
	// a second device's `pull` would refuse to merge over its own untracked
	// copy, and one device's meta would clobber another's (docs/CRITIQUE.md
	// issue 3). The .gitignore itself is tracked and identical everywhere, so
	// a fast-forward pull over a matching untracked copy succeeds.
	if err := os.WriteFile(filepath.Join(r.Path, ".gitignore"), []byte(".sync-meta.json\n"), 0o600); err != nil {
		return err
	}
	// Ensure .sync-meta.json exists so it's read/written from the start.
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
//
// If the working tree has nothing new to stage, it returns ErrNoChanges.
func (r *Repo) Commit(tool, sessionID, ts string) (version int, err error) {
	if _, err := r.git("add", "-A"); err != nil {
		return 0, err
	}
	version, err = r.nextVersion()
	if err != nil {
		return 0, err
	}
	msg := fmt.Sprintf("sync: %s %s v%d %s", tool, sessionID, version, ts)
	out, err := r.git("commit", "-m", msg)
	if err != nil {
		if strings.Contains(strings.ToLower(out), "nothing to commit") {
			return version, ErrNoChanges
		}
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
	tip, ok, err := r.remoteTip()
	if err != nil {
		return err
	}
	if !ok {
		return nil // remote has no branches yet — nothing to pull
	}
	// If the remote tip is already an ancestor of local HEAD, we're up to
	// date or ahead → no merge needed (a `--ff-only` merge would wrongly
	// error on "not possible to fast-forward" when local is merely ahead).
	if _, err := r.git("merge-base", "--is-ancestor", tip, "HEAD"); err == nil {
		return nil
	}
	// Remote is ahead of local → fast-forward. If it can't, the remote has
	// diverged from our history → fail loudly, never force-push/auto-merge
	// (AGENTS.md invariant #9).
	if err := r.clearConflictingUntracked(tip); err != nil {
		return err
	}
	if _, err := r.git("merge", "--ff-only", tip); err != nil {
		return fmt.Errorf("fast-forward merge failed (remote likely diverged): %w", err)
	}
	return nil
}

// clearConflictingUntracked removes untracked working-tree files that the
// incoming commit would overwrite. A fresh `agent-sync init` leaves a
// generated, untracked .gitignore behind; when another device has committed
// the same file, a fast-forward merge refuses to overwrite it even when the
// content is identical. These are deterministic generated files (git merge has
// no "overwrite untracked on ff-merge" flag), so removing exactly the
// conflicting ones is safe — no unrelated local file is touched.
func (r *Repo) clearConflictingUntracked(tip string) error {
	out, err := r.git("ls-files", "--others", "--exclude-standard")
	if err != nil {
		return err
	}
	var untracked []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			untracked = append(untracked, line)
		}
	}
	if len(untracked) == 0 {
		return nil
	}
	tree, err := r.git("ls-tree", "-r", "--name-only", tip)
	if err != nil {
		return err
	}
	incoming := map[string]bool{}
	for _, line := range strings.Split(tree, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			incoming[line] = true
		}
	}
	for _, f := range untracked {
		if !incoming[f] {
			continue
		}
		if err := os.Remove(filepath.Join(r.Path, f)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// remoteTip resolves the remote-tracking branch to fast-forward to. It prefers
// origin/HEAD when the remote advertises one (clone-created remotes); a remote
// that was pushed into (e.g. after `agent-sync init` + `push HEAD`) has
// branches but no HEAD symref, so it falls back to an existing remote-tracking
// branch, preferring main/master. Returns ok=false when the remote has no
// branches at all.
func (r *Repo) remoteTip() (string, bool, error) {
	if _, err := r.git("rev-parse", "--verify", "origin/HEAD"); err == nil {
		return "origin/HEAD", true, nil
	}
	refs, err := r.git("for-each-ref", "--format=%(refname:short)", "refs/remotes/origin")
	if err != nil {
		return "", false, err
	}
	var branches []string
	for _, line := range strings.Split(refs, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "origin/HEAD" {
			continue
		}
		branches = append(branches, line)
	}
	if len(branches) == 0 {
		return "", false, nil
	}
	for _, pref := range []string{"origin/main", "origin/master"} {
		for _, b := range branches {
			if b == pref {
				return b, true, nil
			}
		}
	}
	sort.Strings(branches)
	return branches[0], true, nil
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
