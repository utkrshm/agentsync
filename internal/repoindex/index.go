// Package repoindex provides reverse resolution: given a canonical project
// key from a pulled session, find where that repo lives on THIS device
// (SPEC-DOC.md §4.1). Results are cached in a local SQLite DB at
// ~/.cache/agent-sync/repo-index.db — deliberately OUTSIDE the sync repo's
// working tree (AGENTS.md invariant #6).
//
// The scan is a worker-pool directory walk that stops descending the instant
// a `.git` dir is found, skips known-heavy directories, does not follow
// symlinks, and never crosses filesystem boundaries. It is NOT invoked on
// every pull (invariant #7) — callers trigger Scan explicitly (initial run,
// daily, or a rate-limited targeted rescan on a write-back miss).
package repoindex

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"agentsync/internal/canonicalkey"
	"agentsync/internal/session"
)

// Entry is one indexed local repo.
type Entry struct {
	CanonicalKey session.CanonicalKey
	LocalPath    string
	LastSeen     time.Time
}

// defaultIgnore are directory names the scan always skips descending into
// (SPEC-DOC.md §4.1). Callers may add more via Scan's ignore parameter.
var defaultIgnore = map[string]bool{
	"node_modules": true, ".venv": true, "vendor": true, "target": true,
	"dist": true, "build": true, ".cache": true, ".git": true,
}

// DB wraps the repo-index SQLite cache.
type DB struct {
	path string
}

// DefaultPath returns the cache DB location: ~/.cache/agent-sync/repo-index.db
// (AGENTS.md invariant #6 — never inside the sync repo working tree).
func DefaultPath() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "agent-sync", "repo-index.db"), nil
}

// Open opens (creating if needed) the repo-index DB at the given path.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, fmt.Errorf("open repo-index db: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS repo_index (
			canonical_key TEXT NOT NULL,
			local_path    TEXT NOT NULL,
			last_seen     INTEGER NOT NULL,
			PRIMARY KEY (canonical_key, local_path)
		)`); err != nil {
		return nil, fmt.Errorf("create repo-index schema: %w", err)
	}
	return &DB{path: path}, nil
}

// Resolve returns every local path on this device matching the canonical key
// (0, 1, or many). Pure cache read — no disk walk (invariant #7). Callers of
// write-back iterate over ALL candidates rather than picking one.
func (d *DB) Resolve(key session.CanonicalKey) ([]Entry, error) {
	db, err := sql.Open("sqlite", "file:"+d.path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(
		`SELECT local_path, last_seen FROM repo_index WHERE canonical_key = ?`,
		string(key))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var seen int64
		e.CanonicalKey = key
		if err := rows.Scan(&e.LocalPath, &seen); err != nil {
			return nil, err
		}
		e.LastSeen = time.Unix(seen, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Scan walks each configured root with a bounded worker pool, indexing every
// git repo found (stops descending at `.git`, skips `ignore` + default-heavy
// dirs, no symlink-follow, no filesystem-boundary crossing). It upserts
// (canonical_key, local_path) rows with the current last_seen.
func (d *DB) Scan(ctx context.Context, roots []string, ignore []string) error {
	ign := make(map[string]bool, len(defaultIgnore)+len(ignore))
	for k := range defaultIgnore {
		ign[k] = true
	}
	for _, i := range ignore {
		ign[i] = true
	}

	db, err := sql.Open("sqlite", "file:"+d.path)
	if err != nil {
		return err
	}
	defer db.Close()

	// Collect all entries from the walk, then upsert in one batch per worker
	// batch to keep DB writes out of the walk hot path.
	var (
		mu    sync.Mutex
		keys  []session.CanonicalKey
		paths []string
	)
	upsert := func(key session.CanonicalKey, path string) {
		mu.Lock()
		keys = append(keys, key)
		paths = append(paths, path)
		mu.Unlock()
	}

	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	dirs := make(chan string, workers*4)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for dir := range dirs {
				d.processRoot(ctx, dir, ign, upsert)
			}
		}()
	}

	var sendWG sync.WaitGroup
	for _, root := range roots {
		root = filepath.Clean(root)
		if _, err := os.Stat(root); err != nil {
			continue // configured root doesn't exist — skip silently
		}
		sendWG.Add(1)
		go func(r string) {
			defer sendWG.Done()
			select {
			case dirs <- r:
			case <-ctx.Done():
			}
		}(root)
	}
	sendWG.Wait()
	close(dirs)
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return err
	}
	return d.batchUpsert(db, keys, paths)
}

// processRoot walks a single root directory (recursively, depth-first via its
// own stack) and calls upsert for every repo root found. It never descends
// into ignored dirs, never follows symlinks, and stops at each `.git`.
func (d *DB) processRoot(ctx context.Context, root string, ignore map[string]bool,
	upsert func(session.CanonicalKey, string)) {

	rootDev := devOf(root)
	stack := []string{root}
	for len(stack) > 0 {
		if ctx.Err() != nil {
			return
		}
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		hasGit := false
		for _, e := range entries {
			name := e.Name()
			if e.Type()&os.ModeSymlink != 0 {
				continue // never follow symlinks
			}
			if !e.IsDir() {
				continue
			}
			if name == ".git" {
				hasGit = true
				continue
			}
		}
		if hasGit {
			// This directory is a repo root. Stop descending the instant a
			// `.git` is found — no reason to walk node_modules, build output,
			// or the working tree (SPEC-DOC.md §4.1).
			key := canonicalkey.Resolve(dir)
			upsert(key, dir)
			continue
		}
		for _, e := range entries {
			name := e.Name()
			full := filepath.Join(dir, name)
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			if !e.IsDir() {
				continue
			}
			if ignore[name] {
				continue
			}
			if devOf(full) != rootDev {
				continue // don't cross filesystem boundaries
			}
			stack = append(stack, full)
		}
	}
}

// batchUpsert inserts or refreshes all collected entries in one transaction.
func (d *DB) batchUpsert(db *sql.DB, keys []session.CanonicalKey, paths []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`
		INSERT INTO repo_index (canonical_key, local_path, last_seen)
		VALUES (?, ?, ?)
		ON CONFLICT(canonical_key, local_path) DO UPDATE SET last_seen = excluded.last_seen`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().Unix()
	for i := range keys {
		if _, err := stmt.Exec(string(keys[i]), paths[i], now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// devOf returns the device id of a path's containing filesystem, or -1 on
// error (caller treats -1 as "skip boundary check").
func devOf(path string) uint64 {
	fi, err := os.Stat(path)
	if err != nil {
		return ^uint64(0)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return ^uint64(0)
}
