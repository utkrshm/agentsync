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
// accepts via --repo) the git URL, optionally records a display-only device
// alias (--device-alias), initializes the repo, and adds the remote.
func cmdInit(args []string) error {
	repoURL := ""
	deviceAliasFlag := ""
	// Parse --repo <url> and --device-alias <name> flags.
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--repo":
			if i+1 >= len(args) {
				return fmt.Errorf("--repo requires a URL")
			}
			repoURL = args[i+1]
			i++
		case "--device-alias":
			if i+1 >= len(args) {
				return fmt.Errorf("--device-alias requires a name")
			}
			deviceAliasFlag = args[i+1]
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

	alias, err := resolveAlias(deviceAliasFlag, cfg.Sync.DeviceAlias,
		func() (bool, error) { return confirm("Keep it?") }, prompt)
	if err != nil {
		return err
	}
	cfg.Sync.DeviceAlias = alias

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

	fmt.Printf("AgentSync initialized.\n  Sync repo : %s\n  Remote    : %s\n  Alias     : %s\n",
		cfg.Sync.RepoPath, displayRemote(repoURL), displayAlias(alias))
	return nil
}

// resolveAlias applies device-alias precedence:
//
//  1. flag value provided → use it (overrides anything stored);
//  2. non-empty stored alias → offer to keep it, falling through to the
//     prompt on refusal;
//  3. otherwise prompt (blank input keeps the stored alias, or stays empty).
//
// askKeep and askInput are injectable so precedence is testable without stdin.
func resolveAlias(flagVal, stored string, askKeep func() (bool, error), askInput func(string) (string, error)) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if stored != "" {
		fmt.Printf("Device alias already configured: %s\n", stored)
		ok, err := askKeep()
		if err != nil {
			return "", err
		}
		if ok {
			return stored, nil
		}
	}
	input, err := askInput(aliasLabel(stored))
	if err != nil {
		return "", err
	}
	if input == "" {
		return stored, nil
	}
	return input, nil
}

// aliasLabel builds the interactive prompt label. When a previous/default
// alias exists it is shown after "Default - "; otherwise the label omits
// that portion entirely.
func aliasLabel(existing string) string {
	if existing == "" {
		return "Enter device alias (Optional):"
	}
	return "Enter device alias (Optional, Default - " + existing + "):"
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

func displayAlias(alias string) string {
	if alias == "" {
		return "(none)"
	}
	return alias
}
