package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/syncrepo"
)

const rekeyUsage = `agent-sync rekey — relocate one project's stored revisions to another canonical key

Usage:
  agent-sync rekey <old-key> <new-key>

Moves the whole artifact subtree opencode/<old-key>/ (revisions,
legacy export/, import-meta/) to opencode/<new-key>/ in ONE commit
("rekey: <old-key> -> <new-key>"). The primary use is adopting an
_unmapped/<path> island onto a permanent identity after adding a remote
or alias — zero orphaned revisions, ever.

Both keys are typed explicitly on purpose: this is a rare administrative
operation and the spelled-out keys double as a safety checkpoint.

Rules:
  - <old-key> must exist under opencode/; missing keys list the known ones.
  - <new-key> must NOT exist anywhere — rekey never merges two projects.
  - <new-key> must not start with "_unmapped/" — that would recreate an island.
  - .meta.json sidecars are untouched: provenance describes a revision's
    origin, not its address.
  - Push remains manual afterwards (tool-wide convention).
`

// cmdRekey implements agent-sync rekey <old-key> <new-key>.
//
// Explicit-only by design (no --from <dir> auto-resolution): relocating a
// project's history is administrative, and a wrong guess merges unrelated
// histories — exactly what canonical keys exist to prevent. The move is a
// plain os.Rename of the key directory inside the sync repo working tree;
// staging goes through Repo.CommitMessage so only sync-owned paths are ever
// staged and per-file artifact validation still gates what enters history
// (AGENTS.md invariant 11). Sidecars ride along byte-for-byte: they describe
// revisions, not their storage address.
func cmdRekey(args []string) error {
	var oldKey, newKey string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			fmt.Print(rekeyUsage)
			return nil
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown rekey flag %q", args[i])
			}
			switch {
			case oldKey == "":
				oldKey = args[i]
			case newKey == "":
				newKey = args[i]
			default:
				return fmt.Errorf("unexpected argument %q — rekey takes exactly two keys", args[i])
			}
		}
	}
	if oldKey == "" || newKey == "" {
		return fmt.Errorf("rekey requires two arguments: agent-sync rekey <old-key> <new-key>")
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

	for name, key := range map[string]string{"old-key": oldKey, "new-key": newKey} {
		if err := validateKeyShape(key); err != nil {
			return fmt.Errorf("%s %q: %w", name, key, err)
		}
	}
	if oldKey == newKey {
		return fmt.Errorf("old-key and new-key are identical — nothing to relocate")
	}
	if strings.HasPrefix(newKey, "_unmapped/") {
		return fmt.Errorf("new-key must not start with _unmapped/ — that recreates an unmapped island")
	}

	base := filepath.Join(repo.Path, "opencode")
	oldAbs := filepath.Join(base, filepath.FromSlash(oldKey))
	newAbs := filepath.Join(base, filepath.FromSlash(newKey))

	keys, err := storageRoots(repo.Path)
	if err != nil {
		return err
	}
	known := make([]string, 0, len(keys))
	nestedInsideOld := []string{}
	for _, kr := range keys {
		known = append(known, kr.Key)
		if kr.Key != oldKey && hasDirPrefix(kr.Key, oldKey) {
			nestedInsideOld = append(nestedInsideOld, kr.Key)
		}
	}

	if _, statErr := os.Stat(oldAbs); os.IsNotExist(statErr) {
		return fmt.Errorf("no artifacts under opencode/%s — nothing to relocate.\nKnown project keys:\n  %s",
			oldKey, strings.Join(known, "\n  "))
	} else if statErr != nil {
		return statErr
	}
	if len(nestedInsideOld) > 0 {
		sort.Strings(nestedInsideOld)
		return fmt.Errorf("opencode/%s physically contains other project keys (%s) — renaming it wholesale would silently relocate them; refuse instead",
			oldKey, strings.Join(nestedInsideOld, ", "))
	}
	if fi, statErr := os.Stat(newAbs); statErr == nil {
		return fmt.Errorf("destination opencode/%s already exists (%s) — rekey never merges two projects; pick an unused new-key",
			newKey, describeEntry(fi))
	} else if !os.IsNotExist(statErr) {
		return statErr
	}

	// Multi-segment destinations need their parent chain present for rename.
	if dir := filepath.Dir(newAbs); dir != base && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if err := os.Rename(oldAbs, newAbs); err != nil {
		return fmt.Errorf("move opencode/%s -> opencode/%s: %w", oldKey, newKey, err)
	}

	version, cerr := repo.CommitMessage(fmt.Sprintf("rekey: %s -> %s", oldKey, newKey))
	if cerr != nil {
		if errors.Is(cerr, syncrepo.ErrNoChanges) {
			fmt.Printf("Moved opencode/%s -> opencode/%s, but there was nothing to commit (all moved files failed validation or nothing was tracked). Inspect the working tree.\n",
				oldKey, newKey)
			return nil
		}
		return fmt.Errorf("commit relocation: %w (the move already happened on disk)", cerr)
	}
	fmt.Printf("Rekeyed opencode/%s -> opencode/%s (recorded as commit v%d).\n", oldKey, newKey, version)
	fmt.Println("Push remains manual — run `agent-sync push` or retry when ready.")
	return nil
}

// validateKeyShape rejects anything that could escape opencode/ when joined:
// empty segments, dot segments, absolute paths, Windows drive letters riding
// along in a slash key. Canonical keys MAY contain slashes (AGENTS.md
// invariant 12), so each segment is checked individually rather than the key
// being treated as one filename.
func validateKeyShape(key string) error {
	if key == "" {
		return fmt.Errorf("empty")
	}
	if filepath.IsAbs(key) || strings.HasPrefix(key, "/") {
		return fmt.Errorf("must be relative to the sync repo's opencode/ root")
	}
	for _, seg := range strings.Split(key, "/") {
		switch seg {
		case "", ".", "..":
			return fmt.Errorf("invalid path segment %q", seg)
		}
	}
	return nil
}

// hasDirPrefix reports whether child lives strictly below parent (slash-
// segment aware): child="a/b/c" is inside parent="a/b" but not of parent="a".
func hasDirPrefix(child, parent string) bool {
	if parent == "" {
		return false
	}
	childSegs := strings.Split(strings.TrimSuffix(child, "/"), "/")
	parentSegs := strings.Split(parent, "/")
	if len(childSegs) <= len(parentSegs) {
		return false
	}
	for i, p := range parentSegs {
		if childSegs[i] != p {
			return false
		}
	}
	return true
}

func describeEntry(fi os.FileInfo) string {
	if fi.IsDir() {
		return "directory"
	}
	return fmt.Sprintf("file, %d bytes", fi.Size())
}
