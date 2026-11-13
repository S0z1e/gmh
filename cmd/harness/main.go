// Command harness is the plain CLI: -p print mode, interactive REPL,
// --resume. Stdlib-only: it never imports the TUI package.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
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
		maxSteps  = flag.Int("max-steps", 0, "max agent steps (0 = unlimited)")
		resume    = flag.String("resume", "", "resume session id")
		sessionID = flag.String("session", "", "session id (default: timestamp)")
		ctxLimit  = flag.Int("context-limit", 0, "context window in tokens for the compaction trigger (default: env OPENAI_CONTEXT_LIMIT, model contextWindow, config contextLimit, or 128000)")
		ui        = flag.String("ui", "auto", "ui: auto, plain or tui (ignored by -p, which is always plain)")
		deny      = flag.Bool("deny-instead-of-prompt", false, "refuse approvals without asking (also env OPENAI_DENY_INSTEAD_OF_PROMPT=1; -p always denies)")
		sandbox   = flag.Bool("sandbox", false, "run bash calls under bwrap (bubblewrap): the workspace is the only writable directory (also env HARNESS_SANDBOX=1 or config sandbox)")
		bashTO    = flag.Int("bash-timeout", 0, "default bash timeout in seconds (default 120, or env OPENAI_BASH_TIMEOUT_SECONDS)")
		bgThresh  = flag.Int("bg-threshold", -1, "seconds a bash call runs before it is backgrounded (-1 = env OPENAI_BG_THRESHOLD_SECONDS or 120s default, 0 = never)")
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
			if err := execTUI(*root, *mode, *model, *api, *provider, *config, *maxSteps, *resume, *sessionID, *ctxLimit, *deny, *bashTO, *bgThresh, *sandbox, images); err != nil {
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
		Sandbox:                    *sandbox,
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
		case agent.EventStopBlocked:
			fmt.Printf("stop hook blocked the turn end: %s\n", ev.Text)
		case agent.EventStepRetry:
			fmt.Printf("… LLM call failed, retrying: %s\n", ev.Text)
		case agent.EventStepDiscard:
			fmt.Printf("… discarded partial reply, retrying: %s\n", ev.Text)
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
	fmt.Println("Type a task, /image <path> to attach an image, /model to list models or /model <provider>/<model> to switch model, /sessions to list sessions, /resume <id> to switch session, /rename <old> <new> to rename one, /skill:<name> to load a skill, /usage for session totals, /jobs for background jobs, /clear to drop context, /compact to summarise context, /rollback [N] to drop the last N turns, or /quit to exit.")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	// The approval prompt shares the REPL's scanner: the scanner
	// buffers piped stdin ahead, so a fresh reader on os.Stdin in
	// the loop's default ask would see EOF after the first task
	// line. (The TUI and RPC paths set their own Ask and never
	// reach this code; -p denies without asking.)
	a.Cfg.Ask = askFromScanner(scanner)
	// Doom-loop continuation prompt, on the same scanner (see above):
	// once / always this session / stop. Editing the input is a GUI
	// feature; the CLI offers the three decisions.
	a.Cfg.ConfirmContinuation = confirmContinuationFromScanner(scanner)
	// Structured questions, on the same scanner: one line per
	// question — option numbers (comma-separated for multi-select),
	// or free text for "Other".
	a.Cfg.AskQuestion = askQuestionFromScanner(scanner)
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

// askFromScanner is the plain CLI's approval prompt: it reads the
// answer from the same scanner the REPL loop uses, so an answer
// piped after a task ("y" on the next line) is read from the
// already-buffered stream instead of a fresh reader that would see
// EOF. Interactive TTY input behaves the same either way — the
// terminal only delivers a line once it is typed — and type-ahead
// ("y" typed before the prompt appears) now works too, instead of
// being swallowed by the scanner's read-ahead.
func askFromScanner(scanner *bufio.Scanner) func(string) (bool, error) {
	return func(prompt string) (bool, error) {
		fmt.Printf("%s [y/N] ", prompt)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return false, err
			}
			return false, io.EOF
		}
		ans := strings.ToLower(strings.TrimSpace(scanner.Text()))
		return ans == "y" || ans == "yes", nil
	}
}

// confirmContinuationFromScanner is the plain CLI's doom-loop
// prompt. It shares the REPL scanner with askFromScanner, for the
// same buffered-stdin reason.
func confirmContinuationFromScanner(scanner *bufio.Scanner) func(agent.ContinuationRequest) (agent.ContinuationDecision, error) {
	return func(req agent.ContinuationRequest) (agent.ContinuationDecision, error) {
		fmt.Printf("Repeated tool call: %s %s was requested %d times with identical input. Continue once (o), always this session (s), or stop (d)? [o/s/d] ", req.ToolName, req.InputPreview, req.RepeatCount)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return agent.ContinuationDecision{Outcome: agent.ContinuationDeny}, err
			}
			return agent.ContinuationDecision{Outcome: agent.ContinuationDeny}, io.EOF
		}
		switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
		case "o", "once", "y", "yes":
			return agent.ContinuationDecision{Outcome: agent.ContinuationOnce}, nil
		case "s", "session", "always":
			return agent.ContinuationDecision{Outcome: agent.ContinuationSession}, nil
		default:
			return agent.ContinuationDecision{Outcome: agent.ContinuationDeny}, nil
		}
	}
}

// askQuestionFromScanner is the plain CLI's ask_user prompt.
func askQuestionFromScanner(scanner *bufio.Scanner) func(agent.QuestionRequest) (agent.QuestionAnswers, error) {
	return func(req agent.QuestionRequest) (agent.QuestionAnswers, error) {
		var answers []agent.QuestionAnswer
		for i, q := range req.Questions {
			if q.Header != "" {
				fmt.Printf("%s\n", q.Header)
			}
			fmt.Printf("Q%d: %s\n", i+1, q.Question)
			for j, o := range q.Options {
				if o.Description != "" {
					fmt.Printf("  %d. %s — %s\n", j+1, o.Label, o.Description)
				} else {
					fmt.Printf("  %d. %s\n", j+1, o.Label)
				}
			}
			fmt.Printf("  (pick %s, or type your own answer) > ", map[bool]string{true: "numbers, comma-separated", false: "a number"}[q.MultiSelect])
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return agent.QuestionAnswers{}, err
				}
				return agent.QuestionAnswers{}, io.EOF
			}
			line := strings.TrimSpace(scanner.Text())
			ans := agent.QuestionAnswer{Question: i}
			// Numbers select options; anything else is free text.
			parts := strings.Split(line, ",")
			allNumbers := len(parts) > 0
			var picked []string
			for _, p := range parts {
				p = strings.TrimSpace(p)
				n, err := strconv.Atoi(p)
				if err != nil || n < 1 || n > len(q.Options) {
					allNumbers = false
					break
				}
				picked = append(picked, q.Options[n-1].Label)
			}
			if allNumbers {
				if !q.MultiSelect && len(picked) > 1 {
					picked = picked[:1]
				}
				ans.Selected = picked
			} else {
				ans.Custom = line
			}
			answers = append(answers, ans)
		}
		return agent.QuestionAnswers{Answers: answers}, nil
	}
}

// execTUI replaces this process with the sibling harness-tui binary,
// passing the same flags through. It returns only if the sibling cannot
// be started (then the caller falls back to plain mode).
func execTUI(root, mode, model, api, provider, config string, maxSteps int, resume, sessionID string, ctxLimit int, deny bool, bashTO, bgThresh int, sandbox bool, images []string) error {
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
		"-sandbox", strconv.FormatBool(sandbox),
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
