package main

import (
	"strings"
	"testing"
)

func TestCommandHelpCoversEveryDispatchedCommand(t *testing.T) {
	dispatched := []string{"init", "send", "receive", "resume", "pull", "index",
		"migrate-layout", "recover", "revisions", "conflicts", "rekey"}
	for _, cmd := range dispatched {
		h, ok := commandHelp[cmd]
		if !ok {
			t.Fatalf("command %q has no help entry", cmd)
		}
		if h == "" {
			t.Fatalf("command %q has empty help text", cmd)
		}
	}
	if len(commandHelp) != len(dispatched) {
		t.Errorf("commandHelp has %d entries, want %d (stale map?)", len(commandHelp), len(dispatched))
	}
	for name, h := range commandHelp {
		want := "agent-sync " + name + " —"
		if len(h) < len(want) || h[:len(want)] != want {
			t.Errorf("help for %q should start with %q, got %.40q", name, want, h)
		}
	}
}

func TestHasHelpFlag(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--dry-run"}, false},
		{[]string{"--repo", "x"}, false},
		{[]string{"-h"}, true},
		{[]string{"--help"}, true},
		{[]string{"--repo", "--help"}, true},
	}
	for _, tc := range cases {
		if got := hasHelpFlag(tc.args); got != tc.want {
			t.Errorf("hasHelpFlag(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestTopLevelUsageListsAllCommands(t *testing.T) {
	for _, cmd := range []string{"init", "send", "receive", "resume", "pull", "index",
		"migrate-layout", "recover", "revisions", "conflicts", "rekey", "help"} {
		if !strings.Contains(usage, cmd) {
			t.Errorf("usage text missing command %q", cmd)
		}
	}
}
