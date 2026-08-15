package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/canonicalkey"
	"agentsync/internal/syncrepo"
)

// cmdSend exports an OpenCode session into the sync repo and pushes it.
func cmdSend(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("send requires a session id")
	}
	sessionID := args[0]

	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	repo := syncrepo.Open(cfg.Sync.RepoPath)
	if !repo.Exists() {
		return fmt.Errorf("sync repo not initialized at %s — run `agent-sync init`", cfg.Sync.RepoPath)
	}

	// Export to a temp file first so we can read its canonical key + info.
	tmp, err := os.CreateTemp("", "agentsync-export-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if err := opencode.Export(sessionID, tmpPath); err != nil {
		return err
	}

	info, err := readExportInfoFrom(tmpPath)
	if err != nil {
		return err
	}
	// Resolve canonical key from the session's source project directory.
	key := canonicalkey.Resolve(info.Directory)

	// Layout: <sync-repo>/opencode/<key>/export/<id>.json
	rel := filepath.Join("opencode", string(key), "export", sessionID+".json")
	dest := filepath.Join(repo.Path, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	if err := copyFile(tmpPath, dest); err != nil {
		return err
	}

	// Write import-meta for the receive-side patch.
	metaPath := filepath.Join(repo.Path, "opencode", string(key), "import-meta", sessionID+".json")
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o700); err != nil {
		return err
	}
	if err := writeImportMeta(metaPath, info); err != nil {
		return err
	}

	// Update .sync-meta.json so device state is tracked/committed.
	if err := repo.TouchMeta(); err != nil {
		return err
	}

	// Timestamped + versioned commit.
	ts := time.Now().UTC().Format(time.RFC3339)
	version, err := repo.Commit("opencode", sessionID, ts)
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	fmt.Printf("Committed v%d: sync: opencode %s v%d %s\n", version, sessionID, version, ts)

	if cfg.Sync.Remote == "" {
		fmt.Println("No remote configured — commit is local-only. Run `agent-sync init` to add one.")
		return nil
	}
	if err := repo.Push(); err != nil {
		return fmt.Errorf("push: %w (commit was made locally; fix the remote and re-run `agent-sync push` or retry)", err)
	}
	fmt.Printf("Pushed to %s\n", cfg.Sync.Remote)
	return nil
}
