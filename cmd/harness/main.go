// Command harness is the plain CLI: -p print mode, interactive REPL,
// --resume. Stdlib-only: it never imports the TUI package.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"minimalharness/internal/agent"
	"minimalharness/internal/app"
)

func main() {
	var (
		printMode = flag.String("p", "", "print mode: run one task and exit")
		root      = flag.String("root", ".", "workspace root")
		mode      = flag.String("mode", "suggest", "suggest (ask) or auto")
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

	if a.ResumedMessage != "" {
		fmt.Println(a.ResumedMessage)
	}

	// Plain renderer: the CLI is one consumer of the loop's event stream.
	// It prints streamed text as it arrives and ends the line at the next
	// boundary event (tool call, usage, done, error) — the same output
	// shape as before the event seam.
	var textPending bool
	a.Cfg.OnEvent = func(ev agent.Event) {
		switch ev.Kind {
		case agent.EventTextDelta:
			fmt.Print(ev.Text)
			textPending = true
		default:
			if textPending {
				fmt.Println()
				textPending = false
			}
		}
	}

	ctx := context.Background()
	fmt.Printf("harness v1  api=%s  model=%s  root=%s  mode=%s  session=%s\n", a.Cfg.Client.API, a.Cfg.Client.Model, a.Root, a.Cfg.Mode, a.Store.ID)

	runner := &app.Runner{App: a, Print: func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	}}

	if *printMode != "" {
		runner.Handle(ctx, *printMode)
		return
	}

	// REPL
	fmt.Println("Type a task, /skill:<name> to load a skill, /usage for session totals, /clear to drop context, /compact to summarise context, or /quit to exit.")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		if runner.Handle(ctx, strings.TrimSpace(scanner.Text())) {
			break
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "harness:", err)
	os.Exit(1)
}
