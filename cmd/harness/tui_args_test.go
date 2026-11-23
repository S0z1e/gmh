package main

import (
	"flag"
	"testing"
)

func TestTUIForwardsExplicitRebindAndBooleanFlags(t *testing.T) {
	args := tuiArgs("harness-tui", "/project", "suggest", "model", "chat", "provider", "config", 7, "old", "", 128000, false, 13, 42, true, []string{"picture.png"}, []string{"server log.txt"}, "trust", true)
	fs := flag.NewFlagSet("harness-tui", flag.ContinueOnError)
	for _, name := range []string{"root", "workspace-trust", "mode", "model", "api", "provider", "config", "resume", "session"} {
		fs.String(name, "", "")
	}
	for _, name := range []string{"max-steps", "context-limit", "bash-timeout", "bg-threshold"} {
		fs.Int(name, 0, "")
	}
	rebind := fs.Bool("rebind-session", false, "")
	deny := fs.Bool("deny-instead-of-prompt", true, "")
	sandbox := fs.Bool("sandbox", false, "")
	image := fs.String("image", "", "")
	file := fs.String("attach", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		t.Fatal(err)
	}
	if len(fs.Args()) != 0 || !*rebind || *deny || !*sandbox || *image != "picture.png" || *file != "server log.txt" || fs.Lookup("bg-threshold").Value.String() != "42" {
		t.Fatalf("incomplete TUI flag forwarding: args=%v rest=%v", args, fs.Args())
	}
}
