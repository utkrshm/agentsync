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

Sessions with multiple preserved revisions appear as ONE entry marked
"(N revisions — conflicted)"; resuming one requires explicit confirmation
of the newest revision.
`

// cmdResume composes the v0.1 one-shot flow:
//  1. git -C <code-repo> pull   (code)
//  2. receive logic             (session)
//  3. numbered picker over synced sessions, then `opencode -s <id>`
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

	// 3. Picker + resume, grouped by conflict state (one entry per session;
	//    multi-revision sessions collapse into a single conflicted entry).
	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	repo := syncrepo.Open(cfg.Sync.RepoPath)
	revs, err := findRevisions(repo.Path)
	if err != nil {
		return err
	}
	sessions := resumeEntries(revs)
	if len(sessions) == 0 {
		fmt.Println("No synced sessions to resume.")
		return nil
	}

	choice, err := pickSession(sessions)
	if err != nil {
		return err
	}
	if choice < 0 {
		fmt.Println("Aborted.")
		return nil
	}

	sel := sessions[choice]
	if sel.Conflicted {
		if !confirmNewestConflictResume(revs, sel) {
			fmt.Println("Aborted.")
			return nil
		}
	}
	fmt.Printf("Resuming %s ...\n", sel.ID)
	// Launching always uses the ORIGINAL session id: OpenCode's model keeps
	// one live session per id regardless of how many stored revisions exist.
	return launchOpenCode(sel.ID)
}

type sessionEntry struct {
	ID         string
	Key        string
	Title      string
	Conflicted bool
	RevCount   int // distinct revisions behind this entry (1 when clean)
}

// resumeEntries builds picker entries from conflict groups over walker refs
// (DetectV2: per-device chains collapse to heads):
//   - a session whose device chains agree becomes one plain entry titled from
//     its sidecar ("(untitled)" without one), counted by surviving HEADS;
//   - a conflicted session collapses into ONE entry titled
//     "<title> (N revisions — conflicted)";
//   - superseded mid-chain revisions are shown as an "(N older superseded)"
//     suffix rather than inflating the head count.
//
// Entries are sorted by session id for stable presentation.
func resumeEntries(refs []revisionRef) []sessionEntry {
	groups := buildConflictGroups(refs)
	out := make([]sessionEntry, 0, len(groups))
	for _, gi := range groups {
		newest := newestRevisionRef(gi.Refs)
		title := ""
		if newest.Meta != nil {
			title = newest.Meta.Title
		}
		if title == "" {
			title = "(untitled)"
		}
		e := sessionEntry{
			ID:         gi.Group.SessionID,
			Key:        gi.Group.Key,
			Title:      title,
			Conflicted: gi.Group.Conflicted,
			RevCount:   len(gi.Group.Heads),
		}
		if e.Conflicted {
			e.Title = fmt.Sprintf("%s (%d revisions — conflicted)", title, e.RevCount)
		}
		if gi.Group.Superseded > 0 {
			e.Title += fmt.Sprintf(" (%d older superseded)", gi.Group.Superseded)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// confirmNewestConflictResume lists a conflicted session's revisions
// individually and requires an explicit y/N confirmation before launching the
// NEWEST revision (never an automatic pick). Returning false aborts cleanly.
func confirmNewestConflictResume(refs []revisionRef, sel sessionEntry) bool {
	var groupRefs []revisionRef
	for _, r := range refs {
		if r.SessionID == sel.ID && r.Key == sel.Key {
			groupRefs = append(groupRefs, r)
		}
	}
	gi := buildConflictGroups(groupRefs)[0]

	fmt.Printf("\n%s has %d preserved revisions:\n", sel.ID, len(gi.Group.Revisions))
	for i, rev := range gi.Group.Revisions {
		ref, ok := primaryRef(gi.Refs, rev.Digest)
		if !ok {
			continue
		}
		fmt.Printf("  %2d) %s\n", i+1, conflictReportLine(ref))
	}
	newest := newestRevisionRef(groupRefs)
	fmt.Printf("\nThis session is conflicted — none of these was restored automatically.\n")
	question := fmt.Sprintf("Resume the NEWEST revision anyway (%s)?", conflictReportLine(newest))
	return confirmPrompt(question)
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

// confirmPrompt asks a yes/no question; anything other than an explicit y/yes
// is treated as no.
func confirmPrompt(question string) bool {
	fmt.Printf("%s [y/N] ", question)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return false
	}
	return parseConfirm(sc.Text())
}

// parseConfirm interprets a confirmation answer; only y/yes (any case,
// surrounding whitespace tolerated) count as consent.
func parseConfirm(input string) bool {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "y", "yes":
		return true
	default:
		return false
	}
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
