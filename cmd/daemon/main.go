// Command agent-sync daemon runs the background capture daemon (Phase 2): it
// watches OpenCode's storage, exports changed sessions, mirrors them into the
// sync repo, and commits+pushes. It also runs a periodic pull so a read-only
// device stays current (SPEC-DOC.md §5.2, trigger 2).
//
// Structured logging per AGENTS.md: session id, tool, canonical key, action.
// The daemon must never panic on a malformed session — explicit error
// returns throughout.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/config"
	"agentsync/internal/session"
	"agentsync/internal/syncrepo"
	"agentsync/internal/watch"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-sync daemon: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, exists, err := config.Load()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("no config found — run `agent-sync init` first")
	}
	if cfg.Sync.RepoPath == "" {
		cfg.Sync.RepoPath = config.DefaultRepoPath()
	}

	repo := syncrepo.Open(cfg.Sync.RepoPath)
	if !repo.Exists() {
		return fmt.Errorf("sync repo not initialized at %s — run `agent-sync init`", cfg.Sync.RepoPath)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Periodic pull ticker (trigger 2) so a read-only device picks up remote
	// changes without a write. Runs regardless of whether capture is enabled.
	pullDone := make(chan struct{})
	go func() {
		defer close(pullDone)
		tick := time.NewTicker(time.Duration(cfg.Sync.PollIntervalSeconds) * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := repo.PullFastForward(); err != nil {
					fmt.Fprintf(os.Stderr, "daemon: periodic pull: %v\n", err)
				} else {
					logEvent("pull", "poll", "", "", "ok")
				}
			}
		}
	}()

	if cfg.Watch.OpenCode.Enabled {
		if err := runOpenCodeWatcher(ctx, cfg, repo); err != nil {
			return err
		}
	} else {
		fmt.Println("daemon: [watch.opencode] disabled — capture off; only periodic pull running")
		<-ctx.Done()
	}
	<-pullDone
	return nil
}

// runOpenCodeWatcher wires the OpenCode capture adapter into the watch core.
func runOpenCodeWatcher(ctx context.Context, cfg config.Config, repo *syncrepo.Repo) error {
	ad := opencode.NewAdapter()
	// OpenCode stores every project in one database. Evaluate deny policy from
	// per-session metadata before export, not from the shared DB filename.
	ad.ShouldCapture = func(localPath string, key session.CanonicalKey) bool {
		return !watch.DenyMatches(cfg.Watch.OpenCode.Deny, localPath) &&
			!watch.DenyMatches(cfg.Watch.OpenCode.Deny, string(key))
	}
	w, err := watch.New(watch.Options{
		Adapter:   ad,
		Debounce:  time.Duration(cfg.Sync.DebounceSeconds) * time.Second,
		DenyGlobs: cfg.Watch.OpenCode.Deny,
		OnSessions: func(sessions []session.Session) error {
			for i := range sessions {
				if err := ad.Mirror(&sessions[i], repo.Path); err != nil {
					fmt.Fprintf(os.Stderr, "daemon: mirror %s (%s): %v\n",
						sessions[i].ID, sessions[i].CanonicalKey, err)
					continue
				}
				logEvent("mirror", string(sessions[i].Tool), string(sessions[i].CanonicalKey), sessions[i].ID, "ok")
			}
			if len(sessions) == 0 {
				return nil
			}
			// One commit covering the debounced batch. A batch that produced
			// no actual file changes (re-mirrored, already-committed content)
			// is a no-op, not an error — log it and continue rather than
			// taking down the daemon.
			ts := time.Now().UTC().Format(time.RFC3339)
			if _, err := repo.Commit("opencode", batchLabel(sessions), ts); err != nil {
				if errors.Is(err, syncrepo.ErrNoChanges) {
					logEvent("commit", "opencode", batchLabel(sessions), "", "noop")
					return nil
				}
				return fmt.Errorf("commit: %w", err)
			}
			logEvent("commit", "opencode", batchLabel(sessions), "", ts)
			if cfg.Sync.Remote != "" {
				if err := repo.Push(); err != nil {
					return fmt.Errorf("push: %w (commit was made locally; the daemon will retry on the next change)", err)
				}
				logEvent("push", "opencode", batchLabel(sessions), "", "ok")
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("daemon: watching opencode storage (debounce %ds, deny %v)\n",
		cfg.Sync.DebounceSeconds, cfg.Watch.OpenCode.Deny)
	return w.Start(ctx)
}

// batchLabel summarizes a batch for commit/log messages: "3-sessions" or a
// single session id.
func batchLabel(sessions []session.Session) string {
	if len(sessions) == 1 {
		return sessions[0].ID
	}
	return fmt.Sprintf("%d-sessions", len(sessions))
}

// logEvent writes a structured one-line event to stdout.
func logEvent(action, tool, key, sessionID, status string) {
	fmt.Printf("daemon: action=%s tool=%s key=%s session=%s status=%s\n",
		action, tool, key, sessionID, status)
}
