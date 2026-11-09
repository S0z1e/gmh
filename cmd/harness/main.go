// Command harness is the plain CLI: -p print mode, interactive REPL,
// --resume. Stdlib-only: it never imports the TUI package.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"minimalharness/internal/agent"
	"minimalharness/internal/app"
	"minimalharness/internal/uimode"
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
		ui        = flag.String("ui", "auto", "ui: auto, plain or tui (ignored by -p, which is always plain)")
	)
	flag.Parse()

	// UI selection happens before setup: the TUI is a separate binary
	// (harness-tui) so the plain CLI never imports Bubble Tea.
	// -p print mode is always plain and ignores --ui.
	if *printMode == "" {
		uiMode, err := uimode.Decide(*ui, uimode.IsTTY(os.Stdout))
		if err != nil {
			fatal(err)
		}
		if uiMode == "tui" {
			if err := execTUI(*root, *mode, *model, *api, *maxSteps, *resume, *sessionID, *ctxLimit); err != nil {
				fmt.Fprintf(os.Stderr, "harness: TUI unavailable (%v); continuing in plain mode.\n", err)
			}
			// execTUI replaces this process on success; reaching here
			// means it failed and we fall back to plain mode.
		} else if *ui == "tui" {
			fmt.Fprintln(os.Stderr, "harness: --ui tui needs a terminal; using plain mode.")
		}
	}

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

// execTUI replaces this process with the sibling harness-tui binary,
// passing the same flags through. It returns only if the sibling cannot
// be started (then the caller falls back to plain mode).
func execTUI(root, mode, model, api string, maxSteps int, resume, sessionID string, ctxLimit int) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	sibling := filepath.Join(filepath.Dir(self), "harness-tui")
	if _, err := os.Stat(sibling); err != nil {
		return fmt.Errorf("no harness-tui next to %s (build it with: go build -o <dir>/harness-tui ./cmd/harness-tui)", self)
	}
	args := []string{
		sibling,
		"-root", root,
		"-mode", mode,
		"-model", model,
		"-api", api,
		"-max-steps", strconv.Itoa(maxSteps),
		"-resume", resume,
		"-session", sessionID,
		"-context-limit", strconv.Itoa(ctxLimit),
	}
	return syscall.Exec(sibling, args, os.Environ())
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "harness:", err)
	os.Exit(1)
}
