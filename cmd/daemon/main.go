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
	"agentsync/internal/retry"
	"agentsync/internal/session"
	"agentsync/internal/syncrepo"
	"agentsync/internal/watch"
)

// staleTempMaxAge bounds how long an unclaimed export temp payload may live
// before the startup sweep removes it.
const staleTempMaxAge = 24 * time.Hour

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

	// Clean up export temp payloads leaked by earlier crashed runs (they hold
	// session transcripts; do not let them accumulate).
	if n := opencode.SweepStaleTemps(staleTempMaxAge); n > 0 {
		fmt.Printf("daemon: removed %d stale export temp file(s)\n", n)
	}

	repo := syncrepo.Open(cfg.Sync.RepoPath)
	if !repo.Exists() {
		return fmt.Errorf("sync repo not initialized at %s — run `agent-sync init`", cfg.Sync.RepoPath)
	}
	retryPath, err := retry.DefaultPath()
	if err != nil {
		return err
	}
	retries, err := retry.Open(retryPath)
	if err != nil {
		return err
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
				retryPendingPush(repo, retries)
			}
		}
	}()

	if cfg.Watch.OpenCode.Enabled {
		if err := runOpenCodeWatcher(ctx, cfg, repo, retries); err != nil {
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
func runOpenCodeWatcher(ctx context.Context, cfg config.Config, repo *syncrepo.Repo, retries *retry.Store) error {
	ad := opencode.NewAdapter()
	// Provenance config is wired even though the daemon only captures: any
	// future write-back path through this adapter must honor the same pin.
	ad.TrustedPath = cfg.Producer.TrustedPath
	ad.StrictCheck = cfg.Producer.StrictCheck
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
			mirrored := make([]session.Session, 0, len(sessions))
			// Mirror replaces PayloadPath with the repo destination, so the
			// temp payload paths must be captured before mirroring.
			temps := make([]string, 0, len(sessions))
			for i := range sessions {
				tempPayload := sessions[i].PayloadPath
				if err := ad.Mirror(&sessions[i], repo.Path); err != nil {
					fmt.Fprintf(os.Stderr, "daemon: mirror %s (%s): %v\n",
						sessions[i].ID, sessions[i].CanonicalKey, err)
					continue
				}
				mirrored = append(mirrored, sessions[i])
				temps = append(temps, tempPayload)
				logEvent("mirror", string(sessions[i].Tool), string(sessions[i].CanonicalKey), sessions[i].ID, "ok")
			}
			if len(mirrored) == 0 {
				return nil
			}
			// One commit covering the debounced batch. A batch that produced
			// no actual file changes (re-mirrored, already-committed content)
			// is a no-op, not an error — log it and continue rather than
			// taking down the daemon.
			ts := time.Now().UTC().Format(time.RFC3339)
			if _, err := repo.Commit("opencode", batchLabel(mirrored), ts); err != nil {
				if errors.Is(err, syncrepo.ErrNoChanges) {
					logEvent("commit", "opencode", batchLabel(mirrored), "", "noop")
				} else {
					return fmt.Errorf("commit: %w", err)
				}
			}
			if err := ad.Acknowledge(mirrored); err != nil {
				return fmt.Errorf("acknowledge capture: %w", err)
			}
			// Only after the artifacts are durably committed and the capture
			// cursor advanced are the temp payloads disposable.
			for _, tp := range temps {
				if err := os.Remove(tp); err != nil && !os.IsNotExist(err) {
					fmt.Fprintf(os.Stderr, "daemon: remove temp payload %s: %v\n", tp, err)
					continue
				}
				logEvent("cleanup-temp", "opencode", "", "", "ok")
			}
			logEvent("commit", "opencode", batchLabel(mirrored), "", ts)
			if cfg.Sync.Remote != "" {
				if err := repo.Push(); err != nil {
					if scheduleErr := retries.Schedule(retry.OperationPush, "sync-repo", err.Error()); scheduleErr != nil {
						return fmt.Errorf("push: %w; schedule retry: %v", err, scheduleErr)
					}
					logEvent("push", "opencode", batchLabel(mirrored), "", "retry-scheduled")
					return nil
				}
				if err := retries.Complete(retry.OperationPush, "sync-repo"); err != nil {
					return fmt.Errorf("clear push retry: %w", err)
				}
				logEvent("push", "opencode", batchLabel(mirrored), "", "ok")
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

func retryPendingPush(repo *syncrepo.Repo, retries *retry.Store) {
	items, err := retries.Due(time.Now().UTC())
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: load retry state: %v\n", err)
		return
	}
	for _, item := range items {
		if item.Operation != retry.OperationPush {
			continue
		}
		if err := repo.Push(); err != nil {
			if scheduleErr := retries.Schedule(retry.OperationPush, item.Key, err.Error()); scheduleErr != nil {
				fmt.Fprintf(os.Stderr, "daemon: retry push: %v; schedule retry: %v\n", err, scheduleErr)
			} else {
				logEvent("push", "sync", item.Key, "", "retry-scheduled")
			}
			continue
		}
		if err := retries.Complete(item.Operation, item.Key); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: clear push retry: %v\n", err)
			continue
		}
		logEvent("push", "sync", item.Key, "", "retry-ok")
	}
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
