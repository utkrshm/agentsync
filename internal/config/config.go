package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config mirrors the config.toml shape (IMPLEMENTATION-PLAN.md §6). v0.1
// supports the sync repo pair; Phase 2 adds the watch section for the daemon.
// Canonical-key resolution, repo-index scanning, and per-tool write-back
// config are out of scope for now.
type Config struct {
	Sync  Sync        `toml:"sync"`
	Watch WatchConfig `toml:"watch"`
}

type Sync struct {
	RepoPath string `toml:"repo_path"` // absolute path of the sync repo working tree
	Remote   string `toml:"remote"`    // e.g. git@github.com:user/agent-sessions.git
	// DebounceSeconds is how long the watcher waits after the last event on a
	// file before firing OnChange (IMPLEMENTATION-PLAN.md §3).
	DebounceSeconds int `toml:"debounce_seconds"`
	// PollIntervalSeconds is the periodic pull interval for read-only devices
	// (SPEC-DOC.md §5.2, trigger 2).
	PollIntervalSeconds int `toml:"poll_interval_seconds"`
}

// WatchConfig holds per-tool watcher settings (IMPLEMENTATION-PLAN.md §6).
type WatchConfig struct {
	OpenCode OpenCodeWatch `toml:"opencode"`
}

// OpenCodeWatch is the watcher config for the OpenCode adapter.
type OpenCodeWatch struct {
	Enabled bool     `toml:"enabled"`
	Deny    []string `toml:"deny"` // glob patterns against canonical key or raw path
}

const (
	defaultDebounceSeconds     = 3
	defaultPollIntervalSeconds = 120
)

// DefaultRepoPath is where the sync repo lives unless the user configures
// otherwise. Kept outside this repo's own tree and outside the sync repo.
func DefaultRepoPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~/agent-sessions"
	}
	return filepath.Join(home, "agent-sessions")
}

// Path returns the config file path: ~/.config/agent-sync/config.toml
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent-sync", "config.toml"), nil
}

// Load reads the config file. Returns (zero Config, false) if the file does
// not exist yet (caller decides whether to run init).
func Load() (Config, bool, error) {
	var cfg Config
	p, err := Path()
	if err != nil {
		return cfg, false, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, false, nil
		}
		return cfg, false, err
	}
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return cfg, false, fmt.Errorf("parse %s: %w", p, err)
	}
	cfg.ApplyDefaults(md)
	return cfg, true, nil
}

// ApplyDefaults fills in sane defaults for keys absent from an existing
// config.toml (older files predate the watch section). Existing non-zero
// values are preserved. The OpenCode watcher defaults to enabled when the
// `watch.opencode.enabled` key is absent entirely; an explicit
// `enabled = false` is respected (md.IsDefined is true then).
func (c *Config) ApplyDefaults(md toml.MetaData) {
	if c.Sync.DebounceSeconds <= 0 {
		c.Sync.DebounceSeconds = defaultDebounceSeconds
	}
	if c.Sync.PollIntervalSeconds <= 0 {
		c.Sync.PollIntervalSeconds = defaultPollIntervalSeconds
	}
	if c.Watch.OpenCode.Deny == nil {
		c.Watch.OpenCode.Deny = []string{}
	}
	if !md.IsDefined("watch", "opencode", "enabled") {
		c.Watch.OpenCode.Enabled = true
	}
}

// Save writes the config file, creating the directory if needed, with
// default (owner-only) permissions.
func (c *Config) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	var b []byte
	buf := &stringBuilder{}
	if err := toml.NewEncoder(buf).Encode(c); err != nil {
		return err
	}
	b = []byte(buf.String())
	return os.WriteFile(p, b, 0o600)
}

type stringBuilder struct{ s string }

func (sb *stringBuilder) Write(p []byte) (int, error) { sb.s += string(p); return len(p), nil }
func (sb *stringBuilder) String() string              { return sb.s }
