// Command harness is the plain CLI: -p print mode, interactive REPL,
// --resume. Stdlib-only: it never imports the TUI package.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"minimalharness/internal/agent"
	"minimalharness/internal/app"
	"minimalharness/internal/approvalmode"
	"minimalharness/internal/input"
	"minimalharness/internal/rpc"
	"minimalharness/internal/session"
	"minimalharness/internal/uimode"
)

func main() {
	var (
		printMode      = flag.String("p", "", "print mode: run one task and exit")
		root           = flag.String("root", ".", "workspace root")
		workspaceTrust = flag.String("workspace-trust", "", "save workspace trust: trust or untrust (default: saved decision)")
		mode           = flag.String("mode", "", approvalmode.Choices+" (default ask-for-approval; legacy: suggest or auto)")
		model          = flag.String("model", "", "model id (default OPENAI_MODEL or config defaultModel)")
		api            = flag.String("api", "", "chat/openai-completions, responses/openai-responses or anthropic/anthropic-messages (default OPENAI_API_MODE or config)")
		provider       = flag.String("provider", "", "provider from the config file (default config defaultProvider)")
		config         = flag.String("config", "", "config file (default: ~/.minimal-harness/config.json + <root>/.harness.json)")
		maxSteps       = flag.Int("max-steps", 0, "max agent steps (0 = unlimited)")
		viewSessionID  = flag.String("view-session", "", "read a session as JSONL without opening a writer or loading providers")
		resume         = flag.String("resume", "", "resume session id")
		steal          = flag.Bool("steal", false, "with --resume, take a busy session from a proven stalled or dead owner (asks before signalling a live process)")
		rebindSession  = flag.Bool("rebind-session", false, "explicitly bind resumed or legacy history to this workspace")
		sessionID      = flag.String("session", "", "session id (default: timestamp)")
		ctxLimit       = flag.Int("context-limit", 0, "context window in tokens for the compaction trigger (default: env OPENAI_CONTEXT_LIMIT, model contextWindow, config contextLimit, or 128000)")
		ui             = flag.String("ui", "auto", "ui: auto, plain or tui (ignored by -p, which is always plain)")
		deny           = flag.Bool("deny-instead-of-prompt", false, "refuse tool calls that need approval (also env OPENAI_DENY_INSTEAD_OF_PROMPT=1)")
		sandbox        = flag.Bool("sandbox", false, "run bash calls under bwrap (bubblewrap): the workspace is the only writable directory (also env HARNESS_SANDBOX=1 or config sandbox)")
		bashTO         = flag.Int("bash-timeout", 0, "default bash timeout in seconds (default 120, or env OPENAI_BASH_TIMEOUT_SECONDS)")
		bgThresh       = flag.Int("bg-threshold", -1, "seconds a bash call runs before it is backgrounded (-1 = env OPENAI_BG_THRESHOLD_SECONDS or 120s default, 0 = never)")
		bgMaxLifetime  = flag.Int("bg-max-lifetime", -1, "max lifetime in seconds for background:true jobs without a per-call timeout (-1 = env OPENAI_BACKGROUND_MAX_LIFETIME_SECONDS, config, or 7200s default, 0 = unlimited)")
		rpcMode        = flag.Bool("rpc", false, "RPC mode: JSONL over stdin/stdout for a GUI (see README); stdout carries protocol data only")
		images         stringList
		files          stringList
	)
	flag.Var(&images, "image", "attach an image file to the first turn (repeatable; png, jpg, gif, webp; the model must declare image input)")
	flag.Var(&files, "attach", "attach a UTF-8 text file to the first turn (repeatable)")
	flag.Parse()
	if len(flag.Args()) > 0 && flag.Arg(0) == "usage-repair" {
		runUsageRepair(flag.Args()[1:])
		return
	}
	if *viewSessionID != "" {
		if *rpcMode || *printMode != "" || *resume != "" || *steal || *sessionID != "" || *rebindSession || len(files) > 0 {
			fatal(fmt.Errorf("--view-session conflicts with --rpc, -p, --resume, --steal, --session and -attach"))
		}
		if err := viewSession(os.Stdout, *viewSessionID); err != nil {
			fatal(err)
		}
		return
	}

	if *rpcMode && *printMode != "" {
		fatal(fmt.Errorf("--rpc conflicts with -p"))
	}
	if *steal && *resume == "" {
		fatal(fmt.Errorf("--steal requires --resume <session-id>"))
	}
	if *steal && *rpcMode {
		fatal(fmt.Errorf("--steal is a confirmed takeover; RPC clients send steal_session instead"))
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
			if err := execTUI(*root, *mode, *model, *api, *provider, *config, *maxSteps, *resume, *steal, *sessionID, *ctxLimit, *deny, *bashTO, *bgThresh, *bgMaxLifetime, *sandbox, images, files, *workspaceTrust, *rebindSession); err != nil {
				fmt.Fprintf(os.Stderr, "harness: TUI unavailable (%v); continuing in plain mode.\n", err)
			} else {
				return // Windows waits for the child rather than replacing this process.
			}
		} else if *ui == "tui" {
			fmt.Fprintln(os.Stderr, "harness: --ui tui needs a terminal; using plain mode.")
		}
	}
	// The TUI confirms in its own dialog and returns before this process
	// opens the session. Every other path asks on one scanner, which the REPL
	// keeps, so a piped answer is not swallowed by a second reader.
	var sessionInput *input.Lines
	if *steal {
		sessionInput = input.New(os.Stdin)
		defer sessionInput.Close()
		ok, err := confirmFrom(sessionInput, os.Stderr, context.Background(), *resume)
		if err != nil && err != io.EOF {
			fatal(err)
		}
		if err != nil || !ok {
			fatal(fmt.Errorf("steal of session %q was not confirmed", *resume))
		}
	}

	a, err := app.Setup(app.Params{
		Root:                         *root,
		WorkspaceTrust:               *workspaceTrust,
		Mode:                         *mode,
		Model:                        *model,
		API:                          *api,
		Provider:                     *provider,
		ConfigPath:                   *config,
		MaxSteps:                     *maxSteps,
		ContextLimit:                 *ctxLimit,
		Resume:                       *resume,
		Steal:                        *steal,
		RebindSession:                *rebindSession,
		SessionID:                    *sessionID,
		DenyInsteadOfPrompt:          *deny,
		Headless:                     *printMode != "",
		Sandbox:                      *sandbox,
		BashTimeoutSeconds:           *bashTO,
		BackgroundThresholdSeconds:   *bgThresh,
		BackgroundMaxLifetimeSeconds: app.BackgroundMaxLifetimeSecondsFromFlag(*bgMaxLifetime),
		DisableBackground:            *printMode != "",
		Images:                       images,
		Files:                        files,
	})
	if err != nil {
		fatal(err)
	}
	defer func() {
		if err := a.Close(); err != nil {
			fatal(err)
		}
	}()

	runner := &app.Runner{App: a, Print: func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	}}
	if err := runner.RefreshUsage(); err != nil {
		fatal(err)
	}

	// RPC mode: the server owns the event stream and the approval
	// bridge; usage lines and diagnostics go to stderr so stdout
	// stays pure JSONL. Nothing is printed here at all.
	if *rpcMode {
		runner.Print = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
		if err := rpc.Run(a, runner, os.Stdin, os.Stdout); err != nil {
			_ = a.Close()
			fatal(err)
		}
		return
	}

	if !a.WorkspaceTrusted {
		fmt.Fprintln(os.Stderr, "Workspace untrusted: project config and plugins disabled. Use --workspace-trust trust to enable them.")
	}
	if a.ResumedMessage != "" {
		fmt.Println(a.ResumedMessage)
	}
	if notice := a.OrphanedNotice(); notice != "" {
		if *printMode != "" {
			fmt.Fprintln(os.Stderr, notice)
		} else {
			fmt.Println(notice)
		}
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
		case agent.EventApprovalReview:
			fmt.Printf("[approval] %s: %s\n", ev.ToolName, ev.Text)
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
		if runner.LastErr != nil {
			if err := a.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "close session jobs: %v\n", err)
			}
			os.Exit(1)
		}
		return
	}

	// REPL
	fmt.Println("Type a task, /attach <path> to attach a text file, /image <path> to attach an image, /model to list models or /model <provider>/<model> to switch model, /permissions [mode] to view or change approval mode, /sessions to list sessions, /resume <id> to switch session, /steal <id> to take a busy session after confirmation, /rename <old> <new> to rename one, /skill:<name> to load a skill, /usage for session totals, /jobs for background jobs, /prune-artifacts to remove unreferenced images older than seven days, /clear to drop context, /compact to summarise context, /rollback [N] to drop the last N turns, or /quit to exit.")
	scanner := sessionInput
	if scanner == nil {
		scanner = input.New(os.Stdin)
		defer scanner.Close()
	}
	// The approval prompt shares the REPL's scanner: the scanner
	// buffers piped stdin ahead, so a fresh reader on os.Stdin in
	// the loop's default ask would see EOF after the first task
	// line. (The TUI and RPC paths set their own Ask and never
	// reach this code; -p has no manual approval prompt.)
	a.Cfg.Ask = askFromScanner(scanner)
	// Doom-loop continuation prompt, on the same scanner (see above):
	// once / always this session / stop. Editing the input is a GUI
	// feature; the CLI offers the three decisions.
	a.Cfg.ConfirmContinuation = confirmContinuationFromScanner(scanner)
	// Structured questions, on the same scanner: one line per
	// question — option numbers (comma-separated for multi-select),
	// or free text for "Other".
	a.Cfg.AskQuestion = askQuestionFromScanner(scanner)
	runner.ConfirmSteal = confirmStealFromScanner(scanner)
	for {
		fmt.Print("> ")
		line, err := scanner.Read(ctx)
		if err != nil {
			break
		}
		if runner.Handle(ctx, strings.TrimSpace(line)) {
			break
		}
	}
}

// confirmSteal is the explicit confirmation --steal requires before Setup may
// signal a live owner. A non-interactive reader still has to answer "y"; EOF
// or anything else refuses the steal.
func confirmSteal(in io.Reader, out io.Writer, id string) bool {
	scanner := input.New(in)
	defer scanner.Close()
	ok, err := confirmFrom(scanner, out, context.Background(), id)
	return err == nil && ok
}

// confirmStealFromScanner is the REPL form. It shares the REPL scanner so a
// piped answer is not lost to a second reader on os.Stdin.
func confirmStealFromScanner(scanner *input.Lines) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, id string) (bool, error) {
		return confirmFrom(scanner, os.Stderr, ctx, id)
	}
}

func confirmFrom(scanner *input.Lines, out io.Writer, ctx context.Context, id string) (bool, error) {
	fmt.Fprintf(out, "Steal session %s? This ends a stalled owner when its process identity is proven, and refuses when it is not. [y/N] ", id)
	line, err := scanner.Read(ctx)
	if err != nil {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

// askFromScanner is the plain CLI's approval prompt: it reads the
// answer from the same scanner the REPL loop uses, so an answer
// piped after a task ("y" on the next line) is read from the
// already-buffered stream instead of a fresh reader that would see
// EOF. Interactive TTY input behaves the same either way — the
// terminal only delivers a line once it is typed — and type-ahead
// ("y" typed before the prompt appears) now works too, instead of
// being swallowed by the scanner's read-ahead.
func askFromScanner(scanner *input.Lines) func(agent.ApprovalRequest) (bool, error) {
	return func(req agent.ApprovalRequest) (bool, error) {
		fmt.Printf("%s [y/N] ", req.Prompt())
		line, err := scanner.Read(req.Context)
		if err != nil {
			return false, err
		}
		ans := strings.ToLower(strings.TrimSpace(line))
		return ans == "y" || ans == "yes", nil
	}
}

// confirmContinuationFromScanner is the plain CLI's doom-loop
// prompt. It shares the REPL scanner with askFromScanner, for the
// same buffered-stdin reason.
func confirmContinuationFromScanner(scanner *input.Lines) func(agent.ContinuationRequest) (agent.ContinuationDecision, error) {
	return func(req agent.ContinuationRequest) (agent.ContinuationDecision, error) {
		fmt.Printf("Repeated tool call: %s %s was requested %d times with identical input. Continue once (o), always this session (s), or stop (d)? [o/s/d] ", req.ToolName, req.InputPreview, req.RepeatCount)
		line, err := scanner.Read(req.Context)
		if err != nil {
			return agent.ContinuationDecision{Outcome: agent.ContinuationDeny}, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
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
func askQuestionFromScanner(scanner *input.Lines) func(agent.QuestionRequest) (agent.QuestionAnswers, error) {
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
			line, err := scanner.Read(req.Context)
			if err != nil {
				return agent.QuestionAnswers{}, err
			}
			line = strings.TrimSpace(line)
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
// passing the same flags through. Windows waits for that process to finish.
func execTUI(root, mode, model, api, provider, config string, maxSteps int, resume string, steal bool, sessionID string, ctxLimit int, deny bool, bashTO, bgThresh, bgMaxLifetime int, sandbox bool, images, files []string, workspaceTrust string, rebindSession bool) error {
	sibling, err := tuiSibling()
	if err != nil {
		return err
	}
	return runTUIProcess(sibling, tuiArgs(sibling, root, mode, model, api, provider, config, maxSteps, resume, steal, sessionID, ctxLimit, deny, bashTO, bgThresh, bgMaxLifetime, sandbox, images, files, workspaceTrust, rebindSession))
}

func tuiArgs(sibling, root, mode, model, api, provider, config string, maxSteps int, resume string, steal bool, sessionID string, ctxLimit int, deny bool, bashTO, bgThresh, bgMaxLifetime int, sandbox bool, images, files []string, workspaceTrust string, rebindSession bool) []string {
	args := []string{
		sibling,
		"-root", root,
		"-workspace-trust", workspaceTrust,
		"-rebind-session=" + strconv.FormatBool(rebindSession),
		"-mode", mode,
		"-model", model,
		"-api", api,
		"-provider", provider,
		"-config", config,
		"-max-steps", strconv.Itoa(maxSteps),
		"-resume", resume,
		"-steal=" + strconv.FormatBool(steal),
		"-session", sessionID,
		"-context-limit", strconv.Itoa(ctxLimit),
		"-deny-instead-of-prompt=" + strconv.FormatBool(deny),
		"-bash-timeout", strconv.Itoa(bashTO),
		"-bg-threshold", strconv.Itoa(bgThresh),
		"-bg-max-lifetime", strconv.Itoa(bgMaxLifetime),
		"-sandbox=" + strconv.FormatBool(sandbox),
	}
	for _, img := range images {
		args = append(args, "-image", img)
	}
	for _, path := range files {
		args = append(args, "-attach", path)
	}
	return args
}

// stringList appends one path per occurrence of a repeatable flag.
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

// viewSession has no writable Store, provider configuration or tool executor.
func viewSession(w io.Writer, id string) error {
	msgs, err := session.Load(id)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			return err
		}
	}
	return nil
}

func tuiSibling() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	sibling := filepath.Join(filepath.Dir(self), tuiBinaryName)
	st, err := os.Stat(sibling)
	if err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("no harness-tui next to %s (install both binaries from the same release archive)", self)
	}
	return sibling, nil
}
