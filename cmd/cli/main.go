// Command agent-sync is the v0.1 CLI: init / send / receive / resume / pull
// for syncing OpenCode sessions across devices via a git repo.
package main

import (
	"fmt"
	"os"
)

const usage = `agent-sync — sync AI agent sessions across devices via git

Usage:
  agent-sync init [--repo <url>]              set up the sync repo (prompts for git URL)
  agent-sync send <session-id>                export a session, commit (timestamped+versioned), push
  agent-sync receive                          pull + write back new sessions into local clones
  agent-sync resume [--repo <code-repo>]      pull code + receive + pick a session to resume
  agent-sync pull                             fetch + fast-forward the sync repo only
  agent-sync index                            scan [repoindex] roots for local git repos
  agent-sync help                             show this help
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := args[0]
	var err error
	switch cmd {
	case "init":
		err = cmdInit(args[1:])
	case "send":
		err = cmdSend(args[1:])
	case "receive":
		err = cmdReceive(args[1:])
	case "resume":
		err = cmdResume(args[1:])
	case "pull":
		err = cmdPull(args[1:])
	case "index":
		err = cmdIndex(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-sync %s: %v\n", cmd, err)
		os.Exit(1)
	}
}
