package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/canonicalkey"
	"agentsync/internal/deviceid"
	"agentsync/internal/receivestate"
	"agentsync/internal/repoindex"
	"agentsync/internal/revision"
	"agentsync/internal/syncrepo"
)

const syncUsage = `agent-sync sync — unified per-project synchronization

Usage:
  agent-sync sync [--dry-run] <dir>

The whole synchronization lifecycle scoped strictly to the one project at
<dir>, composing the existing pieces:

  1. resolve <dir>'s canonical key (alias-aware); _unmapped folders are
     offered an interactive alias pin first (a permanent identity means
     nothing can ever be orphaned) and otherwise warn-and-continue
  2. fail-closed pull of the sync repo (diverged history refuses loudly;
     foreign-untracked conflicts propagate verbatim)
  3. PUSH: every local OpenCode session of THIS project is exported,
     validated, digest-deduped against what is already stored, and written
     as immutable revisions — committed as one batch, then pushed
  4. WRITE-BACK: every stored session group of THIS project runs through
     the shared guarded pipeline (restoreRevision): clean heads missing
     locally are restored; conflicts print their report and a numbered
     heads picker ([S]kip leaves everything preserved and detached)

Close-out reports pushed/imported/resolved/skipped honestly; skipped
sessions always remain preserved and restorable via "agent-sync recover".
Re-running sync is a clean no-op: stored digests dedup pushes, verified
outcomes skip re-imports.

  --dry-run    print the identical plan and counts; ZERO mutations —
               including not writing an alias pin ("would prompt to pin")
`

// Injectables for hermetic tests (AGENTS.md conventions: thin IO, tested
// decisions). Production defaults touch the real environment.
var (
	stdinIsTTY = func() bool {
		fi, err := os.Stdin.Stat()
		return err == nil && fi.Mode()&os.ModeCharDevice != 0
	}
	readPromptLine = func(prompt string) string {
		fmt.Print(prompt)
		sc := bufio.NewScanner(os.Stdin)
		if !sc.Scan() {
			return ""
		}
		return strings.TrimSpace(sc.Text())
	}
	projectAliasesPath = func() (string, error) {
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "agent-sync", "project-aliases.toml"), nil
	}
)

// opencodeSessionRow is one `session` table row we care about: id plus its
// source project directory (canonical keys resolve FROM that directory, the
// same way the daemon's deny-policy pass resolves each row).
type opencodeSessionRow struct {
	ID        string `json:"id"`
	Directory string `json:"directory"`
}

// localSessionIDs filters discovered rows down to sessions belonging to key.
// Resolving PER ROW (rather than trusting a coarse prefix) keeps historical
// rows landing under their pinned/aliased key exactly like OnChange does.
func localSessionIDs(rows []opencodeSessionRow, key string) []string {
	var out []string
	for _, r := range rows {
		if r.ID == "" {
			continue
		}
		if string(canonicalkey.Resolve(r.Directory)) == key {
			out = append(out, r.ID)
		}
	}
	sort.Strings(out)
	return out
}

// appendAliasPin records one alias mapping in the canonical parser's own
// format: `"<path>" = "<alias>"` (canonicalkey.alias() quote-strips both
// sides and skips blank/# lines). Identical re-pins are a no-op; existing
// content (hand-made or managed) rides along byte-honest.
func appendAliasPin(path, localPath, alias string) error {
	alias = strings.TrimSpace(strings.Trim(strings.TrimSpace(alias), `"`))
	if alias == "" {
		return fmt.Errorf("empty alias")
	}
	entry := fmt.Sprintf("%q = %q\n", filepath.Clean(localPath), alias)

	existing := ""
	if data, err := os.ReadFile(path); err == nil {
		existing = string(data)
		if strings.Contains(existing, "\n"+entry) || existing == entry {
			return nil // already pinned identically
		}
	}

	var b strings.Builder
	if existing == "" {
		b.WriteString("# Created/extended by `agent-sync sync`: \"<path>\" = \"<alias>\" identity pins.\n")
	} else {
		b.WriteString(existing)
		if !strings.HasSuffix(existing, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString(entry)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// parseHeadChoice interprets one answer of the conflict heads picker:
// numbers select a head (1-based), s/S requests skipping (everything stays
// preserved/archive-only), blank aborts. Garbage and out-of-range numbers
// are errors.
func parseHeadChoice(input string, headCount int) (skip, abort bool, pick int, err error) {
	switch input {
	case "":
		return false, true, 0, nil
	case "s", "S":
		return true, false, 0, nil
	}
	n, cerr := strconv.Atoi(input) // whole-string numbers only: "1x" must fail, not silently parse as 1
	if cerr != nil || n < 1 || n > headCount {
		return false, false, 0, fmt.Errorf("invalid selection %q — want 1-%d, s, or blank", input, headCount)
	}
	return false, false, n, nil
}

// cmdSync implements agent-sync sync [--dry-run] <dir>: one project, one
// command — pinned identity, fail-closed pull, batched push, guarded
// write-back, honest close-out (docs/sync-rekey-collapse-plan.md Step 4).
func cmdSync(args []string) error {
	dryRun := false
	var dir string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			dryRun = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown sync flag %q", args[i])
			}
			if dir != "" {
				return fmt.Errorf("unexpected argument %q — sync takes exactly one project directory", args[i])
			}
			dir = args[i]
		}
	}
	if dir == "" {
		return fmt.Errorf("sync requires a project directory")
	}

	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	repo := syncrepo.Open(cfg.Sync.RepoPath)
	if !repo.Exists() {
		return fmt.Errorf("sync repo not initialized at %s — run `agent-sync init`", cfg.Sync.RepoPath)
	}
	repo.ValidateArtifact = opencode.CheckArtifactFile

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	key := string(canonicalkey.Resolve(absDir))
	// Identity crisis handling FIRST: nothing may be pushed/pulled against a
	// folder that silently points at an _unmapped island until the user had
	// one chance to pin a permanent alias (spec §Step 4, unmapped block).
	if strings.HasPrefix(key, "_unmapped/") {
		key, err = handleUnmappedIdentity(absDir, key, dryRun)
		if err != nil {
			return err
		}
	}

	ad := buildWriteBackAdapter(cfg)

	if cfg.Sync.Remote != "" {
		if err := repo.PullForced(); err != nil {
			return err // foreign-untracked/divergence messages carry remediation already
		}
	}

	// ---- discover local sessions for THIS key ------------------------------
	rows, err := querySessionRows(ad)
	if err != nil {
		return err
	}
	localIDs := localSessionIDs(rows, key)
	// Device-wide session presence for write-back decisions: a clean stored
	// head is only imported into <dir> when the device's OpenCode does not
	// know the session AT ALL — knowing it in another clone of this project
	// (or any other) still means it exists locally.
	presentLocally := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.ID != "" {
			presentLocally[r.ID] = true
		}
	}

	// ---- push phase ---------------------------------------------------------
	storeDigests := make(map[string]map[string]bool)
	refs, err := findRevisions(repo.Path)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if ref.Key != key {
			continue
		}
		if storeDigests[ref.SessionID] == nil {
			storeDigests[ref.SessionID] = make(map[string]bool)
		}
		storeDigests[ref.SessionID][ref.Digest] = true
	}

	deviceID, err := deviceid.LoadOrCreate()
	if err != nil {
		return fmt.Errorf("load device id: %w", err)
	}
	newRevs := 0
	touched := map[string]bool{}
	for _, sid := range localIDs {
		tmp, terr := os.CreateTemp("", "agentsync-export-*.json")
		if terr != nil {
			return terr
		}
		tmpPath := tmp.Name()
		tmp.Close()
		defer os.Remove(tmpPath)

		if xerr := ad.Export(sid, tmpPath); xerr != nil {
			fmt.Fprintf(os.Stderr, "sync: export %s failed — skipping this push; session stays unacknowledged: %v\n", sid, xerr)
			continue
		}
		payload, rerr := os.ReadFile(tmpPath)
		if rerr != nil {
			return rerr
		}
		info, verr := opencode.ValidateExport(payload, sid)
		if verr != nil {
			fmt.Fprintf(os.Stderr, "sync: skipping invalid export %s (no bytes written to storage): %v\n", sid, verr)
			continue
		}
		digest := revision.DigestBytes(payload)
		if storeDigests[sid] != nil && storeDigests[sid][digest] {
			continue // already stored — the digest IS the idempotency ticket
		}
		if !dryRun {
			meta := revision.Meta{
				SchemaVersion:     revision.SchemaVersion,
				OriginalSessionID: sid,
				Digest:            digest,
				SourceDeviceID:    deviceID,
				DeviceAlias:       cfg.Sync.DeviceAlias,
				CapturedAt:        time.Now().UTC(),
				ProducerVersion:   info.Version,
				Status:            revision.StatusCaptured,
				ProjectID:         info.ProjectID,
				Directory:         info.Directory,
				Title:             info.Title,
			}
			if _, werr := revision.Write(repo.Path, key, sid, payload, meta); werr != nil {
				return werr
			}
		}
		newRevs++
		touched[sid] = true
	}

	if len(touched) > 0 {
		if dryRun {
			fmt.Printf("DRY RUN: pushing %d session(s) (%d new revision(s)) — would commit once and push.\n",
				len(touched), newRevs)
		} else {
			// Summary BEFORE committing: visibility without questions.
			fmt.Printf("pushing %d session(s) (%d new revision(s))\n", len(touched), newRevs)
			ts := time.Now().UTC().Format(time.RFC3339)
			version, cerr := repo.Commit("opencode", "sync-batch", ts)
			switch {
			case errors.Is(cerr, syncrepo.ErrNoChanges):
				fmt.Println("nothing new to push")
			case cerr != nil:
				return fmt.Errorf("commit: %w", cerr)
			default:
				fmt.Printf("Committed v%d: sync: opencode sync-batch v%d %s\n", version, version, ts)
				if cfg.Sync.Remote == "" {
					fmt.Println("No remote configured — commit is local-only.")
				} else if perr := repo.Push(); perr != nil {
					return fmt.Errorf("push: %w (commit was made locally; retry when the remote recovers)", perr)
				} else {
					fmt.Printf("Pushed to %s\n", cfg.Sync.Remote)
				}
			}
		}
	} else {
		fmt.Println("nothing new to push")
	}
	pushedSessions, pushedRevs := len(touched), newRevs

	// ---- write-back phase (remote → this directory) --------------------------
	targets := []string{absDir}
	refs, ferr2 := findRevisions(repo.Path)
	if ferr2 != nil {
		return ferr2
	}
	groups := buildConflictGroups(refs)

	var st *receivestate.Store
	var idx *repoindex.DB
	if !dryRun {
		// Opening these creates their cache directories — a mutation, so a
		// dry run keeps them closed entirely.
		statePath, serr := receivestate.DefaultPath()
		if serr != nil {
			return serr
		}
		st, serr = receivestate.Open(statePath)
		if serr != nil {
			return serr
		}
		idx, serr = openRepoIndex()
		if serr != nil {
			return serr
		}
	}

	imported, resolved, skipped := 0, 0, 0
	var unresolved []string
	for _, gi := range groups {
		if gi.Group.Key != key {
			continue
		}
		sessionID := gi.Group.SessionID

		if gi.Group.Conflicted {
			fmt.Println()
			for _, line := range conflictReport(gi) {
				fmt.Println(line)
			}
			if dryRun {
				fmt.Printf("DRY RUN: would prompt to choose among %d preserved head(s) — projected as skipped.\n", len(gi.Group.Heads))
				unresolved = append(unresolved, sessionID)
				skipped++
				continue
			}
			chosen, skip, abort, perr := chooseHeadInteractive(gi)
			if perr != nil {
				return perr
			}
			switch {
			case abort:
				fmt.Printf("skipped (aborted): %s — nothing was changed for this session.\n", sessionID)
				unresolved = append(unresolved, sessionID)
				skipped++
			case skip:
				// Everything stays UNATTACHED locally: archive-only per
				// head×target, recoverable later, never auto-restored.
				for _, h := range gi.Group.Heads {
					recordArchiveOnlyForPaths(st, h.Digest, sessionID, targets)
				}
				unresolved = append(unresolved, sessionID)
				skipped++
			default:
				okRestored, rerr := restoreRevision(repo, idx, st, ad, gi, chosen, targets)
				if rerr != nil {
					return rerr
				}
				if okRestored {
					imported++
					resolved++
				} else {
					fmt.Printf("not resolved: %s — every target was busy or failed (reported above); run sync again later.\n", sessionID)
					unresolved = append(unresolved, sessionID)
					skipped++
				}
			}
			continue
		}

		// Clean single-head group: import ONLY when the device's OpenCode
		// does not know this session locally and a previous run hasn't
		// already verified it here (repeat runs are a clean no-op).
		head := gi.Group.Heads[0]
		if presentLocally[sessionID] {
			continue
		}
		alreadyVerified := false
		if st != nil {
			if prev, found, gerr := st.Get(head.Digest, absDir); gerr == nil && found &&
				(prev.Status == receivestate.StatusVerified || prev.Status == receivestate.StatusDegraded) {
				alreadyVerified = true
			}
		}
		if alreadyVerified {
			continue
		}
		title := ""
		if pref, pok := primaryRef(gi.Refs, head.Digest); pok && pref.Meta != nil {
			title = pref.Meta.Title
		}
		if dryRun {
			fmt.Printf("DRY RUN: would write back %s (%s) into %s.\n", sessionID, title, absDir)
			imported++
			continue
		}
		okRestored, rerr := restoreRevision(repo, idx, st, ad, gi, head.Digest, targets)
		if rerr != nil {
			return rerr
		}
		if okRestored {
			imported++
		} else {
			fmt.Printf("not imported: %s — every target was busy or failed (reported above); run sync again later.\n", sessionID)
		}
	}

	// ---- close-out ------------------------------------------------------------
	fmt.Printf("\ndone: pushed %d session(s)/%d revision(s), imported %d, resolved %d conflict(s), skipped %d\n",
		pushedSessions, pushedRevs, imported, resolved, skipped)
	sort.Strings(unresolved)
	for _, sid := range unresolved {
		fmt.Printf("skipped session(s) remain preserved; `agent-sync recover %s` anytime\n", sid)
	}
	if dryRun {
		fmt.Println("Dry run complete; nothing was mutated (including any alias pin).")
	}
	return nil
}

// handleUnmappedIdentity gives an _unmapped folder exactly one chance to gain
// a permanent identity: TTY runs prompt for an alias pin (decline warns and
// continues), non-TTY runs and --dry-run explain the island risk without any
// interaction or file writes. Returns the possibly-improved key.
func handleUnmappedIdentity(absDir, currentKey string, dryRun bool) (string, error) {
	const fixHint = "give the folder a stable identity: clone it from a repo with a remote origin, or pin an alias in ~/.config/agent-sync/project-aliases.toml"
	if dryRun {
		fmt.Printf("warning: %s resolves to %q — sessions here risk becoming an orphaned island.\n%s\n",
			absDir, currentKey, fixHint)
		fmt.Printf("DRY RUN: would prompt to pin an alias for %s.\n", absDir)
		return currentKey, nil
	}
	if !stdinIsTTY() {
		fmt.Printf("warning: %s resolves to %q — sessions here risk becoming an orphaned island.\n%s\n",
			absDir, currentKey, fixHint)
		return currentKey, nil
	}
	base := filepath.Base(absDir)
	answer := readPromptLine(fmt.Sprintf(
		"%s resolves to %q — the folder has no stable identity\npin an alias for %s [%s]: ",
		absDir, currentKey, absDir, base))
	answer = strings.TrimSpace(answer)
	if answer == "" {
		fmt.Printf("warn: no alias pinned — continuing under %q (island risk acknowledged).\n", currentKey)
		return currentKey, nil
	}
	path, perr := projectAliasesPath()
	if perr != nil {
		return "", perr
	}
	if aerr := appendAliasPin(path, absDir, answer); aerr != nil {
		return "", fmt.Errorf("write alias pin: %w", aerr)
	}
	newKey := string(canonicalkey.Resolve(absDir))
	if strings.HasPrefix(newKey, "_unmapped/") {
		return "", fmt.Errorf("alias %q did not resolve %s away from _unmapped — refusing to continue under a guessed identity", answer, absDir)
	}
	fmt.Println("identity pinned; nothing can be orphaned anymore")
	return newKey, nil
}

// querySessionRows runs the narrow metadata SELECT through the tool's own
// interface (invariant #4) and decodes id/directory rows.
func querySessionRows(ad *opencode.Adapter) ([]opencodeSessionRow, error) {
	raw, err := ad.QueryRecent(`SELECT id, directory FROM session`)
	if err != nil {
		return nil, fmt.Errorf("query opencode sessions: %w", err)
	}
	var rows []opencodeSessionRow
	if uerr := json.Unmarshal(raw, &rows); uerr != nil {
		return nil, fmt.Errorf("parse session rows: %w", uerr)
	}
	return rows, nil
}

// chooseHeadInteractive presents the numbered heads picker WITH a skip exit,
// used by sync's conflict blocks. Numbers activate a head via the shared
// guarded pipeline; S leaves everything preserved; blank aborts.
func chooseHeadInteractive(gi conflictGroup) (digest string, skip, abort bool, err error) {
	header := fmt.Sprintf("\n%s has %d preserved revision head(s) — choose one to make active:",
		gi.Group.SessionID, len(gi.Group.Heads))
	if gi.Group.Superseded > 0 {
		header += fmt.Sprintf(" (+%d older superseded — reachable via `recover --revision`)", gi.Group.Superseded)
	}
	fmt.Println(header)
	for i, rev := range gi.Group.Heads {
		ref, found := primaryRef(gi.Refs, rev.Digest)
		if !found {
			continue
		}
		fmt.Printf("  %2d) %s  %s  %s\n", i+1,
			digest12(ref.Digest), deviceHumanLabel(ref.Meta), capturedLabel(ref.Meta))
	}
	sel := readPromptLine("\nSelect head to activate (number, [S]kip, or blank to abort): ")
	sel = strings.TrimSpace(sel)
	isSkip, isAbort, n, perr := parseHeadChoice(sel, len(gi.Group.Heads))
	if perr != nil {
		return "", false, false, perr
	}
	switch {
	case isAbort:
		return "", false, true, nil
	case isSkip:
		return "", true, false, nil
	default:
		return gi.Group.Heads[n-1].Digest, false, false, nil
	}
}
