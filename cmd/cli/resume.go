package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"agentsync/internal/syncrepo"
)

const resumeUsage = `agent-sync resume — pull code, receive sessions, pick one to resume

Usage:
  agent-sync resume [--repo <code-repo-path>]

1. Optionally runs "git pull" in --repo <path> (the code repo — a
   separate git operation from the sync repo pull).
2. Runs the full receive flow (see "agent-sync help receive").
3. Shows a numbered picker over synced sessions and launches
   "opencode -s <session-id>" for the chosen one.
`

// cmdResume composes the v0.1 one-shot flow:
//  1. git -C <code-repo> pull   (code)
//  2. receive logic             (session)
//  3. numbered picker over synced sessions, then `opencode resume <id>`
func cmdResume(args []string) error {
	codeRepo := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--repo":
			if i+1 >= len(args) {
				return fmt.Errorf("--repo requires a path")
			}
			codeRepo = args[i+1]
			i++
		default:
			return fmt.Errorf("unknown resume flag %q", args[i])
		}
	}

	// 1. Pull the code repo if a path is given (a separate git op from the
	//    sync-repo pull, per IMPLEMENTATION-PLAN.md §0.1).
	if codeRepo != "" {
		fmt.Printf("Pulling code repo %s ...\n", codeRepo)
		if err := pullRepo(codeRepo); err != nil {
			return fmt.Errorf("code repo pull: %w", err)
		}
	}

	// 2. Receive sessions.
	if err := cmdReceive(nil); err != nil {
		return err
	}

	// 3. Picker + resume.
	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	repo := syncrepo.Open(cfg.Sync.RepoPath)
	exports, err := findExports(repo.Path)
	if err != nil {
		return err
	}
	if len(exports) == 0 {
		fmt.Println("No synced sessions to resume.")
		return nil
	}

	sessions := make([]sessionEntry, 0, len(exports))
	for _, ex := range exports {
		title := ""
		if im, err := readImportMeta(ex.ImportMetaPath); err == nil {
			title = im.Title
		}
		if title == "" {
			title = "(untitled)"
		}
		sessions = append(sessions, sessionEntry{ID: ex.SessionID, Key: ex.Key, Title: title})
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })

	choice, err := pickSession(sessions)
	if err != nil {
		return err
	}
	if choice < 0 {
		fmt.Println("Aborted.")
		return nil
	}

	sel := sessions[choice]
	fmt.Printf("Resuming %s ...\n", sel.ID)
	return launchOpenCode(sel.ID)
}

type sessionEntry struct {
	ID    string
	Key   string
	Title string
}

// pickSession presents a numbered list and returns the chosen index (or -1 on
// abort). If there's only one session it returns it directly.
func pickSession(sessions []sessionEntry) (int, error) {
	if len(sessions) == 1 {
		return 0, nil
	}
	fmt.Println("\nSynced sessions:")
	for i, s := range sessions {
		fmt.Printf("  %2d) %s — %s (%s)\n", i+1, s.Title, s.ID, s.Key)
	}
	fmt.Print("\nSelect session to resume (number, or blank to abort): ")
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return -1, sc.Err()
	}
	sel := strings.TrimSpace(sc.Text())
	if sel == "" {
		return -1, nil
	}
	var n int
	if _, err := fmt.Sscanf(sel, "%d", &n); err != nil || n < 1 || n > len(sessions) {
		return -1, fmt.Errorf("invalid selection %q", sel)
	}
	return n - 1, nil
}

// launchOpenCode runs `opencode -s <id>`, inheriting stdio. (opencode
// 1.18.x removed the `resume` subcommand; `-s/--session` is its replacement.)
func launchOpenCode(sessionID string) error {
	cmd := exec.Command("opencode", "-s", sessionID)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// pullRepo runs `git -C <path> pull`.
func pullRepo(path string) error {
	cmd := exec.Command("git", "-C", path, "pull")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
