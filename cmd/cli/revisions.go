package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentsync/internal/revision"
)

// revisionRef is one discoverable session artifact in the sync repo,
// regardless of which storage layout produced it. Legacy flat-layout refs
// carry their digest computed from bytes; revisions-layout refs take theirs
// from the immutable filename.
type revisionRef struct {
	SessionID   string
	Key         string
	Digest      string
	PayloadPath string         // absolute path inside the sync repo working tree
	Meta        *revision.Meta // nil when no sidecar knowledge exists (legacy without import-meta, missing/corrupt sidecar)
	Legacy      bool           // true for opencode/<key>/export/<sid>.json artifacts
}

// findRevisions walks BOTH storage layouts under <repo>/opencode:
//
//   - legacy flat layout: opencode/<key>/export/<session-id>.json, with the
//     digest computed from the payload bytes and a synthetic Meta mapped
//     best-effort from the legacy import-meta/<session-id>.json sidecar;
//   - revisions layout: opencode/<key>/sessions/<sid>/revisions/<digest>.json,
//     ignoring *.meta.json payloads and loading their sidecars best-effort.
//
// Results are sorted deterministically by (Key, SessionID, Digest); exact
// duplicates are collapsed. A missing opencode/ tree yields no refs, not an
// error.
func findRevisions(repoPath string) ([]revisionRef, error) {
	roots, err := storageRoots(repoPath)
	if err != nil {
		return nil, err
	}
	var refs []revisionRef
	for _, root := range roots {
		legacyRefs, err := walkLegacyLayout(root.Key, root.Path)
		if err != nil {
			return nil, err
		}
		refs = append(refs, legacyRefs...)

		newRefs, err := walkRevisionLayout(root.Key, root.Path)
		if err != nil {
			return nil, err
		}
		refs = append(refs, newRefs...)
	}
	return sortAndDedupe(refs), nil
}

// keyRoot is one storage root: a canonical key (which may itself contain
// slashes — the _unmapped/<path> sentinel guarantees it) and its directory.
type keyRoot struct {
	Key  string // slash-separated, relative to opencode/
	Path string // absolute directory of that key
}

// storageRoots lists every opencode/**/ directory that directly contains an
// export/, import-meta/ or sessions/ subdirectory. Key discovery is
// depth-agnostic on purpose: single-level ReadDir loops silently hid every
// session whose canonical key contained a slash (e.g. _unmapped/home/dev/x),
// making it invisible to receive, conflicts and migration alike.
func storageRoots(repoPath string) ([]keyRoot, error) {
	base := filepath.Join(repoPath, "opencode")
	if _, err := os.Stat(base); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var roots []keyRoot
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(base, path)
		if rerr != nil || rel == "." {
			return nil
		}
		hasSub := func(name string) bool {
			fi, e := os.Stat(filepath.Join(path, name))
			return e == nil && fi.IsDir()
		}
		if hasSub("export") || hasSub("import-meta") || hasSub("sessions") {
			roots = append(roots, keyRoot{Key: filepath.ToSlash(rel), Path: path})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Key < roots[j].Key })
	return roots, nil
}

// sortAndDedupe orders refs deterministically by (Key, SessionID, Digest)
// with PayloadPath as final tiebreaker and collapses exact duplicates.
// Identity includes the sidecar pointer: refs differing only in loaded-meta
// knowledge are NOT duplicates.
func sortAndDedupe(refs []revisionRef) []revisionRef {
	sort.Slice(refs, func(i, j int) bool {
		a, b := refs[i], refs[j]
		switch {
		case a.Key != b.Key:
			return a.Key < b.Key
		case a.SessionID != b.SessionID:
			return a.SessionID < b.SessionID
		case a.Digest != b.Digest:
			return a.Digest < b.Digest
		default:
			return a.PayloadPath < b.PayloadPath
		}
	})
	seen := make(map[string]bool, len(refs))
	out := make([]revisionRef, 0, len(refs))
	for _, ref := range refs {
		id := ref.Key + "\x00" + ref.SessionID + "\x00" + ref.Digest + "\x00" + ref.PayloadPath + "\x00"
		if ref.Meta != nil {
			id += fmt.Sprintf("%p", ref.Meta)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, ref)
	}
	return out
}

// walkLegacyLayout collects opencode/<key>/export/<session-id>.json refs,
// computing each digest from the actual bytes (the filename holds only the
// session id). A matching import-meta/<session-id>.json is mapped into a
// synthetic migrated-revision Meta, best-effort: absence or corruption of
// that sidecar leaves Meta nil rather than failing the walk.
func walkLegacyLayout(key, keyPath string) ([]revisionRef, error) {
	exportDir := filepath.Join(keyPath, "export")
	files, err := os.ReadDir(exportDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	importMetaDir := filepath.Join(keyPath, "import-meta")

	var refs []revisionRef
	for _, f := range files {
		name := f.Name()
		if f.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".meta.json") {
			continue
		}
		sessionID := strings.TrimSuffix(name, ".json")
		payloadPath := filepath.Join(exportDir, name)
		digest, err := artifactDigest(payloadPath)
		if err != nil {
			return nil, err
		}
		ref := revisionRef{
			SessionID:   sessionID,
			Key:         key,
			Digest:      digest,
			PayloadPath: payloadPath,
			Legacy:      true,
		}
		// readImportMeta reports a zero value (nil error) when the sidecar is
		// absent, so gate on existence to preserve "no sidecar knowledge"
		// (Meta stays nil) versus "sidecar present" (synthetic Meta).
		metaPath := filepath.Join(importMetaDir, name)
		if _, statErr := os.Stat(metaPath); statErr == nil {
			if im, err := readImportMeta(metaPath); err == nil {
				ref.Meta = &revision.Meta{
					SchemaVersion:     revision.SchemaVersion,
					OriginalSessionID: sessionID,
					Digest:            digest,
					Status:            revision.StatusMigrated,
					ProjectID:         im.ProjectID,
					Directory:         im.Directory,
					Title:             im.Title,
				}
			}
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// walkRevisionLayout collects opencode/<key>/sessions/<sid>/revisions/
// <digest>.json payload refs. The digest comes from the immutable filename;
// the sibling <digest>.meta.json sidecar is loaded best-effort (missing or
// corrupt sidecars leave Meta nil — the payload self-verifies via its name).
func walkRevisionLayout(key, keyPath string) ([]revisionRef, error) {
	sessionsDir := filepath.Join(keyPath, "sessions")
	sidEntries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var refs []revisionRef
	for _, sidEntry := range sidEntries {
		if !sidEntry.IsDir() {
			continue
		}
		sessionID := sidEntry.Name()
		revDir := filepath.Join(sessionsDir, sessionID, "revisions")
		files, err := os.ReadDir(revDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".meta.json") {
				continue
			}
			digest := strings.TrimSuffix(name, ".json")
			ref := revisionRef{
				SessionID:   sessionID,
				Key:         key,
				Digest:      digest,
				PayloadPath: filepath.Join(revDir, name),
			}
			if m, err := revision.ReadMeta(filepath.Join(revDir, digest+".meta.json")); err == nil {
				meta := m
				ref.Meta = &meta
			}
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// artifactDigest returns the lowercase hex sha256 of the file at path.
func artifactDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
