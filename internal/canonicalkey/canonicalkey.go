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

// NormalizeRemote is THE single place remote URLs are interpreted anywhere in
// the codebase: every transport spelling of one repository must reduce to the
// same (host, segments) pair.
//   - trims space; repeatedly drops trailing "/" and ".git"
//   - "scheme://rest"  → strip scheme; if "@" occurs before the first "/",
//     drop everything through it (userinfo)
//   - SCP form "[user@]host:path" → drop through last "@", split first ":"
//   - host: lowercased; ":port" suffix stripped (numeric only)
//   - segments: non-empty path parts; "." and ".." dropped; FULL retention
//     (GitLab-style subgroups must stay unique)
//
// Case folding applies to both host and segments: a repository spelled with
// different casing across transports (HTTPS://GITHUB.COM/USER/REPO.GIT vs
// git@github.com:user/repo.git) must land on ONE identity, so the fold cannot
// stop at the host.
//
// Degenerate inputs with no host at all (bare paths, empty strings) return an
// empty host; their segments keep the original slash positions verbatim (they
// may contain empty parts) so callers reproduce today's byte-for-byte output —
// see keyFromURL.
func NormalizeRemote(raw string) (host string, segments []string) {
	s := strings.ToLower(strings.TrimSpace(raw))

	// Repeatedly drop trailing "/" and ".git" so mixed tails like
	// ".../repo.git//" fully collapse.
	for trimmed := true; trimmed && s != ""; {
		trimmed = false
		if i := len(s); i > 0 && s[i-1] == '/' {
			s = strings.TrimRight(s, "/")
			trimmed = true
			continue
		}
		if strings.HasSuffix(s, ".git") {
			s = strings.TrimSuffix(s, ".git")
			trimmed = true
		}
	}

	var hostPart, pathPart string
	if i := strings.Index(s, "://"); i >= 0 {
		// scheme://rest — work on the remainder after the scheme.
		rest := s[i+3:]
		var path string
		if slash := strings.Index(rest, "/"); slash >= 0 {
			path = rest[slash:]
			rest = rest[:slash]
		}
		if at := strings.Index(rest, "@"); at >= 0 {
			rest = rest[at+1:] // userinfo
		}
		hostPart = rest
		pathPart = path
	} else {
		// SCP form "[user@]host:path".
		if at := strings.LastIndex(s, "@"); at >= 0 {
			s = s[at+1:]
		}
		if colon := strings.Index(s, ":"); colon >= 0 {
			hostPart = s[:colon]
			pathPart = s[colon+1:]
		} else {
			// No host:path structure: degenerate bare path or bare name.
			// Historic behavior pushed the whole string through a plain
			// "/"→"-" mapping; keyFromURL reproduces those bytes exactly by
			// keeping EMPTY separator positions in the segments, so a leading
			// or repeated slash still maps to its dash.
			return "", strings.Split(s, "/")
		}
	}

	return stripNumericPort(hostPart), splitSegments(pathPart)
}

// stripNumericPort removes a single trailing ":<digits>" from host. Non-numeric
// or empty port suffixes are left alone.
func stripNumericPort(host string) string {
	i := strings.LastIndex(host, ":")
	if i <= 0 || !allDigits(host[i+1:]) {
		return host
	}
	return host[:i]
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// splitSegments keeps non-empty path parts, dropping "." and "..".
func splitSegments(path string) []string {
	var segs []string
	for _, part := range strings.Split(path, "/") {
		switch part {
		case "", ".", "..":
			continue
		default:
			segs = append(segs, part)
		}
	}
	return segs
}

// keyFromURL projects a normalized remote onto today's scp-era flat key format
// ("host-seg-seg"). Existing stored scp-era keys are unchanged byte-for-byte
// (zero migration): Resolve still passes this through slug(), which only ever
// sanitized the ":" that used to survive here. Cross-transport spellings that
// previously diverged ("ssh---…", "https---…") now converge on it.
func keyFromURL(url string) string {
	host, segs := NormalizeRemote(url)
	if host == "" {
		// Degenerate bare-path input: segments carry every separator
		// position verbatim, so joining them alone reproduces today's
		// plain "/"→"-" mapped bytes.
		return strings.Join(segs, "-")
	}
	full := append([]string{host}, segs...)
	return strings.Join(full, "-")
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
