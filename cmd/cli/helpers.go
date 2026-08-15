package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"agentsync/internal/adapter/opencode"
	"agentsync/internal/config"
)

// requireConfig loads the config, erroring with a hint if not initialized.
func requireConfig() (config.Config, error) {
	cfg, exists, err := config.Load()
	if err != nil {
		return cfg, err
	}
	if !exists {
		return cfg, fmt.Errorf("no config found — run `agent-sync init` first")
	}
	if cfg.Sync.RepoPath == "" {
		cfg.Sync.RepoPath = config.DefaultRepoPath()
	}
	return cfg, nil
}

// configFilePath returns the config file location.
func configFilePath() (string, error) {
	return config.Path()
}

// readExportInfoFrom is a thin wrapper over the opencode adapter's parser so
// the CLI doesn't import it directly with a hidden name.
func readExportInfoFrom(path string) (opencode.ExportInfo, error) {
	return opencode.ReadExportInfo(path)
}

// copyFile copies src to dst (permissions: owner rw).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// writeImportMeta writes the receive-side patch metadata for a session.
func writeImportMeta(path string, info opencode.ExportInfo) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// importMeta describes what a synced session needs for the import patch.
type importMeta struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID"`
	Directory string `json:"directory"`
	Title     string `json:"title"`
}

// readImportMeta reads an import-meta JSON file, returning zero value if the
// file is absent (older exports may not have it).
func readImportMeta(path string) (importMeta, error) {
	var m importMeta
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}
