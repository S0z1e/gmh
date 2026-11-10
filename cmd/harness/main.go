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
	"minimalharness/internal/rpc"
	"minimalharness/internal/uimode"
)

func main() {
	var (
		printMode = flag.String("p", "", "print mode: run one task and exit")
		root      = flag.String("root", ".", "workspace root")
		mode      = flag.String("mode", "", "suggest (ask) or auto (default config or suggest)")
		model     = flag.String("model", "", "model id (default OPENAI_MODEL or config defaultModel)")
		api       = flag.String("api", "", "chat/openai-completions, responses/openai-responses or anthropic/anthropic-messages (default OPENAI_API_MODE or config)")
		provider  = flag.String("provider", "", "provider from the config file (default config defaultProvider)")
		config    = flag.String("config", "", "config file (default: ~/.minimal-harness/config.json + <root>/.harness.json)")
		maxSteps  = flag.Int("max-steps", 50, "max agent steps")
		resume    = flag.String("resume", "", "resume session id")
		sessionID = flag.String("session", "", "session id (default: timestamp)")
		ctxLimit  = flag.Int("context-limit", 0, "context window in tokens for the compaction trigger (default: env OPENAI_CONTEXT_LIMIT, model contextWindow, config contextLimit, or 128000)")
		ui        = flag.String("ui", "auto", "ui: auto, plain or tui (ignored by -p, which is always plain)")
		deny      = flag.Bool("deny-instead-of-prompt", false, "refuse approvals without asking (also env OPENAI_DENY_INSTEAD_OF_PROMPT=1; -p always denies)")
		bashTO    = flag.Int("bash-timeout", 0, "default bash timeout in seconds (default 60, or env OPENAI_BASH_TIMEOUT_SECONDS)")
		bgThresh  = flag.Int("bg-threshold", -1, "seconds a bash call runs before it is backgrounded (-1 = env OPENAI_BG_THRESHOLD_SECONDS or 10s default, 0 = never)")
		rpcMode   = flag.Bool("rpc", false, "RPC mode: JSONL over stdin/stdout for a GUI (see README); stdout carries protocol data only")
		images    stringList
	)
	flag.Var(&images, "image", "attach an image file to the first turn (repeatable; png, jpg, gif, webp; the model must declare image input)")
	flag.Parse()

	if *rpcMode && *printMode != "" {
		fatal(fmt.Errorf("--rpc conflicts with -p"))
	}

	// UI selection happens before setup: the TUI is a separate binary
	// (harness-tui) so the plain CLI never imports Bubble Tea.
	// -p print mode is always plain and ignores --ui; --rpc is a
	// machine frontend, not a terminal one, and skips this too.
	if *printMode == "" && !*rpcMode {
		uiMode, err := uimode.Decide(*ui, uimode.IsTTY(os.Stdout))
		if err != nil {
			fatal(err)
		}
		if uiMode == "tui" {
			if err := execTUI(*root, *mode, *model, *api, *provider, *config, *maxSteps, *resume, *sessionID, *ctxLimit, *deny, *bashTO, *bgThresh, images); err != nil {
				fmt.Fprintf(os.Stderr, "harness: TUI unavailable (%v); continuing in plain mode.\n", err)
			}
			// execTUI replaces this process on success; reaching here
			// means it failed and we fall back to plain mode.
		} else if *ui == "tui" {
			fmt.Fprintln(os.Stderr, "harness: --ui tui needs a terminal; using plain mode.")
		}
	}

	a, err := app.Setup(app.Params{
		Root:                       *root,
		Mode:                       *mode,
		Model:                      *model,
		API:                        *api,
		Provider:                   *provider,
		ConfigPath:                 *config,
		MaxSteps:                   *maxSteps,
		ContextLimit:               *ctxLimit,
		Resume:                     *resume,
		SessionID:                  *sessionID,
		DenyInsteadOfPrompt:        *deny || *printMode != "",
		BashTimeoutSeconds:         *bashTO,
		BackgroundThresholdSeconds: *bgThresh,
		DisableBackground:          *printMode != "",
		Images:                     images,
	})
	if err != nil {
		fatal(err)
	}
	defer a.Store.Close()

	runner := &app.Runner{App: a, Print: func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	}}

	// RPC mode: the server owns the event stream and the approval
	// bridge; usage lines and diagnostics go to stderr so stdout
	// stays pure JSONL. Nothing is printed here at all.
	if *rpcMode {
		runner.Print = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
		if err := rpc.Run(a, runner, os.Stdin, os.Stdout); err != nil {
			fatal(err)
		}
		return
	}

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
			return
		}
		if textPending {
			fmt.Println()
			textPending = false
		}
		switch ev.Kind {
		case agent.EventToolBackgrounded:
			fmt.Printf("… bash running in background (%s)\n", ev.JobID)
		case agent.EventBackgroundWait:
			fmt.Printf("waiting for background jobs: %s\n", ev.Text)
		case agent.EventToolBackgroundDone:
			if ev.ToolErr != nil {
				fmt.Printf("background job %s finished with error: %v\n", ev.JobID, ev.ToolErr)
			} else {
				fmt.Printf("background job %s finished\n", ev.JobID)
			}
		}
	}

	ctx := context.Background()
	fmt.Printf("harness v1  api=%s  model=%s  root=%s  mode=%s  session=%s\n", a.Cfg.Client.API, a.Cfg.Client.Model, a.Root, a.Cfg.Mode, a.Store.ID)

	if *printMode != "" {
		runner.Handle(ctx, *printMode)
		return
	}

	// REPL
	fmt.Println("Type a task, /image <path> to attach an image, /model to list models or /model <provider>/<model> to switch model, /sessions to list sessions, /resume <id> to switch session, /rename <old> <new> to rename one, /skill:<name> to load a skill, /usage for session totals, /jobs for background jobs, /clear to drop context, /compact to summarise context, or /quit to exit.")
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
func execTUI(root, mode, model, api, provider, config string, maxSteps int, resume, sessionID string, ctxLimit int, deny bool, bashTO, bgThresh int, images []string) error {
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
		"-provider", provider,
		"-config", config,
		"-max-steps", strconv.Itoa(maxSteps),
		"-resume", resume,
		"-session", sessionID,
		"-context-limit", strconv.Itoa(ctxLimit),
		"-deny-instead-of-prompt", strconv.FormatBool(deny),
		"-bash-timeout", strconv.Itoa(bashTO),
		"-bg-threshold", strconv.Itoa(bgThresh),
	}
	for _, img := range images {
		args = append(args, "-image", img)
	}
	return syscall.Exec(sibling, args, os.Environ())
}

// stringList is a repeatable -image flag: each occurrence appends one path.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "harness:", err)
	os.Exit(1)
}
