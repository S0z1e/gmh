// Command harness-tui is the Bubble Tea v2 interface for the harness.
// It is a separate binary so cmd/harness (and -p) never imports Bubble Tea.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"

	"minimalharness/internal/agent"
	"minimalharness/internal/app"
	"minimalharness/internal/tui"
)

func main() {
	var (
		root      = flag.String("root", ".", "workspace root")
		mode      = flag.String("mode", "suggest", "suggest (ask in a y/n dialog) or auto")
		model     = flag.String("model", "", "model (default OPENAI_MODEL or deepseek-v4.1-flash)")
		api       = flag.String("api", "", "chat or responses (default OPENAI_API_MODE or chat)")
		maxSteps  = flag.Int("max-steps", 50, "max agent steps")
		resume    = flag.String("resume", "", "resume session id")
		sessionID = flag.String("session", "", "session id (default: timestamp)")
		ctxLimit  = flag.Int("context-limit", 0, "context window in tokens for the compaction trigger (default: env OPENAI_CONTEXT_LIMIT or 1048576)")
	)
	flag.Parse()

	a, err := app.Setup(app.Params{
		Root:         *root,
		Mode:         *mode,
		Model:        *model,
		API:          *api,
		MaxSteps:     *maxSteps,
		ContextLimit: *ctxLimit,
		Resume:       *resume,
		SessionID:    *sessionID,
	})
	if err != nil {
		fatal(err)
	}
	defer a.Store.Close()

	// Header lines go into the model's initial view: prog.Println before
	// prog.Run() would block forever (the event loop hasn't started).
	info := fmt.Sprintf("harness-tui  api=%s  model=%s  root=%s  mode=%s  session=%s\nType a task, /skill:<name>, /usage, /clear, /compact, or /quit. Ctrl-C quits.", a.Cfg.Client.API, a.Cfg.Client.Model, a.Root, a.Cfg.Mode, a.Store.ID)
	if a.ResumedMessage != "" {
		info = a.ResumedMessage + "\n" + info
	}
	lines := make(chan string, 16)
	m := tui.NewModel(info)
	// Set OnSubmit BEFORE NewProgram: the program holds its own copy of
	// the model, so fields set afterwards on this copy would never fire.
	m.OnSubmit = func(line string) { lines <- line }
	prog := tea.NewProgram(m)

	// The agent loop runs in a goroutine and feeds events into the
	// program; Bubble Tea owns the main goroutine and stdin.
	a.Cfg.OnEvent = func(ev agent.Event) {
		prog.Send(tui.EventMsg{Event: ev})
	}
	// In suggest mode, approvals go through the TUI dialog: the agent
	// blocks on a reply channel, the model answers it from a y/n key.
	if a.Cfg.Mode == "suggest" {
		a.Cfg.Ask = tui.NewApprover(prog).Ask
	}

	runner := &app.Runner{App: a, Print: func(format string, args ...any) {
		prog.Println(fmt.Sprintf(format, args...))
	}}

	ctx := context.Background()
	go func() {
		for line := range lines {
			if runner.Handle(ctx, line) {
				prog.Quit()
				return
			}
		}
	}()

	if _, err := prog.Run(); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "harness-tui:", err)
	os.Exit(1)
}
