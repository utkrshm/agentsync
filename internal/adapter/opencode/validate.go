package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// sync repo commits it. AgentSync writes only two shapes; anything else in
// the payload tree (hand-edited JSON, editor droppings, corrupted exports)
// must never enter shared history where receive would later trip over it.
//
// Recognized locations and their contracts:
//
//	opencode/<project>/export/<session-id>.json      full export: ValidateExport
//	opencode/<project>/import-meta/<session-id>.json sidecar: valid JSON, id == stem
//
// The filename stem must equal the session id recorded inside for both.
// Locations that match neither shape are rejected outright.
func CheckArtifactFile(absPath, relPath string) error {
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	if len(parts) != 4 || parts[0] != "opencode" {
		return fmt.Errorf(
			"unexpected location %s (want opencode/<project>/(export|import-meta)/<session-id>.json)",
			relPath)
	}
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
