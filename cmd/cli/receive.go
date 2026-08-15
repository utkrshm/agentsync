package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/repoindex"
	"agentsync/internal/session"
	"agentsync/internal/syncrepo"
)

// cmdReceive pulls the sync repo and writes back any new OpenCode sessions
// into local clones of the matching project, applying the
// project_id/directory patch. Write-back is broadcast across EVERY local
// clone resolved for a session's canonical key (SPEC-DOC.md §4.1); a clone
// where opencode is currently running is skipped and retried on the next pull
// (AGENTS.md invariant #2).
func cmdReceive(args []string) error {
	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	repo := syncrepo.Open(cfg.Sync.RepoPath)
	if !repo.Exists() {
		return fmt.Errorf("sync repo not initialized at %s — run `agent-sync init`", cfg.Sync.RepoPath)
	}

	if cfg.Sync.Remote != "" {
		// Blocking pre-write-back pull (SPEC-DOC.md §5.2, trigger 4) — writing
		// against stale state is the one case where deferring a pull is unsafe.
		if err := repo.PullForced(); err != nil {
			return fmt.Errorf("sync-repo pull: %w", err)
		}
	}

	idx, err := openRepoIndex()
	if err != nil {
		return err
	}

	meta, err := repo.ReadMeta()
	if err != nil {
		meta = syncrepo.SyncMeta{SchemaVersion: 1, DeviceID: "dev-" + fmt.Sprint(time.Now().UnixNano()), Imported: map[string]string{}}
	}
	if meta.Imported == nil {
		meta.Imported = map[string]string{}
	}

	exports, err := findExports(repo.Path)
	if err != nil {
		return err
	}

	ad := opencode.NewAdapter()
	imported := 0
	noClone := 0
	for _, ex := range exports {
		if _, done := meta.Imported[ex.SessionID]; done {
			continue
		}
		title := ""
		if im, err := readImportMeta(ex.ImportMetaPath); err == nil {
			title = im.Title
		}

		// Resolve every local clone of this session's project.
		candidates, err := idx.Resolve(session.CanonicalKey(ex.Key))
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warn: resolve %s: %v\n", ex.SessionID, err)
			continue
		}
		if len(candidates) == 0 {
			// No local clone yet — archived only. Not marked imported, so a
			// later repo-index rescan after a fresh clone picks it up.
			noClone++
			fmt.Printf("Skipping %s (%s): no local clone of project %s found — archived only. Run `agent-sync index` after cloning.\n",
				ex.SessionID, title, ex.Key)
			continue
		}

		paths := make([]string, 0, len(candidates))
		for _, c := range candidates {
			paths = append(paths, c.LocalPath)
		}
		s := &session.Session{
			ID:           ex.SessionID,
			Tool:         session.ToolOpenCode,
			CanonicalKey: session.CanonicalKey(ex.Key),
			PayloadPath:  ex.ExportPath,
		}
		fmt.Printf("Writing back %s (%s) to %d clone(s)...\n", ex.SessionID, title, len(paths))
		res := ad.BroadcastWriteBack(s, paths)
		reportBroadcast(res, ex.SessionID, ex.Key)
		if len(res.Imported) > 0 {
			meta.Imported[ex.SessionID] = time.Now().UTC().Format(time.RFC3339)
			imported++
		}
	}

	if err := repo.WriteMeta(meta); err != nil {
		return fmt.Errorf("write .sync-meta.json: %w", err)
	}

	if imported == 0 {
		if noClone > 0 {
			fmt.Printf("No sessions written back (%d had no local clone; configure [repoindex] roots and run `agent-sync index`).\n", noClone)
		} else {
			fmt.Println("No new sessions to write back.")
		}
	} else {
		fmt.Printf("Wrote back %d session(s).\n", imported)
		// Commit the .sync-meta.json update so other devices know these were
		// received.
		ts := time.Now().UTC().Format(time.RFC3339)
		if _, err := repo.Commit("receive", "sync-meta", ts); err != nil {
			if !strings.Contains(err.Error(), "nothing to commit") {
				fmt.Fprintf(os.Stderr, "warn: could not commit meta update: %v\n", err)
			}
		}
	}
	return nil
}

// reportBroadcast prints the write-back outcome, explicitly logging the
// degraded case (SPEC-DOC.md §4.1, AGENTS.md invariant #8).
func reportBroadcast(res opencode.BroadcastResult, sessionID, key string) {
	for _, b := range res.Busy {
		fmt.Printf("  busy: %s — opencode running there, skipped (retry on next pull)\n", b)
	}
	for _, i := range res.Imported {
		fmt.Printf("  imported into %s\n", i)
	}
	if res.Degraded {
		fmt.Printf("  WARNING: %s imported into %d clones, but OpenCode's session↔project model is one-to-one — "+
			"the session is resumable in the LAST-imported clone only, not all of them (%s).\n",
			sessionID, len(res.Imported), key)
	}
	if len(res.Imported) == 0 && len(res.Busy) == 0 {
		fmt.Printf("  no clone could be written back for %s\n", sessionID)
	}
}

// openRepoIndex opens the repo-index cache DB.
func openRepoIndex() (*repoindex.DB, error) {
	p, err := repoindex.DefaultPath()
	if err != nil {
		return nil, err
	}
	return repoindex.Open(p)
}

type exportRef struct {
	SessionID      string
	Key            string
	ExportPath     string
	ImportMetaPath string
	HasMeta        bool
}

// findExports walks <repo>/opencode/*/export/*.json and returns references.
func findExports(repoPath string) ([]exportRef, error) {
	base := filepath.Join(repoPath, "opencode")
	var refs []exportRef
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") || !strings.Contains(path, string(filepath.Separator)+"export"+string(filepath.Separator)) {
			return nil
		}
		sessionID := strings.TrimSuffix(d.Name(), ".json")
		key := filepath.Base(filepath.Dir(filepath.Dir(path)))
		metaPath := filepath.Join(filepath.Dir(filepath.Dir(path)), "import-meta", d.Name())
		_, metaErr := os.Stat(metaPath)
		refs = append(refs, exportRef{
			SessionID:      sessionID,
			Key:            key,
			ExportPath:     path,
			ImportMetaPath: metaPath,
			HasMeta:        metaErr == nil,
		})
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].SessionID < refs[j].SessionID })
	return refs, nil
}
