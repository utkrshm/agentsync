package opencode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentsync/internal/revision"
)

// ValidateExport checks an in-memory export payload before it is written
// anywhere durable (sync repo, import patch). OpenCode's export format is
// undocumented and known to change between releases (AGENTS.md invariant #4):
// a killed `opencode export` or an interrupted copy can leave truncated JSON,
// so every consumer gates on this check instead of trusting the file. It
// parses only the `info` object from the given bytes — it never re-reads
// from disk.
//
// Requirements: payload is valid JSON, decodes into {info}, info.ID matches
// expectedSessionID, info.Directory and info.Version are non-empty. Each
// failure returns an error naming exactly what was missing or mismatched.
func ValidateExport(data []byte, expectedSessionID string) (ExportInfo, error) {
	if !json.Valid(data) {
		return ExportInfo{}, fmt.Errorf(
			"export %s: payload is not valid JSON — export was likely truncated; re-export the session",
			expectedSessionID)
	}
	info, err := parseExportInfo(data)
	if err != nil {
		return ExportInfo{}, fmt.Errorf("export %s: %w", expectedSessionID, err)
	}
	if info.ID != expectedSessionID {
		return ExportInfo{}, fmt.Errorf(
			"export %s: info.id is %q — mismatched export, refusing to store it under another session",
			expectedSessionID, info.ID)
	}
	if info.Directory == "" {
		return ExportInfo{}, fmt.Errorf(
			"export %s: info.directory is empty — canonical key cannot be resolved without it",
			expectedSessionID)
	}
	if info.Version == "" {
		return ExportInfo{}, fmt.Errorf(
			"export %s: info.version is empty — write-back compatibility checks need the producing version",
			expectedSessionID)
	}
	return info, nil
}

// parseExportInfo decodes just the `info` field of an export JSON document.
func parseExportInfo(data []byte) (ExportInfo, error) {
	var doc struct {
		Info ExportInfo `json:"info"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return ExportInfo{}, fmt.Errorf("parse export info: %w", err)
	}
	return doc.Info, nil
}

// CheckArtifactFile gates one working-tree file under opencode/ before the
// sync repo commits it. AgentSync writes only the shapes below; anything else
// in the payload tree (hand-edited JSON, editor droppings, corrupted exports)
// must never enter shared history where receive would later trip over it.
//
// Recognized locations and their contracts:
//
//	opencode/<project>/export/<session-id>.json
//	  full export: ValidateExport (legacy flat layout)
//	opencode/<project>/import-meta/<session-id>.json
//	  legacy sidecar: valid JSON, id == stem
//	opencode/<project>/sessions/<sid>/revisions/<digest>.json
//	  immutable revision payload: digest is 64 lowercase hex AND equals the
//	  sha256 of the file bytes — payloads are self-verifying
//	opencode/<project>/sessions/<sid>/revisions/<digest>.meta.json
//	  revision sidecar: valid revision.Meta JSON whose digest matches the
//	  filename and whose original_session_id matches the path segment
//
// Locations that match none of these shapes are rejected outright.
func CheckArtifactFile(absPath, relPath string) error {
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	if len(parts) < 2 || parts[0] != "opencode" {
		return fmt.Errorf(
			"unexpected location %s (want opencode/<project>/... artifact paths)",
			relPath)
	}
	switch {
	case len(parts) == 4:
		return checkLegacyArtifact(absPath, relPath, parts)
	case len(parts) == 6 && parts[2] == "sessions" && parts[4] == "revisions":
		return checkRevisionArtifact(absPath, relPath, parts)
	default:
		return fmt.Errorf(
			"unrecognized opencode/ location %s — expected export/, import-meta/, or sessions/<sid>/revisions/<digest>.json",
			relPath)
	}
}

// checkLegacyArtifact validates the pre-revisions flat layout: full exports
// under export/ and their import-meta sidecars under import-meta/.
func checkLegacyArtifact(absPath, relPath string, parts []string) error {
	name := parts[3]
	stem := strings.TrimSuffix(name, ".json")
	if stem == "" || stem == name {
		return fmt.Errorf("%s: expected a .json file named after its session id", relPath)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}
	switch parts[2] {
	case "export":
		if _, verr := ValidateExport(data, stem); verr != nil {
			return verr
		}
	case "import-meta":
		var m struct {
			ID string `json:"id"`
		}
		if !json.Valid(data) || json.Unmarshal(data, &m) != nil {
			return fmt.Errorf("import-meta %s: not valid JSON", relPath)
		}
		if m.ID != stem {
			return fmt.Errorf("import-meta %s: id %q does not match filename", relPath, m.ID)
		}
	default:
		return fmt.Errorf("unrecognized opencode/ subdirectory %q — expected export or import-meta", parts[2])
	}
	return nil
}

// checkRevisionArtifact validates one immutable revision payload or sidecar.
// The filename carries the sha256 digest of the payload bytes, so a payload's
// content can be verified without any external state; a sidecar must agree
// with both its digest and its session path segment.
func checkRevisionArtifact(absPath, relPath string, parts []string) error {
	sessionID, name := parts[3], parts[5]
	if sessionID == "" || sessionID == "." || sessionID == ".." {
		return fmt.Errorf("%s: invalid session directory", relPath)
	}
	isMeta := strings.HasSuffix(name, ".meta.json")
	stem := strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".meta")
	if !is64Hex(stem) {
		return fmt.Errorf("%s: filename must be a 64-character lowercase hex sha256 digest", relPath)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}
	if isMeta {
		var m revision.Meta
		if !json.Valid(data) || json.Unmarshal(data, &m) != nil {
			return fmt.Errorf("revision meta %s: not valid JSON metadata", relPath)
		}
		if m.Digest != stem {
			return fmt.Errorf("revision meta %s: records digest %q but lives at digest %q",
				relPath, m.Digest, stem)
		}
		if m.OriginalSessionID != sessionID {
			return fmt.Errorf("revision meta %s: original_session_id %q does not match session directory %q",
				relPath, m.OriginalSessionID, sessionID)
		}
		return nil
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != stem {
		return fmt.Errorf(
			"revision payload %s: content hashes to %s but filename says %s — corrupt or hand-placed artifact, refusing to commit",
			relPath, got, stem)
	}
	return nil
}

// is64Hex reports whether s is exactly 64 lowercase hexadecimal digits.
func is64Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9') && !('a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
