package main

import (
	"encoding/json"
	"fmt"
	"os"

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
