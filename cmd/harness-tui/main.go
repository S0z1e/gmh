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
	"minimalharness/internal/tui"
	"minimalharness/internal/uimode"
)

func main() {
	var (
		root      = flag.String("root", ".", "workspace root")
		mode      = flag.String("mode", "", "suggest (ask in a y/n dialog) or auto (default config or suggest)")
		model     = flag.String("model", "", "model id (default OPENAI_MODEL or config defaultModel)")
		api       = flag.String("api", "", "chat/openai-completions, responses/openai-responses or anthropic/anthropic-messages (default OPENAI_API_MODE or config)")
		provider  = flag.String("provider", "", "provider from the config file (default config defaultProvider)")
		config    = flag.String("config", "", "config file (default: ~/.minimal-harness/config.json + <root>/.harness.json)")
		maxSteps  = flag.Int("max-steps", 50, "max agent steps")
		resume    = flag.String("resume", "", "resume session id")
		sessionID = flag.String("session", "", "session id (default: timestamp)")
		ctxLimit  = flag.Int("context-limit", 0, "context window in tokens for the compaction trigger (default: env OPENAI_CONTEXT_LIMIT, model contextWindow, config contextLimit, or 128000)")
		deny      = flag.Bool("deny-instead-of-prompt", false, "refuse approvals without asking (also env OPENAI_DENY_INSTEAD_OF_PROMPT=1)")
		bashTO    = flag.Int("bash-timeout", 0, "default bash timeout in seconds (default 60, or env OPENAI_BASH_TIMEOUT_SECONDS)")
		bgThresh  = flag.Int("bg-threshold", -1, "seconds a bash call runs before it is backgrounded (-1 = env OPENAI_BG_THRESHOLD_SECONDS or 10s default, 0 = never)")
		images    stringList
	)
	flag.Var(&images, "image", "attach an image file to the first turn (repeatable; png, jpg, gif, webp; the model must declare image input)")
	flag.Parse()

	// The TUI needs a real terminal. When stdout is piped, say so
	// plainly instead of failing inside Bubble Tea's TTY open.
	if !uimode.IsTTY(os.Stdout) {
		fatal(fmt.Errorf("harness-tui needs a terminal; run harness (plain mode) or harness --ui plain instead"))
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
		DenyInsteadOfPrompt:        *deny,
		BashTimeoutSeconds:         *bashTO,
		BackgroundThresholdSeconds: *bgThresh,
		Images:                     images,
	})
	if err != nil {
		fatal(err)
	}
	defer a.Store.Close()

	// Header lines, opencode-mini style: a one-line mark plus the
	// session meta, both muted apart from the bold name and the plain
	// model. The model prints the block once at startup via Init, so
	// it stays at the top of the scrollback instead of sitting in the
	// live region after every turn. The help line lives in the input
	// placeholder now, not in the header.
	info := fmt.Sprintf("▪ harness · %s · %s\nroot %s · session %s · mode %s", a.Cfg.Client.API, a.Cfg.Client.Model, a.Root, a.Store.ID, a.Cfg.Mode)
	if a.ResumedMessage != "" {
		info += "\n" + a.ResumedMessage
	}
	lines := make(chan string, 16)
	// Theme: HARNESS_TUI_THEME picks default, terminal (no painted
	// background) or catppuccin-mocha; anything else is the default.
	// Setup already resolved env > config for this.
	tui.SetTheme(tui.ThemeByName(a.TUITheme))
	m := tui.NewModel(info).WithMeta(a.Cfg.Mode, a.Cfg.Client.API, a.Cfg.Client.Model).WithBranch(gitBranch(a.Root))
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

	// Runner output goes through the program's message queue (PrintMsg),
	// not prog.Println: direct prints race the model's own tea.Println
	// of the finished turn and can land above the reply text.
	runner := &app.Runner{App: a, Print: func(format string, args ...any) {
		prog.Send(tui.PrintMsg{Text: fmt.Sprintf(format, args...)})
	}}

	ctx := context.Background()
	go func() {
		for line := range lines {
			quit := runner.Handle(ctx, line)
			// Session totals for the meta line's right side, plus
			// the current model/api so a /model switch shows up.
			prog.Send(tui.MetaUpdateMsg{Tokens: runner.SessionUsage.TotalTokens, Turns: runner.Turns, Model: a.Model.ID, API: a.Cfg.Client.API})
			if quit {
				prog.Quit()
				return
			}
		}
	}()

	if _, err := prog.Run(); err != nil {
		fatal(err)
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

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "harness-tui:", err)
	os.Exit(1)
}

// stringList is a repeatable -image flag: each occurrence appends one path.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
