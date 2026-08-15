package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/receivestate"
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
	dryRun := false
	for _, arg := range args {
		switch arg {
		case "--dry-run":
			dryRun = true
		case "--help", "-h":
			fmt.Println("agent-sync receive [--dry-run] — pull and restore pending OpenCode sessions")
			return nil
		default:
			return fmt.Errorf("unknown receive flag %q", arg)
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
	if cfg.Sync.Remote != "" {
		if err := repo.PullForced(); err != nil {
			return fmt.Errorf("sync-repo pull: %w", err)
		}
	}
	idx, err := openRepoIndex()
	if err != nil {
		return err
	}
	statePath, err := receivestate.DefaultPath()
	if err != nil {
		return err
	}
	local, err := receivestate.Open(statePath)
	if err != nil {
		return err
	}
	exports, err := findExports(repo.Path)
	if err != nil {
		return err
	}
	ad := opencode.NewAdapter()
	attempted := 0
	for _, ex := range exports {
		digest, err := artifactDigest(ex.ExportPath)
		if err != nil {
			return fmt.Errorf("digest %s: %w", ex.SessionID, err)
		}
		title := ""
		if im, err := readImportMeta(ex.ImportMetaPath); err == nil {
			title = im.Title
		}
		candidates, err := idx.Resolve(session.CanonicalKey(ex.Key))
		if err != nil {
			fmt.Fprintf(os.Stderr, "receive: action=resolve session=%s status=error error=%q\n", ex.SessionID, err)
			continue
		}
		if len(candidates) == 0 {
			fmt.Printf("Archived only: %s (%s) has no local clone for %s. Run agent-sync index after cloning.\n", ex.SessionID, title, ex.Key)
			continue
		}
		paths := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			if err := repoindex.ValidateCandidate(session.CanonicalKey(ex.Key), candidate.LocalPath); err != nil {
				fmt.Fprintf(os.Stderr, "receive: action=revalidate session=%s path=%q status=skip error=%q\n", ex.SessionID, candidate.LocalPath, err)
				if !dryRun {
					_ = local.Put(receivestate.Outcome{ArtifactDigest: digest, SessionID: ex.SessionID, CandidatePath: candidate.LocalPath, Status: receivestate.StatusFailed, LastError: err.Error()})
				}
				continue
			}
			previous, ok, err := local.Get(digest, candidate.LocalPath)
			if err != nil {
				return err
			}
			if ok && (previous.Status == receivestate.StatusVerified || previous.Status == receivestate.StatusDegraded) {
				fmt.Printf("Already processed: %s at %s (%s).\n", ex.SessionID, candidate.LocalPath, previous.Status)
				continue
			}
			paths = append(paths, candidate.LocalPath)
		}
		if len(paths) == 0 {
			continue
		}
		s := &session.Session{ID: ex.SessionID, Tool: session.ToolOpenCode, CanonicalKey: session.CanonicalKey(ex.Key), PayloadPath: ex.ExportPath}
		if err := ad.ValidateArtifact(s); err != nil {
			fmt.Fprintf(os.Stderr, "receive: action=validate session=%s status=failed error=%q\n", ex.SessionID, err)
			if !dryRun {
				for _, path := range paths {
					_ = local.Put(receivestate.Outcome{ArtifactDigest: digest, SessionID: ex.SessionID, CandidatePath: path, Status: receivestate.StatusFailed, LastError: err.Error()})
				}
			}
			continue
		}
		if dryRun {
			fmt.Printf("DRY RUN: would write back %s (%s) to %d validated clone(s).\n", ex.SessionID, title, len(paths))
			continue
		}
		attempted++
		fmt.Printf("Writing back %s (%s) to %d clone(s)...\n", ex.SessionID, title, len(paths))
		res := ad.BroadcastWriteBack(s, paths)
		recordBroadcast(local, digest, ex.SessionID, res)
		reportBroadcast(res, ex.SessionID, ex.Key)
	}
	if dryRun {
		fmt.Println("Dry run complete; no OpenCode or local receive state was changed.")
	} else if attempted == 0 {
		fmt.Println("No pending sessions required write-back.")
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
		fmt.Printf("  verified in %s\n", i)
	}
	for _, failure := range res.Failed {
		fmt.Printf("  failed: %s — %s (will retry)\n", failure.Path, failure.Error)
	}
	if res.Degraded {
		fmt.Printf("  WARNING: %s imported into %d clones, but OpenCode's session↔project model is one-to-one — "+
			"the session is resumable in the LAST-imported clone only, not all of them (%s).\n",
			sessionID, len(res.Imported), key)
	}
	if len(res.Imported) == 0 && len(res.Busy) == 0 && len(res.Failed) == 0 {
		fmt.Printf("  no clone could be written back for %s\n", sessionID)
	}
}

func recordBroadcast(local *receivestate.Store, digest, sessionID string, res opencode.BroadcastResult) {
	for _, path := range res.Busy {
		if err := local.Put(receivestate.Outcome{ArtifactDigest: digest, SessionID: sessionID, CandidatePath: path, Status: receivestate.StatusBusy}); err != nil {
			fmt.Fprintf(os.Stderr, "receive: action=record path=%q status=error error=%q\n", path, err)
		}
	}
	for _, failure := range res.Failed {
		if err := local.Put(receivestate.Outcome{ArtifactDigest: digest, SessionID: sessionID, CandidatePath: failure.Path, Status: receivestate.StatusFailed, LastError: failure.Error}); err != nil {
			fmt.Fprintf(os.Stderr, "receive: action=record path=%q status=error error=%q\n", failure.Path, err)
		}
	}
	for i, path := range res.Imported {
		status := receivestate.StatusVerified
		if res.Degraded && i < len(res.Imported)-1 {
			status = receivestate.StatusDegraded
		}
		if err := local.Put(receivestate.Outcome{ArtifactDigest: digest, SessionID: sessionID, CandidatePath: path, Status: status}); err != nil {
			fmt.Fprintf(os.Stderr, "receive: action=record path=%q status=error error=%q\n", path, err)
		}
	}
}

func artifactDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
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
