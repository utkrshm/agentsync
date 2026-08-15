package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/syncrepo"
)

// cmdReceive pulls the sync repo and imports any new OpenCode sessions into
// the local OpenCode data dir, applying the project_id/directory patch.
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
		if err := repo.PullFastForward(); err != nil {
			return fmt.Errorf("sync-repo pull: %w", err)
		}
	}

	// Safety guard: refuse to write to OpenCode storage while it runs for
	// this user (AGENTS.md invariant #2).
	running, err := opencode.IsToolRunning("")
	if err != nil {
		return fmt.Errorf("check opencode running: %w", err)
	}
	if running {
		return fmt.Errorf("refusing to receive: opencode is currently running for your user. Close it, then re-run `agent-sync receive`")
	}

	meta, err := repo.ReadMeta()
	if err != nil {
		meta = syncrepo.SyncMeta{SchemaVersion: 1, DeviceID: "dev-" + fmt.Sprint(time.Now().UnixNano()), Imported: map[string]string{}}
	}
	if meta.Imported == nil {
		meta.Imported = map[string]string{}
	}

	// Discover export files not yet imported.
	exports, err := findExports(repo.Path)
	if err != nil {
		return err
	}

	imported := 0
	for _, ex := range exports {
		if _, done := meta.Imported[ex.SessionID]; done {
			continue
		}
		fmt.Printf("Importing %s (%s)...\n", ex.SessionID, ex.Key)
		if err := opencode.Import(ex.ExportPath); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: import failed for %s: %v (skipping)\n", ex.SessionID, err)
			continue
		}
		// Apply patch using the import-meta (target dir + project info).
		targetDir := ex.Key // fallback: the canonical key is not a path
		if ex.HasMeta {
			if im, err := readImportMeta(ex.ImportMetaPath); err == nil && im.Directory != "" {
				targetDir = im.Directory
			}
		}
		if err := opencode.PatchImport(ex.ExportPath, targetDir, string(ex.Key)); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: patch failed for %s: %v\n", ex.SessionID, err)
		}
		meta.Imported[ex.SessionID] = time.Now().UTC().Format(time.RFC3339)
		imported++
	}

	if err := repo.WriteMeta(meta); err != nil {
		return fmt.Errorf("write .sync-meta.json: %w", err)
	}

	if imported == 0 {
		fmt.Println("No new sessions to import.")
	} else {
		fmt.Printf("Imported %d session(s).\n", imported)
		// Commit the .sync-meta.json update so other devices know these were
		// received (and to keep the sync repo state consistent).
		ts := time.Now().UTC().Format(time.RFC3339)
		if _, err := repo.Commit("receive", "sync-meta", ts); err != nil {
			// It's fine if there's nothing to commit.
			if !strings.Contains(err.Error(), "nothing to commit") {
				fmt.Fprintf(os.Stderr, "warn: could not commit meta update: %v\n", err)
			}
		}
	}
	return nil
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
