package opencode

import (
	"encoding/json"

	"agentsync/internal/fsutil"
)

// WriteImportMeta persists the receive-side patch metadata for a session
// atomically. Used by Mirror and by the send command.
func WriteImportMeta(path string, info ExportInfo) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, data, 0o600)
}
