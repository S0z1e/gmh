// Package app holds the setup and turn-running logic shared by the plain
// CLI (cmd/harness) and the TUI (cmd/harness-tui). It never imports any
// UI code: the caller supplies the event renderer and the line printer.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"minimalharness/internal/agent"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/skills"
	"minimalharness/internal/tools"
)

type Params struct {
	Root         string
	Mode         string // suggest (ask) or auto
	Model        string
	API          string // chat or responses
	MaxSteps     int
	ContextLimit int
	Resume       string
	SessionID    string
	// DenyInsteadOfPrompt refuses approvals without asking. -p print
	// mode sets it; the flag and OPENAI_DENY_INSTEAD_OF_PROMPT do too.
	DenyInsteadOfPrompt bool
	// BashTimeoutSeconds overrides the default 60s bash timeout
	// (0 = default, or OPENAI_BASH_TIMEOUT_SECONDS).
	BashTimeoutSeconds int
	// BackgroundThresholdSeconds is how long a bash call runs before
	// the loop backgrounds it: -1 = unset (env OPENAI_BG_THRESHOLD_SECONDS
	// or the 10s default), 0 = never background, >0 = seconds.
	BackgroundThresholdSeconds int
	// DisableBackground turns backgrounding off entirely. -p print
	// mode sets it: a one-shot run stays synchronous.
	DisableBackground bool
}

type App struct {
	Cfg          *agent.Config
	Store        *session.Store
	History      []llm.Message
	Skills       []skills.Skill
	ContextLimit int
	Root         string
	// Rates configures cost estimates (zero = unconfigured: no estimate
	// is shown, only the "real price unverified" label).
	Rates llm.Rates
	// ResumedMessage is non-empty when --resume loaded a session.
	ResumedMessage string
}

// Setup builds the client, session and agent config. The caller sets
// Cfg.OnEvent (the event renderer) before running anything.
func Setup(p Params) (*App, error) {
	absRoot, err := filepath.Abs(p.Root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(absRoot); err != nil {
		return nil, fmt.Errorf("root not found: %s", absRoot)
	}

	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://www.openconsole.cn/v1"
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("missing OPENAI_API_KEY in environment")
	}
	mdl := p.Model
	if mdl == "" {
		mdl = os.Getenv("OPENAI_MODEL")
	}
	if mdl == "" {
		mdl = "deepseek-v4.1-flash"
	}
	apiMode := p.API
	if apiMode == "" {
		apiMode = os.Getenv("OPENAI_API_MODE")
	}
	if apiMode == "" {
		apiMode = "chat"
	}
	if apiMode != "chat" && apiMode != "responses" {
		return nil, fmt.Errorf("invalid -api %q: use chat or responses", apiMode)
	}
	mode := p.Mode
	if mode == "" {
		mode = "suggest"
	}

	id := p.SessionID
	if p.Resume != "" {
		id = p.Resume
	}
	store, err := session.New(id)
	if err != nil {
		return nil, err
	}

	a := &App{Store: store, Root: absRoot}
	if p.Resume != "" {
		history, err := session.Load(p.Resume)
		if err != nil {
			store.Close()
			return nil, err
		}
		a.History = history
		a.ResumedMessage = fmt.Sprintf("Resumed session %s (%d messages).", p.Resume, len(history))
	}

	skillList := skills.Discover(absRoot)
	a.Skills = skillList
	system := agent.SystemPrompt + skills.Listing(skillList)

	executor := &tools.Executor{Root: absRoot}
	if p.BashTimeoutSeconds > 0 {
		executor.BashTimeout = time.Duration(p.BashTimeoutSeconds) * time.Second
	} else if v := os.Getenv("OPENAI_BASH_TIMEOUT_SECONDS"); v != "" {
		var secs int
		if _, err := fmt.Sscanf(v, "%d", &secs); err == nil && secs > 0 {
			executor.BashTimeout = time.Duration(secs) * time.Second
		}
	}
	executor.Audit = func(e tools.AuditEntry) { _ = store.Audit(e) }

	// Background threshold: flag wins, else env, else the 10s default.
	// 0 disables backgrounding; -p disables it outright.
	bgThreshold := tools.DefaultBackgroundThreshold
	if p.BackgroundThresholdSeconds == 0 {
		bgThreshold = 0
	} else if p.BackgroundThresholdSeconds > 0 {
		bgThreshold = time.Duration(p.BackgroundThresholdSeconds) * time.Second
	} else if v := os.Getenv("OPENAI_BG_THRESHOLD_SECONDS"); v != "" {
		var secs int
		if _, err := fmt.Sscanf(v, "%d", &secs); err == nil {
			if secs == 0 {
				bgThreshold = 0
			} else if secs > 0 {
				bgThreshold = time.Duration(secs) * time.Second
			}
		}
	}
	if p.DisableBackground {
		bgThreshold = 0
	}
	executor.BackgroundThreshold = bgThreshold

	deny := p.DenyInsteadOfPrompt
	if v := os.Getenv("OPENAI_DENY_INSTEAD_OF_PROMPT"); v == "1" || strings.EqualFold(v, "true") {
		deny = true
	}

	a.Cfg = &agent.Config{
		Client:              &llm.Client{BaseURL: baseURL, APIKey: apiKey, Model: mdl, API: apiMode},
		Exec:                executor,
		Tools:               agent.ToolDefs(),
		System:              system,
		Mode:                mode,
		MaxSteps:            p.MaxSteps,
		Session:             store,
		DenyInsteadOfPrompt: deny,
	}

	contextLimit := p.ContextLimit
	if contextLimit <= 0 {
		if v := os.Getenv("OPENAI_CONTEXT_LIMIT"); v != "" {
			fmt.Sscanf(v, "%d", &contextLimit)
		}
	}
	if contextLimit <= 0 {
		contextLimit = agent.DefaultContextLimit
	}
	a.ContextLimit = contextLimit

	rates, err := llm.RatesFromEnv()
	if err != nil {
		store.Close()
		return nil, err
	}
	a.Rates = rates
	return a, nil
}

// Runner executes one line at a time: a task, or a slash command.
// Print is the line printer (plain: fmt.Printf; TUI: sent as a message
// so it stays ordered with the model's own scrollback prints).
// Session totals accumulate across turns. Cost is estimated only when
// rates are configured (App.Rates); the estimate is always labelled
// with its source, and OpenConsole's real price stays unverified.
type Runner struct {
	App          *App
	SessionUsage llm.Usage
	SessionCost  float64 // USD estimate, accumulated per turn
	Print        func(format string, args ...any)
}

// costLine renders the estimate suffix for a usage line, or the
// unverified label when no rates are configured.
func (r *Runner) costLine(cost float64) string {
	if !r.App.Rates.Configured {
		return "fee: OpenConsole 實價未驗證"
	}
	return fmt.Sprintf("est. cost: ~$%.4f (rates: %s; OpenConsole 實價未驗證)", cost, r.App.Rates.Source)
}

// Handle runs one input line. It returns true when the UI should quit.
func (r *Runner) Handle(ctx context.Context, line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	switch line {
	case "/quit", "/exit":
		// Quitting abandons running background jobs (their child
		// processes keep running, orphaned): say so plainly.
		var running []string
		for _, j := range r.App.Cfg.Exec.Jobs() {
			if j.Status() == tools.JobRunning {
				running = append(running, j.ID)
			}
		}
		if len(running) > 0 {
			r.Print("Background jobs still running (%s); quitting abandons them.", strings.Join(running, ", "))
		}
		return true
	case "/usage":
		r.Print("[usage] session: %s | %s", r.SessionUsage, r.costLine(r.SessionCost))
		return false
	case "/jobs":
		r.printJobs()
		return false
	case "/clear":
		if err := r.App.Store.Clear(); err != nil {
			r.Print("[error] %v", err)
			return false
		}
		r.App.History = nil
		r.Print("Context cleared. Cleared messages stay in %s but won't reload on --resume (it starts from the clear point).", r.App.Store.Path)
		return false
	case "/compact":
		r.doCompact(ctx)
		return false
	}
	r.runOnce(ctx, line)
	return false
}

// printJobs lists the background bash jobs of this process, oldest
// first: id, status, elapsed time, command, and exit code once done.
func (r *Runner) printJobs() {
	jobs := r.App.Cfg.Exec.Jobs()
	if len(jobs) == 0 {
		r.Print("No background jobs.")
		return
	}
	for _, j := range jobs {
		cmd := j.Command
		if len(cmd) > 80 {
			cmd = cmd[:77] + "…"
		}
		line := fmt.Sprintf("%s  %s  %s  %s", j.ID, j.Status(), j.Elapsed().Round(time.Second), cmd)
		if res, ok := j.Result(); ok {
			line += fmt.Sprintf("  exit %d", res.ExitCode)
			if res.Err != nil {
				line += fmt.Sprintf("  (%v)", res.Err)
			}
		}
		r.Print("%s", line)
	}
}

func (r *Runner) runOnce(ctx context.Context, task string) {
	// /skill:<name> loads the full skill text into the next turn.
	if strings.HasPrefix(task, "/skill:") {
		name := strings.TrimPrefix(task, "/skill:")
		for _, sk := range r.App.Skills {
			if sk.Name == name {
				body, err := sk.Load()
				if err == nil {
					r.Print("Loaded skill %s (%d bytes).", name, len(body))
					r.App.History = append(r.App.History, llm.Message{Role: "user", Content: "Skill " + name + ":\n" + body})
					_ = r.App.Store.Append(r.App.History[len(r.App.History)-1])
				}
				return
			}
		}
		r.Print("Unknown skill: %s", name)
		return
	}
	// Keyword trigger: if the task names a skill as a whole word, load
	// its full text into the turn before the model sees the task.
	for _, sk := range skills.MatchKeywords(task, r.App.Skills) {
		body, err := sk.Load()
		if err != nil {
			continue
		}
		r.Print("Auto-loaded skill %s (mentioned in your task).", sk.Name)
		skillMsg := llm.Message{Role: "user", Content: "Skill " + sk.Name + ":\n" + body}
		r.App.History = append(r.App.History, skillMsg)
		_ = r.App.Store.Append(skillMsg)
	}
	userMsg := llm.Message{Role: "user", Content: task}
	r.App.History = append(r.App.History, userMsg)
	_ = r.App.Store.Append(userMsg)
	_, usage, err := r.App.Cfg.Run(ctx, r.App.History)
	if err != nil {
		r.Print("\n[error] %v", err)
	}
	// Keep the returned history in sync: Run used its own copy.
	// Re-load the session file to recover tool messages.
	if msgs, err := session.Load(r.App.Store.ID); err == nil {
		r.App.History = msgs
	}
	r.SessionUsage.Add(usage)
	turnCost := r.App.Rates.Cost(usage, time.Now())
	r.SessionCost += turnCost
	// One Print call for both lines: consecutive tea.Println calls
	// reverse their order in Bubble Tea v2 (verified), so the turn
	// and session lines must travel as a single print.
	turnLine := fmt.Sprintf("[usage] turn: %s", usage)
	if r.App.Rates.Configured {
		turnLine += fmt.Sprintf(" | est. cost: ~$%.4f", turnCost)
	}
	r.Print("\n%s\n[usage] session: %s | %s", turnLine, r.SessionUsage, r.costLine(r.SessionCost))
	// Auto-compact when the last call's context hit 70% of the window.
	if agent.ShouldCompact(r.App.Cfg.LastPromptTokens, r.App.ContextLimit) {
		r.Print("[compact] context at %d tokens (limit %d), compacting…", r.App.Cfg.LastPromptTokens, r.App.ContextLimit)
		r.doCompact(ctx)
	}
}

// doCompact summarises history, saves the original transcript to a
// separate file, and replaces history with the summary.
func (r *Runner) doCompact(ctx context.Context) {
	if len(r.App.History) == 0 {
		r.Print("[compact] nothing to compact.")
		return
	}
	summary, cUsage, err := agent.Compact(ctx, r.App.Cfg.Client, r.App.History)
	if err != nil {
		r.Print("[compact] failed: %v (history unchanged)", err)
		return
	}
	archiveName := fmt.Sprintf("%s-pre-compact-%s", r.App.Store.ID, time.Now().Format("150405"))
	archivePath, err := r.App.Store.ArchiveCopy(archiveName)
	if err != nil {
		r.Print("[compact] failed to save transcript: %v (history unchanged)", err)
		return
	}
	if err := r.App.Store.MarkCompact(summary); err != nil {
		r.Print("[compact] failed: %v (history unchanged)", err)
		return
	}
	if msgs, err := session.Load(r.App.Store.ID); err == nil {
		r.App.History = msgs
	}
	r.SessionUsage.Add(cUsage)
	r.SessionCost += r.App.Rates.Cost(cUsage, time.Now())
	r.Print("[compact] %d tokens used for the summary; history replaced by summary (%d chars). Original transcript saved to %s", cUsage.TotalTokens, len(summary), archivePath)
}
