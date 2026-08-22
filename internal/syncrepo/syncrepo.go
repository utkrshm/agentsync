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

	"agentsync/internal/deviceid"
)

// ErrNoChanges is returned by Commit when the working tree had nothing new to
// commit (e.g. a mirror batch whose exported files are byte-identical to what
// is already committed). Callers must treat it as a no-op, not a failure, and
// must not push.
var ErrNoChanges = errors.New("no changes to commit")

// ErrForeignUntrackedConflict is returned by PullFastForward (and propagated
// by Push/PullForced) when incoming history would overwrite an untracked file
// outside the sync-owned allowlist. The user placed that file; deleting it as
// a sync side effect is never acceptable. Callers can detect this with
// errors.Is and surface the remediation message without extra wrapping.
var ErrForeignUntrackedConflict = errors.New("untracked files outside the sync allowlist conflict with incoming history")

const (
	// gitignorePath is the exact repo-relative path of the tracked .gitignore.
	gitignorePath = ".gitignore"
	// payloadPrefix is the repo-relative directory under which all mirrored
	// session payloads live. Everything under it is sync-owned.
	payloadPrefix = "opencode/"
)

// allowlisted reports whether rel (slash-separated, repo-relative) is a path
// agent-sync owns: the tracked .gitignore plus everything under opencode/.
func allowlisted(rel string) bool {
	return rel == gitignorePath || strings.HasPrefix(rel, payloadPrefix)
}

// loadDeviceID resolves this device's durable identity. Package-level var so
// tests can inject a fixed value instead of touching the real per-user
// config directory.
var loadDeviceID = deviceid.LoadOrCreate

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

	// Warnf reports recoverable, user-visible issues (e.g. foreign files that
	// Commit deliberately skips). Injectable like every other shell-out point;
	// nil falls back to writing to os.Stderr.
	Warnf func(format string, args ...any)

	// ValidateArtifact, when non-nil, gates every changed file staged under
	// payloadPrefix: returning an error excludes the file from the commit
	// (warned, never deleted). absPath is the working-tree path; relPath is
	// slash-separated and repo-relative. Deletions are exempt — there is no
	// content left to validate, and recording a stale artifact's removal
	// still matters.
	ValidateArtifact func(absPath, relPath string) error
}

// Open returns a Repo for the given working-tree path.
func Open(path string) *Repo {
	return &Repo{Path: path, Warnf: defaultWarnf}
}

// defaultWarnf is the fallback Warnf: unadorned lines on stderr.
func defaultWarnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
}

// warn emits a warning through Warnf when set, stderr otherwise (covers Repos
// constructed as struct literals rather than via Open).
func (r *Repo) warn(format string, args ...any) {
	if r.Warnf != nil {
		r.Warnf(format, args...)
		return
	}
	defaultWarnf(format, args...)
}

// Exists reports whether the working tree is an initialized git repo.
func (r *Repo) Exists() bool {
	_, err := os.Stat(filepath.Join(r.Path, ".git"))
	return err == nil
}

// git runs `git -C r.Path <args...>` and returns trimmed combined output.
func (r *Repo) git(args ...string) (string, error) {
	out, err := r.gitRaw(args...)
	return strings.TrimSpace(out), err
}

// gitRaw runs git like Repo.git but returns the output verbatim. Porcelain
// consumers need this: `git status --porcelain` encodes state in fixed
// columns, and trimming strips the leading space of worktree-only entries
// (" D path"), shifting every parsed field one character left.
func (r *Repo) gitRaw(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", r.Path}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
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
	// The device id comes from the durable per-device identity file
	// (internal/deviceid) so it survives meta corruption across devices.
	deviceID, err := loadDeviceID()
	if err != nil {
		return fmt.Errorf("load device id: %w", err)
	}
	if err := r.WriteMeta(SyncMeta{SchemaVersion: 1, DeviceID: deviceID}); err != nil {
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

// Commit stages only sync-owned paths (the tracked .gitignore and everything
// under opencode/) and creates a timestamped, versioned commit. version is a
// monotonically increasing integer; the message format is:
//
//	sync: <tool> <session-id> v<version> <ISO-timestamp>
//
// Anything else a user dropped into the working tree is never staged — it
// would otherwise be committed and pushed forever as a sync side effect.
// Each foreign modified/untracked path is surfaced as a warning instead, and
// the commit proceeds with whatever was staged. If the working tree has
// nothing new to stage, it returns ErrNoChanges.
func (r *Repo) Commit(tool, sessionID, ts string) (version int, err error) {
	if err := r.stageSyncOwned(); err != nil {
		return 0, err
	}
	r.warnForeignPaths()
	version, err = r.nextVersion()
	if err != nil {
		return 0, err
	}
	msg := fmt.Sprintf("sync: %s %s v%d %s", tool, sessionID, version, ts)
	out, err := r.git("commit", "-m", msg)
	if err != nil {
		// "nothing to commit": staged tree identical to HEAD.
		// "nothing added to commit but untracked files present": only
		// foreign paths changed and nothing sync-owned was staged. Both are
		// no-ops for a sync repo, not failures.
		low := strings.ToLower(out)
		if strings.Contains(low, "nothing to commit") || strings.Contains(low, "nothing added to commit") {
			return version, ErrNoChanges
		}
		return version, err
	}
	return version, nil
}

// stageSyncOwned stages exactly the sync-owned paths git reports as changed,
// filtered through ValidateArtifact when set. Staging explicit paths (rather
// than blanket-adding the opencode/ directory) keeps files that fail
// validation out of the index entirely, so hand-placed or corrupt files
// inside the payload tree never reach the shared history.
func (r *Repo) stageSyncOwned() error {
	paths := r.syncOwnedChanges()
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"add", "-A", "--"}, paths...)
	_, err := r.git(args...)
	return err
}

// syncOwnedChanges lists changed/untracked allowlisted paths from git status
// --porcelain scoped to the allowlist. Under payloadPrefix each file passes
// through ValidateArtifact; failures are warned and excluded — never deleted
// or staged. Deletions pass unvalidated (nothing left to check). On a status
// failure it falls back to the blanket add: a plumbing error must not
// silently skip syncing real artifacts.
func (r *Repo) syncOwnedChanges() []string {
	out, err := r.gitRaw("status", "--porcelain", "--untracked-files=all", "--",
		strings.TrimSuffix(payloadPrefix, "/"), gitignorePath)
	if err != nil {
		r.warn("sync staging: git status failed (%v); staging whole allowlist without per-file validation", err)
		if _, addErr := r.git("add", "-A", "--", strings.TrimSuffix(payloadPrefix, "/"), gitignorePath); addErr != nil {
			r.warn("sync staging: blanket git add failed: %v", addErr)
		}
		return nil
	}
	var add []string
	consider := func(rel string, deletion bool) {
		if rel == "" || !allowlisted(rel) {
			return
		}
		if !deletion && r.ValidateArtifact != nil {
			abs := filepath.Join(r.Path, filepath.FromSlash(rel))
			if verr := r.ValidateArtifact(abs, rel); verr != nil {
				r.warn("skipping invalid sync artifact: %s (%v)", rel, verr)
				return
			}
		}
		add = append(add, rel)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) < 4 {
			continue
		}
		body := line[3:] // XY<space> then path
		if i := strings.Index(body, " -> "); i >= 0 {
			consider(strings.TrimSpace(body[:i]), true)    // old side: content gone
			consider(strings.TrimSpace(body[i+4:]), false) // new side validated
			continue
		}
		consider(body, isDeletion(line))
	}
	return add
}

// isDeletion reports whether a porcelain status line marks a deleted path in
// the index or worktree (X or Y is 'D').
func isDeletion(line string) bool {
	if len(line) < 2 {
		return false
	}
	return line[0] == 'D' || line[1] == 'D'
}

// warnForeignPaths surfaces modified/untracked working-tree paths outside the
// sync-owned allowlist without failing the commit. Ignored files (e.g.
// .sync-meta.json) never appear in porcelain output.
func (r *Repo) warnForeignPaths() {
	out, err := r.gitRaw("status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		rel := porcelainPath(line)
		if rel == "" || allowlisted(rel) {
			continue
		}
		r.warn("skipping non-sync path: %s\n", rel)
	}
}

// porcelainPath extracts the repo-relative path from a raw
// `git status --porcelain` entry (format: XY<space><path>), handling the
// rename form "R  old -> new" by returning the new path.
func porcelainPath(line string) string {
	if len(line) < 4 {
		return ""
	}
	body := line[3:] // skip XY and the separating space
	if i := strings.Index(body, " -> "); i >= 0 {
		return body[i+4:]
	}
	return body
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
// rather than force-pushing (AGENTS.md invariant #9). A foreign-untracked
// conflict is propagated unwrapped: its message already states exactly what
// happened and what to do, and claiming divergence would be misleading.
func (r *Repo) Push() error {
	if err := r.PullFastForward(); err != nil {
		if errors.Is(err, ErrForeignUntrackedConflict) {
			return err
		}
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
// incoming commit would overwrite — but only files agent-sync owns (the
// tracked .gitignore and paths under opencode/). A fresh `agent-sync init`
// leaves a generated, untracked .gitignore behind; when another device has
// committed the same file, a fast-forward merge refuses to overwrite it even
// when the content is identical. These are deterministic generated files
// (git merge has no "overwrite untracked on ff-merge" flag), so removing
// exactly the conflicting, allowlisted ones is safe.
//
// A conflicting untracked file outside the allowlist was placed there by the
// user: deleting it as a sync side effect is never acceptable. If any such
// file exists, nothing foreign is touched and ErrForeignUntrackedConflict is
// returned listing every affected path with remediation instructions.
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
	var foreignConflicts []string
	for _, f := range untracked {
		if !incoming[f] {
			continue
		}
		if !allowlisted(f) {
			foreignConflicts = append(foreignConflicts, f)
			continue
		}
		if err := os.Remove(filepath.Join(r.Path, f)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if len(foreignConflicts) > 0 {
		sort.Strings(foreignConflicts)
		return fmt.Errorf("%w: refusing fast-forward: incoming history would overwrite untracked file(s) you placed here:\n  %s\nmove or remove them manually, then retry",
			ErrForeignUntrackedConflict, strings.Join(foreignConflicts, "\n  "))
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
// A foreign-untracked conflict propagates unwrapped: "stale state" would
// misdescribe it and its own message carries the remediation.
func (r *Repo) PullForced() error {
	if err := r.PullFastForward(); err != nil {
		if errors.Is(err, ErrForeignUntrackedConflict) {
			return err
		}
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

// TouchMeta ensures .sync-meta.json exists and carries this device's durable
// identity. Ids written before internal/deviceid existed (dev-<unixnano>)
// were regenerated whenever the meta file went missing, silently splitting
// one physical device into several identities — so TouchMeta replaces any
// legacy value with the durable id on touch. This also guarantees the
// device-id file itself exists after any command that touches meta.
func (r *Repo) TouchMeta() error {
	m, err := r.ReadMeta()
	if err != nil {
		// Fresh repo / missing meta: seed with defaults.
		m = SyncMeta{SchemaVersion: 1}
	}
	m.SchemaVersion = 1
	deviceID, err := loadDeviceID()
	if err != nil {
		return fmt.Errorf("load device id: %w", err)
	}
	m.DeviceID = deviceID
	return r.WriteMeta(m)
}
