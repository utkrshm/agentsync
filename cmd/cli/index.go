package main

import (
	"context"
	"fmt"
	"time"
)

const indexUsage = `agent-sync index — refresh the local repo-index cache

Usage:
  agent-sync index

Walks the directories configured under [repoindex] roots for local git
repos so receive can resolve canonical project keys to clone paths
without scanning at write-back time. Re-run after cloning a new
project. The cache lives at ~/.cache/agent-sync/repo-index.db, outside
the sync repo.
`

// cmdIndex runs a full repo-index scan over the configured roots, populating
// the reverse-resolution cache used by write-back (SPEC-DOC.md §4.1). It can
// be re-run any time (e.g. after cloning a new project).
func cmdIndex(args []string) error {
	for _, a := range args {
		switch a {
		default:
			return fmt.Errorf("unknown index flag %q", a)
		}
	}

	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	if len(cfg.RepoIndex.Roots) == 0 {
		return fmt.Errorf("no [repoindex] roots configured — add them to %s (e.g. roots = [\"~/dev\"]) and re-run", configPathHint())
	}

	idx, err := openRepoIndex()
	if err != nil {
		return err
	}
	start := time.Now()
	ctx := context.Background()
	if err := idx.Scan(ctx, cfg.RepoIndex.Roots, nil); err != nil {
		return fmt.Errorf("repo-index scan: %w", err)
	}
	fmt.Printf("Repo index refreshed from %d root(s) in %s.\n", len(cfg.RepoIndex.Roots), time.Since(start).Round(time.Millisecond))
	return nil
}

// configPathHint returns the config file path for error messages.
func configPathHint() string {
	if p, err := configFilePath(); err == nil {
		return p
	}
	return "~/.config/agent-sync/config.toml"
}
