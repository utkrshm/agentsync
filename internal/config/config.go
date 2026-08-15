package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config mirrors the config.toml shape (IMPLEMENTATION-PLAN.md §6), with the
// v0.1 subset: a single hardcoded sync repo. Canonical-key resolution,
// repo-index scanning, and per-tool watch config are out of scope for v0.1.
type Config struct {
	Sync Sync `toml:"sync"`
}

type Sync struct {
	RepoPath string `toml:"repo_path"` // absolute path of the sync repo working tree
	Remote   string `toml:"remote"`    // e.g. git@github.com:user/agent-sessions.git
}

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
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return cfg, false, fmt.Errorf("parse %s: %w", p, err)
	}
	return cfg, true, nil
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
