// Command agent-sync is the v0.1 CLI: init / send / receive / resume / pull
// for syncing OpenCode sessions across devices via a git repo.
package main

import (
	"fmt"
	"os"
)

const usage = `agent-sync — sync AI agent sessions across devices via git

Usage: agent-sync <command> [flags]

Commands:
  init           configure AgentSync and create the sync repo
  send           export one OpenCode session into the sync repo and push
  receive        pull the sync repo and write back pending sessions
  resume         pull code repo, receive sessions, pick one to resume
  pull           fetch + fast-forward the sync repo only
  index          scan [repoindex] roots to find local clones for write-back
  migrate-layout move legacy exports layout (v0.1) into the immutable revisions layout
  recover        restore one chosen revision of a conflicted session
  revisions      inspect stored session revisions (list)
  conflicts      report same-session conflict groups
  rekey          move one project's stored revisions under another canonical key
  sync [--dry-run] <dir>  push/pull/resolve every session of the project at <dir>
  help           show help for all commands or one command

Config lives at ~/.config/agent-sync/config.toml (see ` + "`agent-sync help init`" + `).
Run "agent-sync help <command>" or "agent-sync <command> --help" for details.
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "help", "-h", "--help":
		if len(rest) == 0 || rest[0] == "help" {
			fmt.Print(usage)
			return
		}
		h, ok := commandHelp[rest[0]]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", rest[0], usage)
			os.Exit(2)
		}
		fmt.Print(h)
		return
	}

	if _, known := commandHelp[cmd]; !known {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if hasHelpFlag(rest) {
		fmt.Print(commandHelp[cmd])
		return
	}

	var err error
	switch cmd {
	case "init":
		err = cmdInit(rest)
	case "send":
		err = cmdSend(rest)
	case "receive":
		err = cmdReceive(rest)
	case "resume":
		err = cmdResume(rest)
	case "pull":
		err = cmdPull(rest)
	case "index":
		err = cmdIndex(rest)
	case "migrate-layout":
		err = cmdMigrateLayout(rest)
	case "recover":
		err = cmdRecover(rest)
	case "revisions":
		err = cmdRevisions(rest)
	case "conflicts":
		err = cmdConflicts(rest)
	case "rekey":
		err = cmdRekey(rest)
	case "sync":
		err = cmdSync(rest)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-sync %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

// commandHelp maps every dispatchable command to its detailed usage text,
// defined next to each command's implementation.
var commandHelp = map[string]string{
	"init":           initUsage,
	"send":           sendUsage,
	"receive":        receiveUsage,
	"resume":         resumeUsage,
	"pull":           pullUsage,
	"index":          indexUsage,
	"migrate-layout": migrateUsage,
	"recover":        recoverUsage,
	"revisions":      revisionsUsage,
	"conflicts":      conflictsUsage,
	"rekey":          rekeyUsage,
	"sync":           syncUsage,
}

func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}
