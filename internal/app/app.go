// Package app holds the setup and turn-running logic shared by the plain
// CLI (cmd/harness) and the TUI (cmd/harness-tui). It never imports any
// UI code: the caller supplies the event renderer and the line printer.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"minimalharness/internal/agent"
	"minimalharness/internal/config"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/skills"
	"minimalharness/internal/tools"
)

type Params struct {
	Root         string
	Mode         string // suggest (ask) or auto
	Model        string
	API          string // chat, responses or anthropic
	MaxSteps     int
	ContextLimit int
	Resume       string
	SessionID    string
	// Provider selects a provider entry from the config file
	// (defaultProvider when empty).
	Provider string
	// ConfigPath names one config file explicitly. Empty means
	// auto-discover: ~/.minimal-harness/config.json, then
	// <root>/.harness.json on top of it.
	ConfigPath string
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
	// Images are image files attached to the first turn (-image flag,
	// repeatable). They are loaded in Setup and fail the run when a
	// file is missing, too big, or the model has no image input.
	Images []string
}

type App struct {
	Cfg          *agent.Config
	Store        *session.Store
	History      []llm.Message
	Skills       []skills.Skill
	ContextLimit int
	Root         string
	// Model is the resolved model entry (Pi shape): id, api, baseUrl,
	// contextWindow, maxTokens, samplingParams, headers, input, cost.
	Model config.Model
	// Config is the loaded config file, kept so /model can re-resolve
	// a different provider or model mid-session. Provider is the
	// current provider's name ("" before Setup).
	Config   *config.File
	Provider string
	// Rates configures cost estimates (zero = unconfigured: no estimate
	// is shown, only the "real price unverified" label).
	Rates llm.Rates
	// ResumedMessage is non-empty when --resume loaded a session.
	ResumedMessage string
	// TUITheme is the resolved TUI theme name: HARNESS_TUI_THEME,
	// else the config file, else "" (the default theme).
	TUITheme string
	// PendingImages are images attached for the next turn (/image,
	// -image, RPC attach_image). runOnce moves them onto the task
	// message and clears the list; the images then live in history
	// and are resent on later calls, like any message.
	PendingImages []llm.Image
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

	// Config: flag > env > project file > global file > built-in
	// default. A bad config file fails fast, before any session
	// is created. The API key is never read from a file.
	var cfg *config.File
	if p.ConfigPath != "" {
		var err error
		cfg, err = config.LoadExplicit(p.ConfigPath)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		cfg, err = config.LoadFor(absRoot)
		if err != nil {
			return nil, err
		}
	}
	prov, ok := cfg.Select(p.Provider)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (configured: %s)", p.Provider, strings.Join(cfg.ProviderNames(), ", "))
	}
	provName := cfg.SelectedName(p.Provider)
	if provName == "" {
		provName = "default"
	}

	// A provider must be configured by the user: there is no
	// built-in default provider any more.
	if !cfg.HasProviders() {
		global, _ := config.GlobalPath()
		return nil, fmt.Errorf("no providers configured: add a provider to %s (global) or %s (project)", global, config.ProjectPath(absRoot))
	}

	// API key: the provider's apiKey "$NAME" reference wins, else
	// OPENAI_API_KEY. A literal key never lives in the file.
	apiKey := ""
	if prov.APIKey != "" {
		k, err := config.ResolveAPIKey(prov.APIKey)
		if err != nil {
			return nil, err
		}
		apiKey = k
	} else {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("missing API key: set OPENAI_API_KEY, or set apiKey: \"$NAME\" for provider %q in the config file", provName)
	}

	// Model: flag > env > defaultModel (when the selected provider
	// is the default one) > provider default > first models entry.
	modelID := p.Model
	if modelID == "" {
		modelID = os.Getenv("OPENAI_MODEL")
	}
	if modelID == "" && (p.Provider == "" || p.Provider == cfg.DefaultProvider) {
		modelID = cfg.DefaultModel
	}
	mdl, ok := prov.ResolveModel(modelID)
	if !ok {
		return nil, fmt.Errorf("unknown model %q for provider %q (models: %s)", modelID, provName, modelIDs(prov))
	}
	if mdl.ID == "" {
		return nil, fmt.Errorf("provider %q: no model configured (set \"model\" or add a \"models\" entry)", provName)
	}

	// API shape: flag > env > model/provider. A provider needs an
	// api at provider or model level (Pi); there is no silent default.
	rawAPI := p.API
	if rawAPI == "" {
		rawAPI = os.Getenv("OPENAI_API_MODE")
	}
	if rawAPI == "" {
		rawAPI = mdl.API
	}
	if rawAPI == "" {
		return nil, fmt.Errorf("provider %q: api is required (chat/openai-completions, responses/openai-responses or anthropic/anthropic-messages)", provName)
	}
	apiMode, err := config.NormalizeAPI(rawAPI)
	if err != nil {
		return nil, fmt.Errorf("invalid -api: %w", err)
	}

	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = mdl.BaseURL
	}
	if baseURL == "" {
		return nil, fmt.Errorf("provider %q: baseUrl is required", provName)
	}
	mode := p.Mode
	if mode == "" {
		mode = cfg.Mode
	}
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
	system := agent.SystemPrompt + agent.ProjectContext(absRoot) + skills.Listing(skillList)

	executor := &tools.Executor{Root: absRoot, ImageInput: slices.Contains(mdl.Input, "image")}
	if p.BashTimeoutSeconds > 0 {
		executor.BashTimeout = time.Duration(p.BashTimeoutSeconds) * time.Second
	} else if v := os.Getenv("OPENAI_BASH_TIMEOUT_SECONDS"); v != "" {
		var secs int
		if _, err := fmt.Sscanf(v, "%d", &secs); err == nil && secs > 0 {
			executor.BashTimeout = time.Duration(secs) * time.Second
		}
	} else if cfg.BashTimeoutSeconds != nil {
		executor.BashTimeout = time.Duration(*cfg.BashTimeoutSeconds) * time.Second
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
	} else if cfg.BgThresholdSeconds != nil {
		if *cfg.BgThresholdSeconds == 0 {
			bgThreshold = 0
		} else {
			bgThreshold = time.Duration(*cfg.BgThresholdSeconds) * time.Second
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
	if cfg.DenyInsteadOfPrompt != nil && *cfg.DenyInsteadOfPrompt {
		deny = true
	}

	// MaxTokens: env > model > config > Pi default 16384. All three
	// modes send it now: chat as max_tokens, responses as
	// max_output_tokens, anthropic as max_tokens.
	maxTokens := 0
	if v := os.Getenv("ANTHROPIC_MAX_TOKENS"); v != "" {
		fmt.Sscanf(v, "%d", &maxTokens)
	} else if mdl.MaxTokens != nil {
		maxTokens = *mdl.MaxTokens
	} else if cfg.AnthropicMaxTokens != nil {
		maxTokens = *cfg.AnthropicMaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = config.DefaultMaxTokens
	}

	// Headers: provider headers, model headers on top. A "$NAME"
	// value reads the environment; literals pass through.
	var headers map[string]string
	merged := map[string]string{}
	for k, v := range prov.Headers {
		merged[k] = v
	}
	for k, v := range mdl.Headers {
		merged[k] = v
	}
	if len(merged) > 0 {
		headers = make(map[string]string, len(merged))
		for k, v := range merged {
			rv, err := config.ResolveValue(v)
			if err != nil {
				return nil, err
			}
			headers[k] = rv
		}
	}

	a.Model = mdl
	a.Config = cfg
	a.Provider = provName
	a.Cfg = &agent.Config{
		Client:              &llm.Client{BaseURL: baseURL, APIKey: apiKey, Model: mdl.ID, API: apiMode, MaxTokens: maxTokens, SamplingParams: mdl.SamplingParams, Headers: headers},
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
	if contextLimit <= 0 && mdl.ContextWindow != nil {
		contextLimit = *mdl.ContextWindow
	}
	if contextLimit <= 0 && cfg.ContextLimit != nil {
		contextLimit = *cfg.ContextLimit
	}
	if contextLimit <= 0 {
		contextLimit = config.DefaultContextWindow
	}
	a.ContextLimit = contextLimit

	a.TUITheme = os.Getenv("HARNESS_TUI_THEME")
	if a.TUITheme == "" {
		a.TUITheme = cfg.TUITheme
	}

	rates, err := llm.RatesFromEnv()
	if err != nil {
		store.Close()
		return nil, err
	}
	a.Rates = rates

	// Images stored in a resumed session carry only paths; re-read
	// their bytes now, while the executor exists.
	a.hydrateImages(a.History)
	// -image attachments fail the run when a file cannot be loaded.
	for _, p := range p.Images {
		if _, err := a.AttachImage(p); err != nil {
			store.Close()
			return nil, fmt.Errorf("-image %s: %w", p, err)
		}
	}
	return a, nil
}

// AttachImage loads one image file and queues it for the next turn.
// At most MaxImagesPerMessage wait for a turn.
func (a *App) AttachImage(path string) (llm.Image, error) {
	if len(a.PendingImages) >= tools.MaxImagesPerMessage {
		return llm.Image{}, fmt.Errorf("at most %d images per message", tools.MaxImagesPerMessage)
	}
	img, err := a.Cfg.Exec.LoadImage(path)
	if err != nil {
		return llm.Image{}, err
	}
	a.PendingImages = append(a.PendingImages, img)
	return img, nil
}

// hydrateImages re-reads the bytes of every image in msgs whose
// session file stored only the path (Data is never written to the
// file). Images whose file is gone stay in history as text only:
// the marker line says an image was there, and the payload builders
// skip images with no bytes.
func (a *App) hydrateImages(msgs []llm.Message) {
	for i := range msgs {
		for j := range msgs[i].Images {
			img := &msgs[i].Images[j]
			if img.Data != "" || img.Path == "" {
				continue
			}
			if loaded, err := a.Cfg.Exec.LoadImage(img.Path); err == nil {
				*img = loaded
			}
		}
	}
}

// SplitModelArg parses a /model argument: "provider/model" or a bare
// model id (the provider is resolved by FindModel, current first).
func SplitModelArg(arg string) (provider, model string) {
	if p, m, ok := strings.Cut(arg, "/"); ok {
		return p, m
	}
	return "", arg
}

// FindModel locates a bare model id across providers: the current
// provider first, then the rest in config order. A provider claims
// an id only when it lists it (flat model or models entry); a
// provider with no models claims nothing.
func (a *App) FindModel(modelID string) (string, bool) {
	if a.Config == nil || modelID == "" {
		return "", false
	}
	names := []string{}
	if a.Provider != "" {
		names = append(names, a.Provider)
	}
	for _, n := range a.Config.ProviderNames() {
		if n != a.Provider {
			names = append(names, n)
		}
	}
	for _, n := range names {
		prov, ok := a.Config.Select(n)
		if !ok {
			continue
		}
		if prov.Model == modelID {
			return n, true
		}
		for _, m := range prov.Models {
			if m.ID == modelID {
				return n, true
			}
		}
	}
	return "", false
}

// SwitchModel rebuilds the client for another provider/model
// mid-session, Pi-style (/model). History is kept: it is stored
// API-agnostic and replayed through the new model's API on the next
// turn, like a --resume across providers. Only the model, client,
// context limit and image-input flag change; session, skills and
// history are untouched. Precedence differs from Setup on purpose:
// there are no startup flags here, so the api comes from the model
// entry (falling back to the current api when the entry declares
// none), baseUrl from OPENAI_BASE_URL or the entry, maxTokens from
// ANTHROPIC_MAX_TOKENS or the entry, and the context limit from
// OPENAI_CONTEXT_LIMIT or the entry. It returns the resolved
// provider, model id and api shape.
func (a *App) SwitchModel(provider, model string) (provName, modelID, api string, err error) {
	if a.Config == nil || a.Cfg == nil {
		return "", "", "", fmt.Errorf("no config loaded")
	}
	cfg := a.Config
	if provider == "" {
		provider = a.Provider
	}
	prov, ok := cfg.Select(provider)
	if !ok {
		return "", "", "", fmt.Errorf("unknown provider %q (configured: %s)", provider, strings.Join(cfg.ProviderNames(), ", "))
	}
	provName = cfg.SelectedName(provider)
	if provName == "" {
		provName = provider
	}
	if model == "" && provName == cfg.DefaultProvider {
		model = cfg.DefaultModel
	}
	mdl, ok := prov.ResolveModel(model)
	if !ok && model != "" {
		// A bare model id with no provider named: look across
		// providers (current first) before giving up, so /model
		// <model> and RPC set_model behave the same.
		if p, found := a.FindModel(model); found && p != provName {
			prov, _ = cfg.Select(p)
			provName = cfg.SelectedName(p)
			if provName == "" {
				provName = p
			}
			mdl, ok = prov.ResolveModel(model)
		}
	}
	if !ok {
		return "", "", "", fmt.Errorf("unknown model %q for provider %q (models: %s)", model, provName, modelIDs(prov))
	}
	if mdl.ID == "" {
		return "", "", "", fmt.Errorf("provider %q: no model configured (set \"model\" or add a \"models\" entry)", provName)
	}
	// API key: the provider's apiKey "$NAME" reference wins, else
	// OPENAI_API_KEY. A literal key never lives in the file.
	apiKey := ""
	if prov.APIKey != "" {
		k, err := config.ResolveAPIKey(prov.APIKey)
		if err != nil {
			return "", "", "", err
		}
		apiKey = k
	} else {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return "", "", "", fmt.Errorf("missing API key: set OPENAI_API_KEY, or set apiKey: \"$NAME\" for provider %q in the config file", provName)
	}
	apiMode := a.Cfg.Client.API
	if mdl.API != "" {
		m, err := config.NormalizeAPI(mdl.API)
		if err != nil {
			return "", "", "", fmt.Errorf("invalid api for model %q: %w", mdl.ID, err)
		}
		apiMode = m
	}
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = mdl.BaseURL
	}
	if baseURL == "" {
		return "", "", "", fmt.Errorf("provider %q: baseUrl is required", provName)
	}
	maxTokens := 0
	if v := os.Getenv("ANTHROPIC_MAX_TOKENS"); v != "" {
		fmt.Sscanf(v, "%d", &maxTokens)
	} else if mdl.MaxTokens != nil {
		maxTokens = *mdl.MaxTokens
	} else if cfg.AnthropicMaxTokens != nil {
		maxTokens = *cfg.AnthropicMaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = config.DefaultMaxTokens
	}
	var headers map[string]string
	merged := map[string]string{}
	for k, v := range prov.Headers {
		merged[k] = v
	}
	for k, v := range mdl.Headers {
		merged[k] = v
	}
	if len(merged) > 0 {
		headers = make(map[string]string, len(merged))
		for k, v := range merged {
			rv, err := config.ResolveValue(v)
			if err != nil {
				return "", "", "", err
			}
			headers[k] = rv
		}
	}
	contextLimit := 0
	if v := os.Getenv("OPENAI_CONTEXT_LIMIT"); v != "" {
		fmt.Sscanf(v, "%d", &contextLimit)
	}
	if contextLimit <= 0 && mdl.ContextWindow != nil {
		contextLimit = *mdl.ContextWindow
	}
	if contextLimit <= 0 && cfg.ContextLimit != nil {
		contextLimit = *cfg.ContextLimit
	}
	if contextLimit <= 0 {
		contextLimit = config.DefaultContextWindow
	}
	a.Cfg.Client = &llm.Client{BaseURL: baseURL, APIKey: apiKey, Model: mdl.ID, API: apiMode, MaxTokens: maxTokens, SamplingParams: mdl.SamplingParams, Headers: headers}
	a.Cfg.Exec.ImageInput = slices.Contains(mdl.Input, "image")
	a.Model = mdl
	a.Provider = provName
	a.ContextLimit = contextLimit
	return provName, mdl.ID, apiMode, nil
}

// modelIDs lists a provider's model ids for error messages: the
// flat model first, then the models entries.
func modelIDs(p config.Provider) string {
	ids := []string{}
	if p.Model != "" {
		ids = append(ids, p.Model)
	}
	for _, m := range p.Models {
		ids = append(ids, m.ID)
	}
	return strings.Join(ids, ", ")
}

// NewSession closes the current session store and starts a fresh one,
// resetting history. The executor's audit closure is re-pointed at
// the new store; a background job finishing mid-swap can still audit
// to the old session's sidecar. The runner's totals are reset by
// ResetSession, not here.
func (a *App) NewSession() (*session.Store, error) {
	store, err := session.New("")
	if err != nil {
		return nil, err
	}
	old := a.Store
	a.Store = store
	a.History = nil
	if a.Cfg != nil {
		a.Cfg.Session = store
		if a.Cfg.Exec != nil {
			a.Cfg.Exec.Audit = func(e tools.AuditEntry) { _ = store.Audit(e) }
		}
	}
	if old != nil {
		_ = old.Close()
	}
	return store, nil
}

// LoadSession switches to an existing session: its history is loaded
// (like --resume) and the store is opened for appending. The id must
// name a session file; a missing one is an error, not a new session.
func (a *App) LoadSession(id string) ([]llm.Message, error) {
	if _, err := os.Stat(filepath.Join(session.Dir(), id+".jsonl")); os.IsNotExist(err) {
		return nil, fmt.Errorf("unknown session %q (see /sessions)", id)
	}
	history, err := session.Load(id)
	if err != nil {
		return nil, err
	}
	store, err := session.New(id)
	if err != nil {
		return nil, err
	}
	old := a.Store
	a.Store = store
	a.History = history
	a.ResumedMessage = fmt.Sprintf("Resumed session %s (%d messages).", id, len(history))
	if a.Cfg != nil {
		a.Cfg.Session = store
		if a.Cfg.Exec != nil {
			a.Cfg.Exec.Audit = func(e tools.AuditEntry) { _ = store.Audit(e) }
		}
	}
	if old != nil {
		_ = old.Close()
	}
	return history, nil
}

// Print is the line printer (plain: fmt.Printf; TUI: sent as a message
// so it stays ordered with the model's own scrollback prints).
// Session totals accumulate across turns. Cost is estimated only when
// rates are configured (App.Rates); the estimate is always labelled
// with its source, and the provider's real price stays unverified.
type Runner struct {
	App          *App
	SessionUsage llm.Usage
	SessionCost  float64 // USD estimate, accumulated per turn
	// Turns counts completed agent turns (slash commands excluded),
	// for the TUI meta line.
	Turns int
	// LastErr is the error of the most recent turn (nil on success).
	// The RPC server reads it to report a turn's outcome; the plain
	// CLI and TUI already print the error through Print.
	LastErr error
	Print   func(format string, args ...any)
}

// ResetSession starts a fresh session and zeroes the runner's
// totals. It returns the new session id.
func (r *Runner) ResetSession() (string, error) {
	store, err := r.App.NewSession()
	if err != nil {
		return "", err
	}
	r.SessionUsage = llm.Usage{}
	r.SessionCost = 0
	r.Turns = 0
	r.LastErr = nil
	return store.ID, nil
}

// ResumeSession switches to an existing session and zeroes the
// runner's totals. It returns how many messages were loaded.
func (r *Runner) ResumeSession(id string) (int, error) {
	history, err := r.App.LoadSession(id)
	if err != nil {
		return 0, err
	}
	r.SessionUsage = llm.Usage{}
	r.SessionCost = 0
	r.Turns = 0
	r.LastErr = nil
	return len(history), nil
}

// costLine renders the estimate suffix for a usage line, or the
// unverified label when no rates are configured. The label names the
// provider from the base URL: a second provider must not inherit
// OpenConsole's name.
func (r *Runner) costLine(cost float64) string {
	label := r.App.Cfg.Client.ProviderName() + " 實價未驗證"
	if !r.App.Rates.Configured {
		return "fee: " + label
	}
	return fmt.Sprintf("est. cost: ~$%.4f (rates: %s; %s)", cost, r.App.Rates.Source, label)
}

// Handle runs one input line. It returns true when the UI should quit.
func (r *Runner) Handle(ctx context.Context, line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if line == "/image" || strings.HasPrefix(line, "/image ") {
		arg := strings.TrimSpace(strings.TrimPrefix(line, "/image"))
		if arg == "" {
			if len(r.App.PendingImages) == 0 {
				r.Print("No images attached. Usage: /image <path> (png, jpg, gif, webp).")
				return false
			}
			for _, img := range r.App.PendingImages {
				r.Print("Attached: %s", img.Describe())
			}
			return false
		}
		img, err := r.App.AttachImage(arg)
		if err != nil {
			r.Print("[error] %v", err)
			return false
		}
		r.Print("Attached %s — will be sent with your next message.", img.Describe())
		return false
	}
	if line == "/sessions" {
		r.printSessions()
		return false
	}
	if line == "/resume" || strings.HasPrefix(line, "/resume ") {
		id := strings.TrimSpace(strings.TrimPrefix(line, "/resume"))
		if id == "" {
			r.Print("Usage: /resume <id> (see /sessions).")
			return false
		}
		n, err := r.ResumeSession(id)
		if err != nil {
			r.Print("[error] %v", err)
			return false
		}
		r.Print("Resumed session %s (%d messages).", id, n)
		return false
	}
	if line == "/rename" || strings.HasPrefix(line, "/rename ") {
		args := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "/rename")))
		if len(args) != 2 {
			r.Print("Usage: /rename <old> <new>.")
			return false
		}
		old, newID := args[0], args[1]
		var err error
		if old == r.App.Store.ID {
			err = r.App.Store.Rename(newID)
		} else {
			err = session.RenameSession(old, newID)
		}
		if err != nil {
			r.Print("[error] %v", err)
			return false
		}
		r.Print("Renamed session %s → %s.", old, newID)
		return false
	}
	if line == "/model" || strings.HasPrefix(line, "/model ") {
		arg := strings.TrimSpace(strings.TrimPrefix(line, "/model"))
		if arg == "" {
			r.printModels()
			return false
		}
		provider, model := SplitModelArg(arg)
		provName, modelID, api, err := r.App.SwitchModel(provider, model)
		if err != nil {
			r.Print("[error] %v", err)
			return false
		}
		r.Print("Switched to %s/%s (%s). Context window %d tokens; history replays through the new API.", provName, modelID, api, r.App.ContextLimit)
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

// printSessions lists the sessions, newest first, current marked *.
func (r *Runner) printSessions() {
	list, err := session.List()
	if err != nil {
		r.Print("[error] %v", err)
		return
	}
	if len(list) == 0 {
		r.Print("No sessions yet.")
		return
	}
	r.Print("Sessions (newest first; * = current):")
	for _, s := range list {
		mark := " "
		if s.ID == r.App.Store.ID {
			mark = "*"
		}
		line := fmt.Sprintf("%s %s  %d msgs  %s", mark, s.ID, s.Messages, s.Modified.Format("2006-01-02 15:04"))
		if s.Preview != "" {
			line += "  " + s.Preview
		}
		r.Print("%s", line)
	}
	r.Print("/resume <id> to switch, /rename <old> <new> to rename.")
}

// printModels lists every configured model, current one marked *.
func (r *Runner) printModels() {
	a := r.App
	if a.Config == nil {
		r.Print("No config loaded.")
		return
	}
	r.Print("Models (current marked *):")
	for _, name := range a.Config.ProviderNames() {
		prov, ok := a.Config.Select(name)
		if !ok {
			continue
		}
		ids := []string{}
		if prov.Model != "" {
			ids = append(ids, prov.Model)
		}
		for _, m := range prov.Models {
			ids = append(ids, m.ID)
		}
		for _, id := range ids {
			mdl, _ := prov.ResolveModel(id)
			mark := " "
			if name == a.Provider && id == a.Model.ID {
				mark = "*"
			}
			r.Print("%s %s/%s (%s)", mark, name, id, mdl.API)
		}
	}
	r.Print("Usage: /model <provider>/<model> or /model <model>")
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
	// A run that never reaches the loop (e.g. /skill: with an unknown
	// name) must not leave a stale error behind for the next reader.
	r.LastErr = nil
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
	// Images attached for this turn (-image, /image, RPC) ride on
	// the task message itself, then clear: they live in history
	// from here and are resent on later calls like any message.
	if len(r.App.PendingImages) > 0 {
		userMsg.Images = r.App.PendingImages
		r.App.PendingImages = nil
	}
	r.App.History = append(r.App.History, userMsg)
	_ = r.App.Store.Append(userMsg)
	r.Turns++
	_, usage, err := r.App.Cfg.Run(ctx, r.App.History)
	r.LastErr = err
	if err != nil {
		r.Print("\n[error] %v", err)
	}
	// Keep the returned history in sync: Run used its own copy.
	// Re-load the session file to recover tool messages. Images in
	// the file carry paths only; re-read their bytes here.
	if msgs, err := session.Load(r.App.Store.ID); err == nil {
		r.App.hydrateImages(msgs)
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
