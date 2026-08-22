package main

import (
	"fmt"

	"agentsync/internal/syncrepo"
)

const pullUsage = `agent-sync pull — fetch and fast-forward the sync repo only

Usage:
  agent-sync pull

No write-back. Refuses diverged history (never force-pushes or
auto-merges) and refuses to delete untracked files outside the sync
allowlist (opencode/** and .gitignore); such conflicts fail with the
offending paths listed for manual resolution.
`

// cmdPull fetches and fast-forwards the sync repo only (foundation for the
// shell-init trigger, IMPLEMENTATION-PLAN.md §4.2).
func cmdPull(args []string) error {
	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	repo := syncrepo.Open(cfg.Sync.RepoPath)
	if !repo.Exists() {
		return fmt.Errorf("sync repo not initialized at %s — run `agent-sync init`", cfg.Sync.RepoPath)
	}
	if cfg.Sync.Remote == "" {
		return fmt.Errorf("no remote configured — run `agent-sync init --repo <url>`")
	}
	if err := repo.PullFastForward(); err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	fmt.Printf("Sync repo up to date at %s\n", cfg.Sync.RepoPath)
	return nil
}
