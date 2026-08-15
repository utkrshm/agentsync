// Package watch provides the fsnotify-based watcher core (IMPLEMENTATION-PLAN.md
// §3): recursive watch setup per adapter, debounce, deny-glob enforcement
// (AGENTS.md invariant #5), and a startup reconciliation pass.
//
// The watcher is tool-agnostic: it talks only to a session.Adapter. Per-tool
// change semantics (SQLite watermark for OpenCode, JSONL tail offsets for
// future adapters) live inside the adapter's OnChange.
package watch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"agentsync/internal/session"
)

// Options configures a Watcher.
type Options struct {
	Adapter   session.Adapter
	Debounce  time.Duration // window to wait after the last event before firing OnChange
	DenyGlobs []string      // glob patterns; a matching path never gets a watch (invariant #5)
	// OnSessions is invoked with the sessions an adapter produced for a
	// debounced change. It runs on the watcher's own goroutine — implementors
	// must not block indefinitely.
	OnSessions func([]session.Session) error
}

// Watcher watches an adapter's paths and fires OnChange after a debounce.
type Watcher struct {
	opts Options

	mu        sync.Mutex
	lastEvent map[string]time.Time // path -> time of most recent event
	debounced map[string]bool      // path already queued for OnChange
}

// New creates a Watcher. It does not attach watches yet — call Start.
func New(opts Options) (*Watcher, error) {
	if opts.Adapter == nil {
		return nil, fmt.Errorf("watch: adapter is required")
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 3 * time.Second
	}
	return &Watcher{
		opts:      opts,
		lastEvent: map[string]time.Time{},
		debounced: map[string]bool{},
	}, nil
}

// denied reports whether path matches any deny glob.
func (w *Watcher) denied(path string) bool {
	for _, glob := range w.opts.DenyGlobs {
		if ok, _ := filepath.Match(glob, path); ok {
			return true
		}
		// Also allow matching against the base name for convenience.
		if ok, _ := filepath.Match(glob, filepath.Base(path)); ok {
			return true
		}
	}
	return false
}

// Start runs the reconciliation pass, attaches all watches, and blocks
// watching for changes until ctx is cancelled. Any watch path that doesn't
// exist yet (tool creates it lazily) is skipped — new directories are picked
// up dynamically from Create events.
func (w *Watcher) Start(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watch: fsnotify: %w", err)
	}
	defer watcher.Close()

	if err := w.reconcile(); err != nil {
		return fmt.Errorf("watch: reconciliation: %w", err)
	}

	paths, err := w.opts.Adapter.WatchPaths()
	if err != nil {
		return fmt.Errorf("watch: adapter WatchPaths: %w", err)
	}
	for _, p := range paths {
		w.addRecursive(watcher, p)
	}

	// Debounce sweeper: fires OnChange for paths that have been quiet for the
	// window. Uses a coarse ticker; firing is idempotent per queued path.
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.sweep(watcher)
			}
		}
	}()

	err = w.eventLoop(ctx, watcher)
	<-sweepDone
	return err
}

// reconcile is the startup pass: fires OnChange once per adapter watch path so
// changes made while the daemon was down are picked up (the adapter's own
// watermark/offset logic guarantees no duplication).
func (w *Watcher) reconcile() error {
	paths, err := w.opts.Adapter.WatchPaths()
	if err != nil {
		return err
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			continue // doesn't exist yet — nothing to reconcile
		}
		if w.denied(p) {
			continue
		}
		if err := w.fire(filepath.Dir(p)); err != nil {
			return err
		}
	}
	return nil
}

// addRecursive attaches a watch to p (if a directory) and every existing
// subdirectory under it. Denied paths are never watched (invariant #5). For a
// file path, its parent directory is watched instead so Create/Write on the
// file is visible.
func (w *Watcher) addRecursive(watcher *fsnotify.Watcher, p string) {
	if w.denied(p) {
		return
	}
	info, err := os.Stat(p)
	if err != nil {
		return // path doesn't exist (tool creates it lazily) — skip for now
	}
	dir := p
	if !info.IsDir() {
		dir = filepath.Dir(p)
	}
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if w.denied(path) {
				// Still walk (a denied dir may contain a non-denied subdir)
				// but never attach a watch to it.
				return nil
			}
			_ = watcher.Add(path)
		}
		return nil
	})
}

// eventLoop consumes fsnotify events, records them per-path for debounce, and
// dynamically attaches watches to newly created directories.
func (w *Watcher) eventLoop(ctx context.Context, watcher *fsnotify.Watcher) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			w.handleEvent(watcher, ev)
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			// Log-and-continue: a single watch error must not take down the
			// whole daemon (AGENTS.md coding conventions).
			fmt.Fprintf(os.Stderr, "watch: fsnotify error: %v\n", err)
		}
	}
}

func (w *Watcher) handleEvent(watcher *fsnotify.Watcher, ev fsnotify.Event) {
	if w.denied(ev.Name) {
		return
	}
	if ev.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() && !w.denied(ev.Name) {
			_ = watcher.Add(ev.Name)
		}
	}
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
		return
	}
	w.mu.Lock()
	w.lastEvent[ev.Name] = time.Now()
	w.mu.Unlock()
}

// sweep fires OnChange for every path whose debounce window has elapsed.
func (w *Watcher) sweep(watcher *fsnotify.Watcher) {
	w.mu.Lock()
	now := time.Now()
	var ready []string
	for p, t := range w.lastEvent {
		if now.Sub(t) >= w.opts.Debounce {
			ready = append(ready, p)
			delete(w.lastEvent, p)
		}
	}
	w.mu.Unlock()

	for _, p := range ready {
		if err := w.fire(p); err != nil {
			fmt.Fprintf(os.Stderr, "watch: OnChange for %s: %v\n", p, err)
		}
	}
}

// fire runs the adapter's OnChange for the changed path and hands any produced
// sessions to OnSessions. Errors are returned to the caller.
func (w *Watcher) fire(path string) error {
	sessions, err := w.opts.Adapter.OnChange(session.WatchEvent{Path: path})
	if err != nil {
		return err
	}
	if len(sessions) == 0 || w.opts.OnSessions == nil {
		return nil
	}
	return w.opts.OnSessions(sessions)
}

// DenyMatches reports whether the given glob list contains any pattern that
// matches path — exported so the daemon can surface which paths are excluded.
func DenyMatches(globs []string, path string) bool {
	for _, glob := range globs {
		if ok, _ := filepath.Match(glob, path); ok {
			return true
		}
		if ok, _ := filepath.Match(glob, filepath.Base(path)); ok {
			return true
		}
	}
	return false
}