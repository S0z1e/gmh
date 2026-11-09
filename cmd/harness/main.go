// Command harness is the v1 CLI: -p print mode, interactive REPL, --resume.
package main

import (
	"bufio"
	"context"
	"flag"
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

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fatal(err)
	}
	if _, err := os.Stat(absRoot); err != nil {
		fatal(fmt.Errorf("root not found: %s", absRoot))
	}

	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://www.openconsole.cn/v1"
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fatal(fmt.Errorf("missing OPENAI_API_KEY in environment"))
	}
	mdl := *model
	if mdl == "" {
		mdl = os.Getenv("OPENAI_MODEL")
	}
	if mdl == "" {
		mdl = "deepseek-v4.1-flash"
	}
	apiMode := *api
	if apiMode == "" {
		apiMode = os.Getenv("OPENAI_API_MODE")
	}
	if apiMode == "" {
		apiMode = "chat"
	}
	if apiMode != "chat" && apiMode != "responses" {
		fatal(fmt.Errorf("invalid -api %q: use chat or responses", apiMode))
	}

	// Session: resume loads history, otherwise start a new store.
	id := *sessionID
	if *resume != "" {
		id = *resume
	}
	store, err := session.New(id)
	if err != nil {
		fatal(err)
	}
	defer store.Close()

	var history []llm.Message
	if *resume != "" {
		history, err = session.Load(*resume)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("Resumed session %s (%d messages).\n", *resume, len(history))
	}

	skillList := skills.Discover(absRoot)
	system := agent.SystemPrompt + skills.Listing(skillList)

	// Plain renderer: the CLI is one consumer of the loop's event stream.
	// It prints streamed text as it arrives and ends the line at the next
	// boundary event (tool call, usage, done, error) — the same output
	// shape as before the event seam.
	var textPending bool
	plainRenderer := func(ev agent.Event) {
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

	cfg := &agent.Config{
		Client:   &llm.Client{BaseURL: baseURL, APIKey: apiKey, Model: mdl, API: apiMode},
		Exec:     &tools.Executor{Root: absRoot},
		Tools:    agent.ToolDefs(),
		System:   system,
		Mode:     *mode,
		MaxSteps: *maxSteps,
		Session:  store,
		OnEvent:  plainRenderer,
	}
	_ = store // kept for clarity; cfg.Session owns it

	contextLimit := *ctxLimit
	if contextLimit <= 0 {
		if v := os.Getenv("OPENAI_CONTEXT_LIMIT"); v != "" {
			fmt.Sscanf(v, "%d", &contextLimit)
		}
	}
	if contextLimit <= 0 {
		contextLimit = agent.DefaultContextLimit
	}

	ctx := context.Background()
	fmt.Printf("harness v1  api=%s  model=%s  root=%s  mode=%s  session=%s\n", apiMode, mdl, absRoot, *mode, store.ID)

	// Session totals accumulate across turns (REPL) as well as within one
	// -p run. Fee is never estimated: OpenConsole's real price is unverified.
	var sessionUsage llm.Usage
	printUsage := func(turn llm.Usage) {
		fmt.Printf("\n[usage] turn: %s\n", turn)
		fmt.Printf("[usage] session: %s | fee: OpenConsole 實價未驗證\n", sessionUsage)
	}

	// doCompact summarises history, saves the original transcript to a
	// separate file, and replaces history with the summary.
	doCompact := func() {
		if len(history) == 0 {
			fmt.Println("[compact] nothing to compact.")
			return
		}
		summary, cUsage, err := agent.Compact(ctx, cfg.Client, history)
		if err != nil {
			fmt.Printf("[compact] failed: %v (history unchanged)\n", err)
			return
		}
		archiveName := fmt.Sprintf("%s-pre-compact-%s", store.ID, time.Now().Format("150405"))
		archivePath, err := store.ArchiveCopy(archiveName)
		if err != nil {
			fmt.Printf("[compact] failed to save transcript: %v (history unchanged)\n", err)
			return
		}
		if err := store.MarkCompact(summary); err != nil {
			fmt.Printf("[compact] failed: %v (history unchanged)\n", err)
			return
		}
		if msgs, err := session.Load(store.ID); err == nil {
			history = msgs
		}
		sessionUsage.Add(cUsage)
		fmt.Printf("[compact] %d tokens used for the summary; history replaced by summary (%d chars). Original transcript saved to %s\n", cUsage.TotalTokens, len(summary), archivePath)
	}

	runOnce := func(task string) {
		task = strings.TrimSpace(task)
		if task == "" {
			return
		}
		// /skill:<name> loads the full skill text into the next turn.
		if strings.HasPrefix(task, "/skill:") {
			name := strings.TrimPrefix(task, "/skill:")
			for _, sk := range skillList {
				if sk.Name == name {
					body, err := sk.Body()
					if err == nil {
						fmt.Printf("Loaded skill %s (%d bytes).\n", name, len(body))
						history = append(history, llm.Message{Role: "user", Content: "Skill " + name + ":\n" + body})
						_ = store.Append(history[len(history)-1])
					}
					return
				}
			}
			fmt.Printf("Unknown skill: %s\n", name)
			return
		}
		userMsg := llm.Message{Role: "user", Content: task}
		history = append(history, userMsg)
		_ = store.Append(userMsg)
		text, usage, err := cfg.Run(ctx, history)
		if err != nil {
			fmt.Printf("\n[error] %v\n", err)
		}
		// Keep the returned history in sync: Run used its own copy.
		// For v1, re-load the session file to recover tool messages.
		if msgs, err := session.Load(store.ID); err == nil {
			history = msgs
		}
		_ = text
		sessionUsage.Add(usage)
		printUsage(usage)
		// Auto-compact when the last call's context hit 70% of the window.
		if agent.ShouldCompact(cfg.LastPromptTokens, contextLimit) {
			fmt.Printf("[compact] context at %d tokens (limit %d), compacting…\n", cfg.LastPromptTokens, contextLimit)
			doCompact()
		}
	}

	if *printMode != "" {
		runOnce(*printMode)
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
		line := strings.TrimSpace(scanner.Text())
		if line == "/quit" || line == "/exit" {
			break
		}
		if line == "/usage" {
			fmt.Printf("[usage] session: %s | fee: OpenConsole 實價未驗證\n", sessionUsage)
			continue
		}
		if line == "/clear" {
			if err := store.Clear(); err != nil {
				fmt.Printf("[error] %v\n", err)
				continue
			}
			history = nil
			fmt.Printf("Context cleared. Cleared messages stay in %s but won't reload on --resume (it starts from the clear point).\n", store.Path)
			continue
		}
		if line == "/compact" {
			doCompact()
			continue
		}
		runOnce(line)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "harness:", err)
	os.Exit(1)
}
