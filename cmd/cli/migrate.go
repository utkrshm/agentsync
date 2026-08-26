package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/revision"
	"agentsync/internal/syncrepo"
)

const migrateUsage = `agent-sync migrate-layout — move legacy exports into the immutable revisions layout

Usage:
  agent-sync migrate-layout [--dry-run]

Re-stores every legacy flat-layout artifact,
opencode/<key>/export/<session-id>.json, as an immutable revision:

  opencode/<key>/sessions/<session-id>/revisions/<digest>.json
  opencode/<key>/sessions/<session-id>/revisions/<digest>.meta.json

Each payload is validated before anything is written; files failing
validation are reported and left untouched — never deleted. The legacy
payload and its import-meta sidecar are removed only after the revision
landed successfully. The sidecar records status "migrated" with the
capture time taken from the legacy file's modification time; source
device stays empty because legacy artifacts carry no provenance. All
moves land in ONE commit; git history retains the old paths. Re-running
is a no-op once nothing legacy remains.

Freshly joined devices whose repo contains ONLY legacy artifacts are
migrated automatically by send/receive/resume with a one-line notice.
Mixed-state repos (both layouts populated) always require this explicit
command — no guessing mid-transition.

  --dry-run    print what would be migrated; change nothing
`

// layoutState classifies which storage layouts a sync repo populates
// (docs/hardening-plan.md WS-B "Migration semantics").
type layoutState int

const (
	layoutEmpty         layoutState = iota // nothing stored yet
	layoutLegacyOnly                       // legacy exports only → safe to auto-migrate on first touch
	layoutMixed                            // both layouts populated → explicit command required
	layoutRevisionsOnly                    // revisions layout only → nothing to do
)

// classifyLayout maps artifact counts to a layout state: a repo holding ONLY
// legacy exports migrates silently when a new device first touches it, a MIXED
// repo must go through the explicit migrate-layout command, everything else
// needs no action at all.
func classifyLayout(legacyCount, sessionsDirs int) layoutState {
	switch {
	case legacyCount == 0 && sessionsDirs == 0:
		return layoutEmpty
	case sessionsDirs == 0:
		return layoutLegacyOnly
	case legacyCount == 0:
		return layoutRevisionsOnly
	default:
		return layoutMixed
	}
}

// legacyExport identifies one candidate artifact in the legacy flat layout.
type legacyExport struct {
	Key        string
	SessionID  string
	Payload    string // absolute path of export/<sid>.json
	ImportMeta string // absolute path of import-meta/<sid>.json (may not exist)
}

// scanLegacyExports lists every opencode/<key>/export/<session-id>.json under
// root, sorted deterministically by (key, session id). A missing opencode/
// tree yields no candidates, not an error.
func scanLegacyExports(root string) ([]legacyExport, error) {
	base := filepath.Join(root, "opencode")
	roots, err := storageRoots(root)
	if err != nil {
		return nil, err
	}
	var out []legacyExport
	for _, kr := range roots {
		exportDir := filepath.Join(kr.Path, "export")
		files, err := os.ReadDir(exportDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue // revisions-only root
			}
			return nil, err
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".meta.json") {
				continue
			}
			out = append(out, legacyExport{
				Key:        kr.Key,
				SessionID:  strings.TrimSuffix(name, ".json"),
				Payload:    filepath.Join(exportDir, name),
				ImportMeta: filepath.Join(base, kr.Key, "import-meta", name),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out, nil
}

// countSessionsDirs counts opencode/<key>/sessions directories under root.
// Any such directory means the revisions layout is in use somewhere.
func countSessionsDirs(root string) (int, error) {
	roots, err := storageRoots(root)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, kr := range roots {
		if fi, err := os.Stat(filepath.Join(kr.Path, "sessions")); err == nil && fi.IsDir() {
			n++
		}
	}
	return n, nil
}

// relToRoot renders abs relative to root for user-facing messages, falling
// back to the absolute path outside/below root.
func relToRoot(root, abs string) string {
	if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(abs)
}

// runMigration validates and moves every legacy export into the revisions
// layout under repo.Path. Per-file plan lines (dry-run) and invalid-artifact
// warnings print unless quiet; a non-quiet dry run ends with a
// "would migrate N artifact(s)" count summary. It performs NO commit;
// callers decide whether one is due. dryRun touches nothing on disk. Returns
// how many artifacts were fully handled: payload written (or already present
// identically) AND legacy copies removed. Unexpected IO/digest errors
// accumulate and abort committing upstream while leaving already-migrated
// files in place (idempotent re-runs finish the rest); validation failures
// merely skip their file, never delete it.
func runMigration(repo *syncrepo.Repo, dryRun, quiet bool) (int, error) {
	exports, err := scanLegacyExports(repo.Path)
	if err != nil {
		return 0, err
	}
	migrated := 0
	var hardErrs []error
	for _, le := range exports {
		data, err := os.ReadFile(le.Payload)
		if err != nil {
			hardErrs = append(hardErrs, fmt.Errorf("read %s: %w", le.Payload, err))
			continue
		}
		info, err := opencode.ValidateExport(data, le.SessionID)
		if err != nil {
			if !quiet {
				fmt.Fprintf(os.Stderr, "skipping invalid legacy export: %s (%v)\n",
					relToRoot(repo.Path, le.Payload), err)
			}
			continue // never delete an artifact that failed validation
		}
		digest := revision.DigestBytes(data)
		targetRel := revision.Path(le.Key, le.SessionID, digest)

		if dryRun {
			if !quiet {
				fmt.Printf("would migrate %s -> %s\n",
					relToRoot(repo.Path, le.Payload), targetRel)
			}
			migrated++
			continue
		}

		fi, err := os.Stat(le.Payload)
		if err != nil {
			hardErrs = append(hardErrs, fmt.Errorf("stat %s: %w", le.Payload, err))
			continue
		}
		meta := revision.Meta{
			SchemaVersion:     revision.SchemaVersion,
			OriginalSessionID: le.SessionID,
			Digest:            digest,
			SourceDeviceID:    "",
			DeviceAlias:       "",
			CapturedAt:        fi.ModTime().UTC(),
			ProducerVersion:   info.Version,
			Status:            revision.StatusMigrated,
			ProjectID:         info.ProjectID,
			Directory:         info.Directory,
			Title:             info.Title,
		}
		// revision.Write is idempotent for identical content (the "destination
		// exists identical" case skips silently) and hard-errors on any attempt
		// to put different bytes where a digest says they cannot be.
		if _, err := revision.Write(repo.Path, le.Key, le.SessionID, data, meta); err != nil {
			hardErrs = append(hardErrs, fmt.Errorf("migrate %s: %w",
				relToRoot(repo.Path, le.Payload), err))
			continue // legacy pair stays: it is still the only durable copy
		}
		// Success: drop the legacy pair. Missing sidecars are fine; a failed
		// payload removal still counts as not-migrated so the next run retries.
		if err := os.Remove(le.Payload); err != nil && !os.IsNotExist(err) {
			hardErrs = append(hardErrs, fmt.Errorf("remove %s: %w", le.Payload, err))
			continue
		}
		if err := os.Remove(le.ImportMeta); err != nil && !os.IsNotExist(err) {
			hardErrs = append(hardErrs, fmt.Errorf("remove %s: %w", le.ImportMeta, err))
		}
		migrated++
	}
	if dryRun && !quiet {
		fmt.Printf("would migrate %d artifact(s)\n", migrated)
	}
	return migrated, errors.Join(hardErrs...)
}

// applyMigration performs the moves (runMigration) and then, in apply mode
// with something actually migrated, records them as ONE migration commit.
// A run with hard errors commits NOTHING — partial work sits uncommitted in
// the working tree until a clean re-run completes and commits it all.
func applyMigration(repo *syncrepo.Repo, dryRun, quiet bool) (int, error) {
	migrated, err := runMigration(repo, dryRun, quiet)
	if err != nil || dryRun || migrated == 0 {
		// Dry-run never commits; with zero cleanly migrated files there is
		// nothing of ours to record, so skipping Commit also avoids sweeping
		// unrelated pending sync-owned changes into a migration commit.
		return migrated, err
	}
	repo.ValidateArtifact = opencode.CheckArtifactFile
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, cerr := repo.Commit("opencode", "migrate-layout", ts); cerr != nil {
		if errors.Is(cerr, syncrepo.ErrNoChanges) {
			return migrated, nil // treat as success, not failure
		}
		return migrated, fmt.Errorf("commit migration: %w", cerr)
	}
	return migrated, nil
}

// cmdMigrateLayout implements agent-sync migrate-layout [--dry-run].
func cmdMigrateLayout(args []string) error {
	dryRun := false
	for _, arg := range args {
		switch arg {
		case "--dry-run":
			dryRun = true
		default:
			return fmt.Errorf("unknown migrate-layout flag %q", arg)
		}
	}
	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	repo := syncrepo.Open(cfg.Sync.RepoPath)
	if !repo.Exists() {
		return fmt.Errorf("sync repo not initialized at %s — run `agent-sync init`", cfg.Sync.RepoPath)
	}
	n, err := applyMigration(repo, dryRun, false)
	if err != nil {
		return err
	}
	if dryRun {
		// Plan lines and the count summary were already printed by
		// runMigration; printing again here would duplicate them.
		return nil
	}
	fmt.Printf("migrated %d artifact(s)\n", n)
	return nil
}

// Fresh-join migration guard: resume composes receive, so within one CLI
// invocation chain the layout decision must run exactly once — no duplicate
// notice output, no double migration. Package-level state keeps command
// signatures unchanged; tests reset it via resetMigrationGuard.
var (
	migrationGuard    sync.Once
	migrationGuardErr error
)

// resetMigrationGuard clears the once-guard between tests.
func resetMigrationGuard() {
	migrationGuard = sync.Once{}
	migrationGuardErr = nil
}

// migrateIfNeededOnce runs migrateIfNeeded at most once per process, even
// when receive is invoked twice through resume's composed flow.
func migrateIfNeededOnce(repo *syncrepo.Repo) error {
	migrationGuard.Do(func() {
		migrationGuardErr = migrateIfNeeded(repo)
	})
	return migrationGuardErr
}

// migrateIfNeeded implements the locked silent-migration semantics
// (docs/hardening-plan.md WS-B "Migration semantics"): a repo holding ONLY
// legacy artifacts migrates silently, printing exactly ONE notice line; a
// MIXED repo prints one hint pointing at the explicit command and changes
// nothing; anything else is untouched and silent.
func migrateIfNeeded(repo *syncrepo.Repo) error {
	exports, err := scanLegacyExports(repo.Path)
	if err != nil {
		return err
	}
	sessions, err := countSessionsDirs(repo.Path)
	if err != nil {
		return err
	}
	switch classifyLayout(len(exports), sessions) {
	case layoutLegacyOnly:
		n, err := applyMigration(repo, false, true)
		if err != nil {
			return err
		}
		if n > 0 {
			fmt.Printf("migrated %d legacy artifact(s) to revisions layout\n", n)
		}
	case layoutMixed:
		fmt.Printf("hint: run agent-sync migrate-layout to finish migrating %d remaining legacy artifact(s)\n",
			len(exports))
	}
	return nil
}
