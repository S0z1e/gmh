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
}

type App struct {
	Cfg          *agent.Config
	Store        *session.Store
	History      []llm.Message
	Skills       []skills.Skill
	ContextLimit int
	Root         string
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

	a.Cfg = &agent.Config{
		Client:   &llm.Client{BaseURL: baseURL, APIKey: apiKey, Model: mdl, API: apiMode},
		Exec:     &tools.Executor{Root: absRoot},
		Tools:    agent.ToolDefs(),
		System:   system,
		Mode:     mode,
		MaxSteps: p.MaxSteps,
		Session:  store,
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
	return a, nil
}

// Runner executes one line at a time: a task, or a slash command.
// Print is the line printer (plain: fmt.Printf; TUI: program.Println).
// Session totals accumulate across turns; no fee is ever estimated
// (OpenConsole's real price is unverified).
type Runner struct {
	App          *App
	SessionUsage llm.Usage
	Print        func(format string, args ...any)
}

// Handle runs one input line. It returns true when the UI should quit.
func (r *Runner) Handle(ctx context.Context, line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	switch line {
	case "/quit", "/exit":
		return true
	case "/usage":
		r.Print("[usage] session: %s | fee: OpenConsole 實價未驗證", r.SessionUsage)
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

func (r *Runner) runOnce(ctx context.Context, task string) {
	// /skill:<name> loads the full skill text into the next turn.
	if strings.HasPrefix(task, "/skill:") {
		name := strings.TrimPrefix(task, "/skill:")
		for _, sk := range r.App.Skills {
			if sk.Name == name {
				body, err := sk.Body()
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
		body, err := sk.Body()
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
	r.Print("\n[usage] turn: %s", usage)
	r.Print("[usage] session: %s | fee: OpenConsole 實價未驗證", r.SessionUsage)
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
	r.Print("[compact] %d tokens used for the summary; history replaced by summary (%d chars). Original transcript saved to %s", cUsage.TotalTokens, len(summary), archivePath)
}
