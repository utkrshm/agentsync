package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"agentsync/internal/config"
	"agentsync/internal/syncrepo"
)

// cmdInit sets up the sync repo: resolves the sync repo path, prompts for (or
// accepts via --repo) the git URL, initializes the repo, and adds the remote.
func cmdInit(args []string) error {
	repoURL := ""
	// Parse --repo <url> flag.
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--repo":
			if i+1 >= len(args) {
				return fmt.Errorf("--repo requires a URL")
			}
			repoURL = args[i+1]
			i++
		default:
			return fmt.Errorf("unknown init flag %q", args[i])
		}
	}

	// Load existing config (if any) to preserve/choose repo_path.
	cfg, exists, err := config.Load()
	if err != nil {
		return err
	}

	if cfg.Sync.RepoPath == "" {
		cfg.Sync.RepoPath = config.DefaultRepoPath()
	}

	// Prompt for the git URL if not provided.
	if repoURL == "" {
		if exists && cfg.Sync.Remote != "" {
			fmt.Printf("Remote already configured: %s\n", cfg.Sync.Remote)
			ok, err := confirm("Keep it?")
			if err != nil {
				return err
			}
			if ok {
				repoURL = cfg.Sync.Remote
			}
		}
		if repoURL == "" {
			repoURL, err = prompt("Enter git URL to store agent sessions in (blank = local-only):")
			if err != nil {
				return err
			}
		}
	}
	cfg.Sync.Remote = repoURL

	// Ensure a fresh config carries the daemon defaults (watch enabled, etc.)
	// so the written file isn't surprising on first `daemon` run.
	cfg.ApplyDefaults(func(...string) bool { return false })

	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	repo := syncrepo.Open(cfg.Sync.RepoPath)
	if !repo.Exists() {
		fmt.Printf("Initializing sync repo at %s ...\n", cfg.Sync.RepoPath)
		if err := repo.Init(); err != nil {
			return fmt.Errorf("init sync repo: %w", err)
		}
	}
	if repoURL != "" {
		if err := repo.SetRemote(repoURL); err != nil {
			return fmt.Errorf("set remote: %w", err)
		}
		fmt.Printf("Remote set: %s\n", repoURL)
	}
	if err := repo.TouchMeta(); err != nil {
		return fmt.Errorf("write .sync-meta.json: %w", err)
	}

	fmt.Printf("AgentSync initialized.\n  Sync repo : %s\n  Remote    : %s\n",
		cfg.Sync.RepoPath, displayRemote(repoURL))
	return nil
}

func prompt(label string) (string, error) {
	fmt.Print(label + " ")
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return "", sc.Err()
	}
	return strings.TrimSpace(sc.Text()), nil
}

func confirm(question string) (bool, error) {
	fmt.Print(question + " [y/N] ")
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return false, sc.Err()
	}
	a := strings.ToLower(strings.TrimSpace(sc.Text()))
	return a == "y" || a == "yes", nil
}

func displayRemote(url string) string {
	if url == "" {
		return "(none — local only)"
	}
	return url
}
