package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/canonicalkey"
	"agentsync/internal/deviceid"
	"agentsync/internal/revision"
	"agentsync/internal/syncrepo"
)

const sendUsage = `agent-sync send — export an OpenCode session into the sync repo and push

Usage:
  agent-sync send <session-id>

Runs "opencode export <session-id>", validates the result (complete JSON,
matching session id, non-empty directory/version), then stores it as an
immutable revision artifact:

  opencode/<project-key>/sessions/<session-id>/revisions/<digest>.json
  opencode/<project-key>/sessions/<session-id>/revisions/<digest>.meta.json

where <digest> is the sha256 of the exact export bytes. Re-sending identical
content is a no-op; distinct revisions of the same session are preserved side
by side. Commits (timestamped + versioned) and pushes to origin. Only
opencode/** and .gitignore are ever staged — anything else under the sync dir
is skipped with a warning, never staged or deleted.
Requires "agent-sync init" first.
`

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
	repo.ValidateArtifact = opencode.CheckArtifactFile
	if !repo.Exists() {
		return fmt.Errorf("sync repo not initialized at %s — run `agent-sync init`", cfg.Sync.RepoPath)
	}
	// Fresh-join migration (docs/hardening-plan.md WS-B): a repo holding only
	// legacy exports is migrated silently before this send appends its own
	// revision; a mixed repo just gets a hint. Runs at most once per process.
	if err := migrateIfNeededOnce(repo); err != nil {
		return err
	}

	// Export to a temp file first so a killed export never lands in the repo.
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

	// Read the payload once, validate it before anything durable is written.
	payload, err := os.ReadFile(tmpPath)
	if err != nil {
		return err
	}
	info, err := opencode.ValidateExport(payload, sessionID)
	if err != nil {
		return err
	}
	// Resolve canonical key from the session's source project directory.
	key := canonicalkey.Resolve(info.Directory)

	deviceID, err := deviceid.LoadOrCreate()
	if err != nil {
		return fmt.Errorf("load device id: %w", err)
	}

	// Immutable revisions layout: <sync-repo>/opencode/<key>/sessions/<id>/
	// revisions/<digest>.json plus the .meta.json sidecar. Identical content
	// is an idempotent no-op; different revisions of one session coexist.
	meta := revision.Meta{
		SchemaVersion:     revision.SchemaVersion,
		OriginalSessionID: sessionID,
		Digest:            revision.DigestBytes(payload),
		SourceDeviceID:    deviceID,
		DeviceAlias:       cfg.Sync.DeviceAlias,
		CapturedAt:        time.Now().UTC(),
		ProducerVersion:   info.Version,
		Status:            revision.StatusCaptured,
		ProjectID:         info.ProjectID,
		Directory:         info.Directory,
		Title:             info.Title,
	}
	if _, err := revision.Write(repo.Path, string(key), sessionID, payload, meta); err != nil {
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
		if errors.Is(err, syncrepo.ErrNoChanges) {
			fmt.Println("Already synced; nothing to commit.")
			return nil
		}
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
