// Package opencode adapts the OpenCode CLI (export/import) for AgentSync.
// v0.1 supports capture (send) and write-back (receive), OpenCode only.
//
// Per AGENTS.md invariant #4 and SPEC-DOC.md §3.2, we interface with OpenCode
// through its own `export`/`import` commands rather than reading internal
// storage, except for a minimal, well-scoped SQLite patch that fixes the
// project_id/directory columns after import (see Phase 0 findings).
package opencode

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// binName is the OpenCode CLI binary name on PATH.
const binName = "opencode"

var (
	binPathOnce sync.Once
	binPathVal  string
	binPathErr  error
)

// BinaryPath resolves the absolute path of the opencode executable via
// exec.LookPath and caches the result for the lifetime of the process. All
// exec sites go through it so provenance checks (trusted_opencode_path,
// producer fingerprints) see exactly the binary that would run.
func BinaryPath() (string, error) {
	binPathOnce.Do(func() {
		binPathVal, binPathErr = exec.LookPath(binName)
	})
	return binPathVal, binPathErr
}

// binCmd builds an exec.Cmd for the opencode CLI pinned to its resolved
// absolute path instead of relying on PATH lookup at spawn time.
func binCmd(args ...string) (*exec.Cmd, error) {
	path, err := BinaryPath()
	if err != nil {
		return nil, fmt.Errorf("resolve %s binary: %w", binName, err)
	}
	return exec.Command(path, args...), nil
}

// DataDir returns the OpenCode data directory, resolved per Phase 0 findings:
// $XDG_DATA_HOME/opencode when XDG_DATA_HOME is set, else ~/.local/share/opencode.
// NOTE: OPENCODE_DATA_DIR does not exist in OpenCode 1.18.18 and is ignored.
func DataDir() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "opencode"), nil
}

// dbPath returns the path to the OpenCode SQLite database.
func dbPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "opencode.db"), nil
}

// Export runs `opencode export <sessionID>` and writes the resulting JSON to
// outPath. OpenCode 1.18.18 truncates stdout at 64 KiB when stdout is a pipe,
// so use a regular file descriptor instead of exec.Cmd's pipe capture.
func Export(sessionID, outPath string) error {
	cmd, err := binCmd("export", sessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	var stderr strings.Builder
	cmd.Stdout = out
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	closeErr := out.Close()
	if runErr != nil {
		return fmt.Errorf("opencode export %s: %w: %s", sessionID, runErr, strings.TrimSpace(stderr.String()))
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}

// Import runs `opencode import <exportPath>` using the current process
// working directory. New write-back code must use ImportInto so OpenCode
// associates the import with the resolved target clone.
func Import(exportPath string) error {
	return ImportInto(exportPath, "")
}

// ImportInto runs `opencode import <exportPath>` from targetDir. OpenCode
// derives project context from its invocation directory, so omitting Cmd.Dir
// makes cross-device write-back target-dependent by accident.
func ImportInto(exportPath, targetDir string) error {
	cmd, err := binCmd("import", exportPath)
	if err != nil {
		return err
	}
	if targetDir != "" {
		cmd.Dir = targetDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("opencode import %s: %w: %s", exportPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ToolVersion returns the installed OpenCode version for write-back
// compatibility checks.
func ToolVersion() (string, error) {
	cmd, err := binCmd("--version")
	if err != nil {
		return "", err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("opencode --version: %w: %s", err, strings.TrimSpace(string(out)))
	}
	v := normalizeVersion(string(out))
	if v == "" {
		return "", fmt.Errorf("opencode --version returned no version")
	}
	return v, nil
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "opencode ")
	v = strings.TrimPrefix(v, "OpenCode ")
	return strings.TrimSpace(v)
}

// VerifyImport confirms that the patch associated the imported session with
// targetDir. It validates the same narrow schema boundary PatchImport uses.
func VerifyImport(exportPath, targetDir string) error {
	info, err := readExportInfo(exportPath)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+mustDBPath()+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open opencode db for verification: %w", err)
	}
	defer db.Close()
	var directory string
	if err := db.QueryRow(`SELECT directory FROM session WHERE id = ?`, info.ID).Scan(&directory); err != nil {
		return fmt.Errorf("verify imported session %s: %w", info.ID, err)
	}
	if filepath.Clean(directory) != filepath.Clean(targetDir) {
		return fmt.Errorf("verify imported session %s: directory %q, want %q", info.ID, directory, targetDir)
	}
	return nil
}

// exportInfo is the minimal shape of an export file's `info` object we need
// to read the session id, project id, and directory for the patch step.
type ExportInfo struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID"`
	Directory string `json:"directory"`
	Version   string `json:"version"`
	Title     string `json:"title"`
}

// readExportInfo parses just the `info` field of an export JSON file.
func readExportInfo(path string) (ExportInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ExportInfo{}, err
	}
	return parseExportInfo(data)
}

// PatchImport fixes the project_id and directory of an imported session so it
// lands on the correct OpenCode project (Phase 0 findings, §4–5). It is
// idempotent: if the session already has the right project_id, it's a no-op.
//
// targetDir is the device-local clone path where the session should live.
// projectKey is the target project's directory/identity to ensure a project
// row exists.
func PatchImport(exportPath, targetDir, projectKey string) error {
	info, err := readExportInfo(exportPath)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+mustDBPath()+"?mode=rw")
	if err != nil {
		return fmt.Errorf("open opencode db: %w", err)
	}
	defer db.Close()

	// Ensure the target project row exists (id = hash of project key is
	// unreliable; use the project id from the export if present, else derive).
	// We prefer the export's own projectID when it's not "global".
	projectID := info.ProjectID
	if projectID == "" || projectID == "global" {
		// Derive a stable project id from the target dir's git remote.
		pid, err := deriveProjectID(targetDir)
		if err != nil {
			return fmt.Errorf("derive target project id: %w", err)
		}
		projectID = pid
	}

	// Upsert project row (ensure exists).
	if _, err := db.Exec(`
		INSERT INTO project (id, worktree, sandboxes, commands, time_created, time_updated)
		VALUES (?, ?, '', '', unixepoch(), unixepoch())
		ON CONFLICT(id) DO NOTHING`, projectID, targetDir); err != nil {
		return fmt.Errorf("upsert project row: %w", err)
	}

	// Upsert project_directory join row.
	if _, err := db.Exec(`
		INSERT INTO project_directory (project_id, directory, time_created)
		VALUES (?, ?, unixepoch())
		ON CONFLICT(project_id, directory) DO NOTHING`, projectID, targetDir); err != nil {
		return fmt.Errorf("upsert project_directory row: %w", err)
	}

	// Fix the session row.
	res, err := db.Exec(`UPDATE session SET project_id = ?, directory = ? WHERE id = ?`,
		projectID, targetDir, info.ID)
	if err != nil {
		return fmt.Errorf("patch session row: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("session %s not found after import", info.ID)
	}
	return nil
}

func mustDBPath() string {
	p, err := dbPath()
	if err != nil {
		return "opencode.db"
	}
	return p
}

// deriveProjectID produces a stable project id for targetDir by hashing the
// canonical key or, failing that, the path itself.
func deriveProjectID(targetDir string) (string, error) {
	// Try git remote.
	cmd := exec.Command("git", "-C", targetDir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return hashString(strings.TrimSpace(string(out))), nil
	}
	return hashString(targetDir), nil
}

// IsToolRunning reports whether an opencode process is running for the
// current user, optionally scoped to a project directory (AGENTS.md invariant
// #2). It scans the process table, filtering by owner UID == current user and
// by the executable name. When targetPath is non-empty, it additionally checks
// each matching process's working directory (via /proc/<pid>/cwd) and only
// counts processes whose cwd is within targetPath — a per-candidate guard for
// broadcast write-back. It is inherently best-effort (TOCTOU) — do not present
// it as a lock.
func IsToolRunning(targetPath string) (bool, error) {
	pids, err := matchingPIDs(targetPath)
	if err != nil {
		return false, err
	}
	return len(pids) > 0, nil
}

// matchingPIDs returns PIDs of opencode processes owned by the current user.
// When targetPath is non-empty, only processes whose cwd falls under
// targetPath are returned (best-effort; a cwd we cannot read is counted, since
// refusing-to-write is the safe failure mode).
func matchingPIDs(targetPath string) ([]string, error) {
	cmd := exec.Command("pgrep", "-u", fmt.Sprint(os.Getuid()), "-f", binName)
	out, err := cmd.Output()
	if err != nil {
		// pgrep exits 1 when no match; that means not running.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	all := strings.Fields(string(out))
	if targetPath == "" {
		return all, nil
	}
	var scoped []string
	target := filepath.Clean(targetPath)
	for _, pid := range all {
		if cwd, ok := procCWD(pid); !ok || isWithin(cwd, target) {
			scoped = append(scoped, pid)
		}
	}
	return scoped, nil
}

// procCWD reads a process's working directory on Linux.
func procCWD(pid string) (string, bool) {
	dest, err := os.Readlink(filepath.Join("/proc", pid, "cwd"))
	if err != nil {
		return "", false
	}
	return dest, true
}

// isWithin reports whether path equals dir or is a descendant of it.
func isWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// hashString returns a stable hex hash for a string (used for project ids).
func hashString(s string) string {
	h := fnv64(s)
	return fmt.Sprintf("%x", h)
}

// fnv64 is a tiny non-crypto hash (FNV-1a 64-bit).
func fnv64(s string) uint64 {
	const offset uint64 = 14695981039346656037
	const prime uint64 = 1099511628211
	h := offset
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// ProcessTable is only used on non-Linux where pgrep -u may differ; kept as a
// stub for now since the target deployment is Linux.
func processTable() []string { return nil }

var _ = runtime.GOOS
