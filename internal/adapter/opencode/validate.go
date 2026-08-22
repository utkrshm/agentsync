package opencode

import (
	"encoding/json"
	"fmt"
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
