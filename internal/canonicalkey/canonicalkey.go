// Package canonicalkey resolves the stable identity of a project from a
// local path, per SPEC-DOC.md §4: git remote URL → first-commit hash → alias
// file → _unmapped sentinel.
package canonicalkey

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agentsync/internal/session"
)

// aliasesFile is the path to the project-alias TOML (optional in v0.1).
var aliasesFile = func() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "agent-sync", "project-aliases.toml")
}

// Resolve determines the canonical key for the project rooted at localPath.
// Returns the _unmapped sentinel (never an error) when no key can be
// established, per AGENTS.md invariant #3.
func Resolve(localPath string) session.CanonicalKey {
	// 1. Origin remote URL, walking up to find the nearest .git.
	url, ok := originURL(localPath)
	if ok {
		return slug(keyFromURL(url))
	} // 2. First-commit hash for a repo with no remote.
	if hash, ok := firstCommitHash(localPath); ok {
		return session.CanonicalKey(hash)
	}
	// 3. Alias file.
	if k, ok := alias(localPath); ok {
		return k
	}
	// 4. _unmapped sentinel.
	return session.CanonicalKey("_unmapped/" + strings.TrimPrefix(filepath.Clean(localPath), string(filepath.Separator)))
}

// originURL finds the nearest enclosing .git/config and returns the origin
// remote URL.
func originURL(localPath string) (string, bool) {
	dir := localPath
	for {
		cfg := filepath.Join(dir, ".git", "config")
		if data, err := os.ReadFile(cfg); err == nil {
			// Parse `[remote "origin"]` and its `url =` line without shelling out.
			inOrigin := false
			for _, line := range strings.Split(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "[remote") {
					inOrigin = strings.Contains(trimmed, `"origin"`)
					continue
				}
				if strings.HasPrefix(trimmed, "[") {
					inOrigin = false
					continue
				}
				if inOrigin && strings.HasPrefix(trimmed, "url") {
					val := strings.TrimSpace(strings.TrimPrefix(trimmed, "url"))
					val = strings.Trim(val, `"=`)
					val = strings.TrimSpace(val)
					if val != "" {
						return val, true
					}
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// firstCommitHash returns the first commit hash for a repo with no remote.
func firstCommitHash(localPath string) (string, bool) {
	cmd := exec.Command("git", "-C", localPath, "log", "--reverse", "--format=%H")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	lines := strings.Fields(string(out))
	if len(lines) == 0 {
		return "", false
	}
	return lines[0], true
}

// alias looks up localPath in the alias file (best-effort; empty file = none).
func alias(localPath string) (session.CanonicalKey, bool) {
	p := aliasesFile()
	if p == "" {
		return "", false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	// Extremely minimal parser: lines `path = "key"` or `"path" = "key"`.
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		key := strings.Trim(strings.TrimSpace(parts[0]), `" `)
		val := strings.Trim(strings.TrimSpace(parts[1]), `" `)
		if key == localPath || filepath.Clean(key) == filepath.Clean(localPath) {
			return session.CanonicalKey(val), true
		}
	}
	return "", false
}

// keyFromURL normalizes a remote URL into a stable slug.
func keyFromURL(url string) string {
	// Strip optional scheme/user@ and ".git" suffix, keep host/path.
	u := url
	if i := strings.LastIndex(u, "@"); i >= 0 && !strings.Contains(u[:i], "://") {
		u = u[i+1:]
	}
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimPrefix(u, "git@")
	return strings.ReplaceAll(u, "/", "-")
}

// slug makes a key filesystem-safe for use as a directory name.
func slug(k string) session.CanonicalKey {
	s := string(k)
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, s)
	return session.CanonicalKey(s)
}
