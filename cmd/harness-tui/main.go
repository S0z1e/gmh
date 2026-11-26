// Command harness-tui is the Bubble Tea v2 interface for the harness.
// It is a separate binary so cmd/harness (and -p) never imports Bubble Tea.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"minimalharness/internal/agent"
	"minimalharness/internal/app"
	"minimalharness/internal/approvalmode"
	"minimalharness/internal/tui"
	"minimalharness/internal/uimode"
)

func main() {
	var (
		root           = flag.String("root", ".", "workspace root")
		workspaceTrust = flag.String("workspace-trust", "", "save workspace trust: trust or untrust (default: saved decision)")
		mode           = flag.String("mode", "", approvalmode.Choices+" (default ask-for-approval; legacy: suggest or auto)")
		model          = flag.String("model", "", "model id (default OPENAI_MODEL or config defaultModel)")
		api            = flag.String("api", "", "chat/openai-completions, responses/openai-responses or anthropic/anthropic-messages (default OPENAI_API_MODE or config)")
		provider       = flag.String("provider", "", "provider from the config file (default config defaultProvider)")
		config         = flag.String("config", "", "config file (default: ~/.minimal-harness/config.json + <root>/.harness.json)")
		maxSteps       = flag.Int("max-steps", 0, "max agent steps (0 = unlimited)")
		resume         = flag.String("resume", "", "resume session id")
		steal          = flag.Bool("steal", false, "with --resume, take a busy session after confirmation")
		rebindSession  = flag.Bool("rebind-session", false, "explicitly bind resumed or legacy history to this workspace")
		sessionID      = flag.String("session", "", "session id (default: timestamp)")
		ctxLimit       = flag.Int("context-limit", 0, "context window in tokens for the compaction trigger (default: env OPENAI_CONTEXT_LIMIT, model contextWindow, config contextLimit, or 128000)")
		deny           = flag.Bool("deny-instead-of-prompt", false, "refuse tool calls that need approval (also env OPENAI_DENY_INSTEAD_OF_PROMPT=1)")
		sandbox        = flag.Bool("sandbox", false, "run bash calls under bwrap (bubblewrap): the workspace is the only writable directory (also env HARNESS_SANDBOX=1 or config sandbox)")
		bashTO         = flag.Int("bash-timeout", 0, "default bash timeout in seconds (default 120, or env OPENAI_BASH_TIMEOUT_SECONDS)")
		bgThresh       = flag.Int("bg-threshold", -1, "seconds a bash call runs before it is backgrounded (-1 = env OPENAI_BG_THRESHOLD_SECONDS or 120s default, 0 = never)")
		bgMaxLifetime  = flag.Int("bg-max-lifetime", -1, "max lifetime in seconds for background:true jobs without a per-call timeout (-1 = env OPENAI_BACKGROUND_MAX_LIFETIME_SECONDS, config, or 7200s default, 0 = unlimited)")
		images         stringList
		files          stringList
	)
	flag.Var(&images, "image", "attach an image file to the first turn (repeatable; png, jpg, gif, webp; the model must declare image input)")
	flag.Var(&files, "attach", "attach a UTF-8 text file to the first turn (repeatable)")
	flag.Parse()

	// The TUI needs a real terminal. When stdout is piped, say so
	// plainly instead of failing inside Bubble Tea's TTY open.
	if !uimode.IsTTY(os.Stdout) {
		fatal(fmt.Errorf("harness-tui needs a terminal; run harness (plain mode) or harness --ui plain instead"))
	}
	if *steal && *resume == "" {
		fatal(fmt.Errorf("--steal requires --resume <session-id>"))
	}
	if *steal && !confirmStartupSteal(*resume) {
		fatal(fmt.Errorf("steal of session %q was not confirmed", *resume))
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
		Sandbox:                      *sandbox,
		BashTimeoutSeconds:           *bashTO,
		BackgroundThresholdSeconds:   *bgThresh,
		BackgroundMaxLifetimeSeconds: app.BackgroundMaxLifetimeSecondsFromFlag(*bgMaxLifetime),
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Header lines, opencode-mini style: a one-line mark plus the
	// session meta, both muted apart from the bold name and the plain
	// model. The model prints the block once at startup via Init, so
	// it stays at the top of the scrollback instead of sitting in the
	// live region after every turn. The help line lives in the input
	// placeholder now, not in the header.
	info := fmt.Sprintf("▪ harness · %s · %s\nroot %s · session %s · mode %s", a.Cfg.Client.API, a.Cfg.Client.Model, a.Root, a.Store.ID, a.Cfg.Mode)
	if !a.WorkspaceTrusted {
		info += "\nWorkspace untrusted: project config and plugins disabled (--workspace-trust trust)."
	}
	if a.ResumedMessage != "" {
		info += "\n" + a.ResumedMessage
	}
	if notice := a.OrphanedNotice(); notice != "" {
		info += "\n" + notice
	}
	lines := make(chan string, 16)
	// Theme: HARNESS_TUI_THEME picks default, terminal (no painted
	// background) or catppuccin-mocha; anything else is the default.
	// Setup already resolved env > config for this.
	tui.SetTheme(tui.ThemeByName(a.TUITheme))
	var approver *tui.Approver
	runner := &app.Runner{App: a, ConfirmSteal: func(ctx context.Context, id string) (bool, error) {
		return approver.ConfirmSteal(ctx, id)
	}}
	if err := runner.RefreshUsage(); err != nil {
		fatal(err)
	}
	m := tui.NewModel(info).WithMeta(a.Cfg.Mode, a.Cfg.Client.API, a.Cfg.Client.Model).WithBranch(gitBranch(a.Root)).WithSessionTotals(runner.SessionUsage.TotalTokens, runner.Turns).WithUsageCoverage(runner.SessionMetering.TokenCountsComplete())
	// Set OnSubmit BEFORE NewProgram: the program holds its own copy of
	// the model, so fields set afterwards on this copy would never fire.
	m.OnSubmit = func(line string) {
		select {
		case lines <- line:
		case <-ctx.Done():
		}
	}
	prog := tea.NewProgram(m)
	approver = tui.NewApprover(prog)

	// The agent loop runs in a goroutine and feeds events into the
	// program; Bubble Tea owns the main goroutine and stdin.
	a.Cfg.OnEvent = func(ev agent.Event) {
		prog.Send(tui.EventMsg{Event: ev})
	}
	// Keep the dialog available when /permissions changes the approval mode.
	a.Cfg.Ask = approver.Ask
	a.Cfg.ConfirmContinuation = approver.ConfirmContinuation

	// Runner output goes through the program's message queue (PrintMsg),
	// not prog.Println: direct prints race the model's own tea.Println
	// of the finished turn and can land above the reply text.
	runner.Print = func(format string, args ...any) { prog.Send(tui.PrintMsg{Text: fmt.Sprintf(format, args...)}) }

	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		for {
			var line string
			select {
			case <-ctx.Done():
				return
			case line = <-lines:
			}
			if ctx.Err() != nil {
				return
			}
			quit := runner.Handle(ctx, line)
			// Session totals for the meta line's right side, plus
			// the current model/api so a /model switch shows up,
			// and the tool mode so a /plan switch shows its chip.
			prog.Send(tui.MetaUpdateMsg{UsageIncomplete: !runner.SessionMetering.TokenCountsComplete(), UsageUnavailable: !runner.MeteringAvailable, Tokens: runner.SessionUsage.TotalTokens, Turns: runner.Turns, Model: a.Model.ID, API: a.Cfg.Client.API, ToolMode: string(a.ToolMode()), ApprovalMode: a.Cfg.Mode})
			if quit {
				prog.Quit()
				return
			}
		}
	}()

	_, runErr := prog.Run()
	cancel()
	<-runnerDone
	if err := a.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close session jobs: %v\n", err)
		if runErr == nil {
			runErr = err
		}
	}
	if runErr != nil {
		fatal(runErr)
	}
}

// gitBranch reads the current branch of the git checkout at root,
// or "" when root is not a checkout (or HEAD is detached, where the
// branch has no name to show). It reads .git/HEAD directly: no
// subprocess, no dependency on git being installed.
func gitBranch(root string) string {
	b, err := os.ReadFile(filepath.Join(root, ".git", "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(b))
	const prefix = "ref: refs/heads/"
	if strings.HasPrefix(head, prefix) {
		return strings.TrimPrefix(head, prefix)
	}
	return ""
}

// confirmStartupSteal asks in its own short-lived program so a refused steal
// never opens the session. The main TUI is created only after a yes.
func confirmStartupSteal(id string) bool {
	reply := make(chan bool, 1)
	m := tui.NewModel("").WithApproval(&tui.ApprovalRequest{
		ToolName: "steal",
		Prompt:   tui.StealPrompt(id),
		Reply:    reply,
	})
	p := tea.NewProgram(m)
	_, err := p.Run()
	select {
	case answered := <-reply:
		return err == nil && answered
	default:
		return false
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "harness-tui:", err)
	os.Exit(1)
}

// stringList appends one path per occurrence of a repeatable flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
