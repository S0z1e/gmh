// Package app holds the setup and turn-running logic shared by the plain
// CLI (cmd/harness) and the TUI (cmd/harness-tui). It never imports any
// UI code: the caller supplies the event renderer and the line printer.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"minimalharness/internal/agent"
	"minimalharness/internal/approvalmode"
	"minimalharness/internal/config"
	"minimalharness/internal/guidance"
	"minimalharness/internal/llm"
	"minimalharness/internal/plugins"
	"minimalharness/internal/session"
	"minimalharness/internal/skills"
	"minimalharness/internal/tools"
	"minimalharness/internal/workspace"
)

type Params struct {
	// WorkspaceTrust records a direct user decision: trust, untrust, or empty
	// to use the saved decision. It never comes from a project config.
	WorkspaceTrust string
	Root           string
	Mode           string // approval mode, separate from the active tool set
	Model          string
	API            string // chat, responses or anthropic
	MaxSteps       int
	ContextLimit   int
	Resume         string
	RebindSession  bool // direct startup consent, only valid with Resume
	// Steal takes over a busy writer before opening Resume. It is a direct
	// user choice (CLI --steal or an answered TUI/REPL prompt) and is refused
	// when the owner's process identity cannot be proven.
	Steal     bool
	SessionID string
	// Provider selects a provider entry from the config file
	// (defaultProvider when empty).
	Provider string
	// ConfigPath names one config file explicitly. Empty means
	// auto-discover: ~/.minimal-harness/config.json, then
	// <root>/.harness.json on top of it.
	ConfigPath string
	// DenyInsteadOfPrompt refuses requests that need approval.
	DenyInsteadOfPrompt bool
	// Headless disables interactive approval prompts in -p mode.
	Headless bool
	// Sandbox runs bash calls under bwrap (opt-in): the workspace
	// root is the only writable directory. The flag, HARNESS_SANDBOX
	// or the config sandbox field turn it on; Setup fails fast when
	// bwrap is missing or cannot create a sandbox here.
	Sandbox bool
	// BashTimeoutSeconds overrides the default 120s bash timeout
	// (0 = default, or OPENAI_BASH_TIMEOUT_SECONDS).
	BashTimeoutSeconds int
	// BackgroundThresholdSeconds is how long a bash call runs before
	// the loop backgrounds it: -1 = unset (env OPENAI_BG_THRESHOLD_SECONDS
	// or the 120s default), 0 = never background, >0 = seconds.
	BackgroundThresholdSeconds int
	// BackgroundMaxLifetimeSeconds caps background:true jobs when no per-call
	// timeout is given: 0 = unset (env OPENAI_BACKGROUND_MAX_LIFETIME_SECONDS,
	// config backgroundMaxLifetimeSeconds, or 7200s default), >0 = seconds,
	// <0 = unlimited.
	BackgroundMaxLifetimeSeconds int
	// DisableBackground turns backgrounding off entirely. -p print
	// mode sets it: a one-shot run stays synchronous.
	DisableBackground bool
	// Images are image files attached to the first turn (-image flag,
	// repeatable). They are loaded in Setup and fail the run when a
	// file is missing, too big, or the model has no image input.
	Images []string
	// Files are UTF-8 text files attached to the first turn (-attach flag).
	Files []string
}

// BackgroundMaxLifetimeSecondsFromFlag converts the CLI -bg-max-lifetime flag
// (-1 = unset, 0 = unlimited, >0 = seconds) into Params.BackgroundMaxLifetimeSeconds.
func BackgroundMaxLifetimeSecondsFromFlag(flagVal int) int {
	if flagVal == 0 {
		return -1
	}
	if flagVal > 0 {
		return flagVal
	}
	return 0
}

type App struct {
	WorkspaceTrusted bool
	Cfg              *agent.Config
	Store            *session.Store
	History          []llm.Message
	Skills           []skills.Skill
	ContextLimit     int
	Root             string
	// Model is the resolved model entry: id, api, baseUrl,
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
	// RecoveredJobs summarizes any retained background jobs inspected while
	// reopening the current session.
	RecoveredJobs []tools.RecoveredJob
	// OrphanedJobs summarizes running background jobs in other unsupervised
	// sessions of the same workspace, and OrphanedJobsNotice formats them.
	OrphanedJobs       []tools.OrphanedSessionJobs
	OrphanedJobsNotice string
	// TUITheme is the resolved TUI theme name: HARNESS_TUI_THEME,
	// else the config file, else "" (the default theme).
	TUITheme string
	// PendingImages are images attached for the next turn (/image,
	// -image, RPC attach_image). runOnce moves them onto the task
	// message and clears the list; the images then live in history
	// and are resent on later calls, like any message.
	PendingImages []llm.Image
	PendingFiles  []llm.TextAttachment
	// Plugins is the loaded plugin registry (nil = none loaded).
	// Kept so later commands can list plugins; the executor holds
	// the same registry for dispatch.
	Plugins *plugins.Registry
	// toolMode is the loop's active tool set (agent.ToolModeDefault
	// or ToolModePlan — NOT Cfg.Mode, the approval mode). Set only
	// between turns, via SetToolMode; the registry's ModeSetter
	// points at it, so a plugin command can switch modes.
	toolMode agent.ToolMode
	// allTools is the full tool list (built-ins + plugin tools) that
	// SetToolMode filters per mode. Cfg.Tools is the filtered view.
	allTools []llm.ToolDef
	// pendingReminder is a reminder for the next turn snapshot set by a mode
	// switch and persisted by the next runOnce with that snapshot.
	pendingReminder string
	// Writers with undelivered jobs remain owned across session switches.
	retainedStores map[*session.Store]struct{}
	orphansMu      sync.Mutex
}

// Setup builds the client, session and agent config. The caller sets
// Cfg.OnEvent (the event renderer) before running anything.
func Setup(p Params) (*App, error) {
	if p.RebindSession && p.Resume == "" {
		return nil, fmt.Errorf("--rebind-session requires --resume")
	}
	if p.Steal && p.Resume == "" {
		return nil, fmt.Errorf("--steal requires --resume")
	}
	identity, err := workspace.Identify(p.Root)
	if err != nil {
		return nil, err
	}
	absRoot := identity.Path
	if p.WorkspaceTrust != "" {
		if err := workspace.Decide(identity, p.WorkspaceTrust); err != nil {
			return nil, err
		}
	}
	trusted, err := workspace.Trusted(identity)
	if err != nil {
		return nil, err
	}

	// Config: flag > env > project file > global file > built-in
	// default. A bad config file fails fast, before any session
	// is created.
	var cfg *config.File
	if p.ConfigPath != "" {
		var err error
		cfg, err = config.LoadExplicit(p.ConfigPath)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		cfg, err = config.LoadForTrust(absRoot, trusted)
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
		return nil, fmt.Errorf("no providers configured: add a provider to %s; project config requires a trusted workspace (--workspace-trust trust)", global)
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
	// api at provider or model level; there is no silent default.
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
	apiKey, headers, err := prov.ResolveCredentials(mdl, baseURL, os.Getenv("OPENAI_BASE_URL") != "")
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", provName, err)
	}
	mode := p.Mode
	if mode == "" {
		mode = cfg.Mode
	}
	if mode == "" {
		mode = approvalmode.Suggest
	}
	if err := approvalmode.Validate(mode); err != nil {
		return nil, err
	}

	id := p.SessionID
	if p.Resume != "" {
		id = p.Resume
	}
	var store *session.Store
	if p.Steal {
		store, err = session.StealSession(id, absRoot, p.RebindSession)
	} else {
		store, err = session.Open(id, session.OpenOptions{Workspace: absRoot, Rebind: p.RebindSession, Existing: p.Resume != ""})
	}
	if err != nil {
		if p.Resume != "" {
			return nil, fmt.Errorf("cannot resume session %q: %w", p.Resume, err)
		}
		return nil, err
	}

	setupComplete := false
	defer func() {
		if !setupComplete {
			_ = store.Close()
		}
	}()
	a := &App{Store: store, Root: absRoot, WorkspaceTrusted: trusted}
	if p.Resume != "" {
		history, err := session.Load(store.ID)
		if err != nil {
			store.Close()
			return nil, err
		}
		a.History = history
		a.ResumedMessage = fmt.Sprintf("Resumed session %s (%d messages).", p.Resume, len(history))
		if p.Steal {
			a.ResumedMessage = fmt.Sprintf("Stole and resumed session %s (%d messages).", p.Resume, len(history))
		}
	}

	skillList := skills.DiscoverForTrust(absRoot, trusted)
	a.Skills = skillList
	system := agent.SystemPrompt
	system += skills.Listing(skillList)

	// Plugins: a bad plugin is skipped with a warning on stderr; it
	// never fails Setup. Disabled with HARNESS_PLUGINS=0, or one by
	// one in the config file (plugins.disabled).
	disabled := []string{}
	if cfg.Plugins != nil {
		disabled = cfg.Plugins.Disabled
	}
	pluginReg := plugins.Untrusted(absRoot)
	var pluginErrs []error
	if trusted {
		pluginReg, pluginErrs = plugins.Load(absRoot, disabled)
	}
	for _, err := range pluginErrs {
		fmt.Fprintf(os.Stderr, "warning: plugin: %v\n", err)
	}
	a.Plugins = pluginReg
	pluginTools := pluginReg.ToolDefs()
	// The mode callbacks: a plugin command (e.g. /plan) switches
	// the tool mode through the registry, and getMode reads it back.
	// Wired here, after Load; they only run at runtime, once Setup
	// has built the agent config below.
	pluginReg.ModeSetter = func(name string) error { return a.SetToolMode(agent.ToolMode(name)) }
	pluginReg.ModeGetter = func() string { return string(a.ToolMode()) }
	pluginReg.SessionGetter = a.sessionID

	executor := &tools.Executor{Root: absRoot, UntrustedWorkspace: !trusted, ImageInput: slices.Contains(mdl.Input, "image"), Plugins: pluginReg}
	if trusted {
		executor.Guidance = guidance.ForWorkspace(identity)
	}
	executor.BashEnv = cfg.BashEnv
	// Bash output files live in this session's directory (never in
	// the project tree): the loop's read tool can pull them, and a
	// new session / resume re-points the executor (see newSession
	// and LoadSession, which reset it alongside the audit hook).
	executor.OutputDir = store.OutputDir()
	executor.OutputID = store.OutputID()
	executor.JobsDir = store.JobsDir()
	executor.SwitchSession(store.ID)
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

	// Background threshold: flag wins, else env, else the 120s default.
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

	// Background max lifetime: flag wins, else env, else config, else 2h default.
	// 0 in env/config (or <0 in Params) disables the cap.
	var bgMaxLifetime time.Duration
	if p.BackgroundMaxLifetimeSeconds < 0 {
		bgMaxLifetime = -1
	} else if p.BackgroundMaxLifetimeSeconds > 0 {
		bgMaxLifetime = time.Duration(p.BackgroundMaxLifetimeSeconds) * time.Second
	} else if v := os.Getenv("OPENAI_BACKGROUND_MAX_LIFETIME_SECONDS"); v != "" {
		var secs int
		if _, err := fmt.Sscanf(v, "%d", &secs); err == nil {
			if secs == 0 {
				bgMaxLifetime = -1
			} else if secs > 0 {
				bgMaxLifetime = time.Duration(secs) * time.Second
			}
		}
	} else if cfg.BackgroundMaxLifetimeSeconds != nil {
		if *cfg.BackgroundMaxLifetimeSeconds == 0 {
			bgMaxLifetime = -1
		} else {
			bgMaxLifetime = time.Duration(*cfg.BackgroundMaxLifetimeSeconds) * time.Second
		}
	}
	executor.BackgroundMaxLifetime = bgMaxLifetime

	// Sandbox: flag wins, else env, else config. bwrap is located and
	// probed now so a broken sandbox fails at startup, not on the
	// first bash call.
	sandbox := p.Sandbox
	if v := os.Getenv("HARNESS_SANDBOX"); v == "1" || strings.EqualFold(v, "true") {
		sandbox = true
	}
	if cfg.Sandbox != nil && *cfg.Sandbox {
		sandbox = true
	}
	if sandbox {
		bwrap, err := exec.LookPath("bwrap")
		if err != nil {
			return nil, fmt.Errorf("--sandbox needs bwrap installed (bubblewrap), not found on PATH: %w", err)
		}
		if err := tools.ProbeSandbox(bwrap); err != nil {
			return nil, err
		}
		executor.Sandbox = true
		executor.BwrapPath = bwrap
	}

	deny := p.DenyInsteadOfPrompt
	if v := os.Getenv("OPENAI_DENY_INSTEAD_OF_PROMPT"); v == "1" || strings.EqualFold(v, "true") {
		deny = true
	}
	if cfg.DenyInsteadOfPrompt != nil && *cfg.DenyInsteadOfPrompt {
		deny = true
	}

	// MaxTokens: env > model > config > default 16384. All three
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

	a.Model = mdl
	a.Config = cfg
	a.Provider = provName
	// The tool mode starts at default, with the full tool list.
	a.toolMode = agent.ToolModeDefault
	a.allTools = append(agent.ToolDefs(), pluginTools...)
	if !trusted {
		a.allTools = slices.DeleteFunc(a.allTools, func(d llm.ToolDef) bool {
			return d.Function.Name == tools.GuidanceTool || d.Function.Name == tools.GitStatusTool
		})
	}
	timeout, idle := clientTimeouts(mdl)
	a.Cfg = &agent.Config{
		Client:              &llm.Client{BaseURL: baseURL, APIKey: apiKey, Model: mdl.ID, API: apiMode, MaxTokens: maxTokens, SamplingParams: mdl.SamplingParams, Headers: headers, Effort: mdl.Effort, Thinking: mdl.Thinking, PromptCacheKey: a.sessionID(), Timeout: timeout, IdleTimeout: idle},
		Exec:                executor,
		Tools:               a.allTools,
		System:              system,
		TraceRequests:       true,
		Mode:                mode,
		MaxSteps:            p.MaxSteps,
		Session:             store,
		Verification:        cfg.Verification,
		TurnBudget:          cfg.TurnBudget,
		DenyInsteadOfPrompt: deny,
		Headless:            p.Headless,
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
	a.Cfg.ContextLimit = contextLimit

	a.TUITheme = os.Getenv("HARNESS_TUI_THEME")
	if a.TUITheme == "" {
		a.TUITheme = cfg.TUITheme
	}

	envRates, err := llm.RatesFromEnv()
	if err != nil {
		store.Close()
		return nil, err
	}
	rates := ratesForModel(envRates, provName, mdl)
	a.Rates = rates
	a.Cfg.UsageRates = rates

	// Images stored in a resumed session carry only paths; re-read
	// their bytes now, while the executor exists.
	a.hydrateImages(a.History)
	if p.Resume != "" {
		if err := a.recoverSessionJobs(store, a.History); err != nil {
			return nil, err
		}
	}
	a.RefreshOrphanedJobs()
	// -image attachments fail the run when a file cannot be loaded.
	for _, p := range p.Images {
		if _, err := a.AttachImage(p); err != nil {
			store.Close()
			return nil, fmt.Errorf("-image %s: %w", p, err)
		}
	}
	for _, path := range p.Files {
		if _, err := a.AttachFile(path); err != nil {
			store.Close()
			return nil, fmt.Errorf("-attach %q: %w", path, err)
		}
	}
	setupComplete = true
	return a, nil
}

func (a *App) recoverSessionJobs(store *session.Store, history []llm.Message) error {
	if a.Cfg == nil || a.Cfg.Exec == nil || store == nil {
		return nil
	}
	recovered, err := agent.RecoverExecutorJobs(a.Cfg.Exec, store.ID, history)
	if err != nil {
		return err
	}
	a.RecoveredJobs = recovered
	if summary := tools.FormatRecoveredJobsSummary(recovered); summary != "" {
		a.ResumedMessage += "\n" + summary
	}
	return nil
}

// RefreshOrphanedJobs scans other unsupervised sessions in the same workspace
// for live background jobs and updates OrphanedJobs and OrphanedJobsNotice.
func (a *App) RefreshOrphanedJobs() ([]tools.OrphanedSessionJobs, string) {
	if a == nil || a.Root == "" {
		return nil, ""
	}
	a.orphansMu.Lock()
	owned := map[string]bool{}
	if a.Store != nil {
		id, outID := a.Store.SnapshotIDs()
		owned[id] = true
		owned[outID] = true
	}
	for retained := range a.retainedStores {
		if retained != nil {
			id, outID := retained.SnapshotIDs()
			owned[id] = true
			owned[outID] = true
		}
	}
	a.orphansMu.Unlock()

	sessions, err := session.ListWorkspaceSessions(a.Root)
	var orphans []tools.OrphanedSessionJobs
	if err == nil {
		for _, ws := range sessions {
			if ws.Busy || owned[ws.ID] || owned[ws.OutputID] {
				continue
			}
			jobs, jerr := tools.FindOrphanedRunningJobs(ws.JobsDir, ws.OutputID, ws.OutputDir)
			if jerr != nil || len(jobs) == 0 {
				continue
			}
			orphans = append(orphans, tools.OrphanedSessionJobs{
				SessionID: ws.ID,
				Jobs:      jobs,
			})
		}
	}
	notice := tools.FormatOrphanedJobsNotice(orphans)

	a.orphansMu.Lock()
	a.OrphanedJobs = orphans
	a.OrphanedJobsNotice = notice
	a.orphansMu.Unlock()
	return orphans, notice
}

// OrphanedNotice returns the latest formatted notice of orphaned workspace jobs.
func (a *App) OrphanedNotice() string {
	if a == nil {
		return ""
	}
	a.orphansMu.Lock()
	defer a.orphansMu.Unlock()
	return a.OrphanedJobsNotice
}

// CloseContext stops commands and saves finished results...

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

// hydrateImages uses the same immutable-blob loader for every history entry point.
func (a *App) hydrateImages(msgs []llm.Message) { session.HydrateImages(msgs) }

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
// mid-session (/model). History is kept: it is stored
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
// clientTimeouts resolves a model's timeout settings into the
// llm.Client durations: nil means "use the client's defaults"
// (total 600s, idle 60s).
func clientTimeouts(mdl config.Model) (timeout, idle time.Duration) {
	if mdl.TimeoutSeconds != nil {
		timeout = time.Duration(*mdl.TimeoutSeconds) * time.Second
	}
	if mdl.IdleTimeoutSeconds != nil {
		idle = time.Duration(*mdl.IdleTimeoutSeconds) * time.Second
	}
	return timeout, idle
}

// ratesForModel merges explicit environment rates over the model's configured
// cost schedule field-by-field so partial env overrides (e.g. missing
// OPENAI_PRICE_CACHE_WRITE_PER_MTOK) still inherit model config fields.
func ratesForModel(envRates llm.Rates, provName string, mdl config.Model) llm.Rates {
	var base llm.Rates
	hasModelCost := mdl.Cost != nil && mdl.Cost.Input != nil && mdl.Cost.Output != nil && mdl.Cost.CacheRead != nil && mdl.Cost.CacheWrite != nil
	if hasModelCost {
		base = llm.Rates{
			InputPerMTok:         *mdl.Cost.Input,
			OutputPerMTok:        *mdl.Cost.Output,
			CachedPerMTok:        *mdl.Cost.CacheRead,
			CacheWritePerMTok:    *mdl.Cost.CacheWrite,
			InputConfigured:      true,
			OutputConfigured:     true,
			CachedConfigured:     true,
			CacheWriteConfigured: true,
			Source:               fmt.Sprintf("config %s/%s", provName, mdl.ID),
			Configured:           true,
			Complete:             true,
		}
		for _, t := range mdl.Cost.Tiers {
			if t.InputTokensAbove == nil || t.Input == nil || t.Output == nil || t.CacheRead == nil || t.CacheWrite == nil {
				continue
			}
			base.Tiers = append(base.Tiers, llm.RateTier{
				InputTokensAbove:  int64(*t.InputTokensAbove),
				InputPerMTok:      *t.Input,
				OutputPerMTok:     *t.Output,
				CachedPerMTok:     *t.CacheRead,
				CacheWritePerMTok: *t.CacheWrite,
			})
		}
	}
	if !envRates.Configured {
		if hasModelCost {
			return base
		}
		return envRates
	}
	if !hasModelCost {
		return envRates
	}
	if envRates.InputConfigured {
		base.InputPerMTok = envRates.InputPerMTok
	}
	if envRates.OutputConfigured {
		base.OutputPerMTok = envRates.OutputPerMTok
	}
	if envRates.CachedConfigured {
		base.CachedPerMTok = envRates.CachedPerMTok
	}
	if envRates.CacheWriteConfigured {
		base.CacheWritePerMTok = envRates.CacheWritePerMTok
		base.CacheWriteConfigured = true
	}
	if envRates.PeakInputPerMTok > 0 {
		base.PeakInputPerMTok = envRates.PeakInputPerMTok
	}
	if envRates.PeakOutputPerMTok > 0 {
		base.PeakOutputPerMTok = envRates.PeakOutputPerMTok
	}
	if envRates.PeakCachedPerMTok > 0 {
		base.PeakCachedPerMTok = envRates.PeakCachedPerMTok
	}
	// Apply explicit env overrides to configured tiers too so env rates are
	// never silently overridden above a tier threshold.
	for i := range base.Tiers {
		if envRates.InputConfigured {
			base.Tiers[i].InputPerMTok = envRates.InputPerMTok
		}
		if envRates.OutputConfigured {
			base.Tiers[i].OutputPerMTok = envRates.OutputPerMTok
		}
		if envRates.CachedConfigured {
			base.Tiers[i].CachedPerMTok = envRates.CachedPerMTok
		}
		if envRates.CacheWriteConfigured {
			base.Tiers[i].CacheWritePerMTok = envRates.CacheWritePerMTok
		}
	}
	if envRates.Source != "" {
		base.Source = envRates.Source
	}
	base.Complete = base.InputConfigured && base.OutputConfigured && base.CachedConfigured
	base.Configured = true
	return base
}

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
	apiKey, headers, err := prov.ResolveCredentials(mdl, baseURL, os.Getenv("OPENAI_BASE_URL") != "")
	if err != nil {
		return "", "", "", fmt.Errorf("provider %q: %w", provName, err)
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
	timeout, idle := clientTimeouts(mdl)
	a.Cfg.Client = &llm.Client{BaseURL: baseURL, APIKey: apiKey, Model: mdl.ID, API: apiMode, MaxTokens: maxTokens, SamplingParams: mdl.SamplingParams, Headers: headers, Effort: mdl.Effort, Thinking: mdl.Thinking, PromptCacheKey: a.sessionID(), Timeout: timeout, IdleTimeout: idle}
	a.Cfg.Exec.ImageInput = slices.Contains(mdl.Input, "image")
	a.Model = mdl
	a.Provider = provName
	a.ContextLimit = contextLimit
	a.Cfg.ContextLimit = contextLimit
	if envRates, rateErr := llm.RatesFromEnv(); rateErr == nil {
		rates := ratesForModel(envRates, provName, mdl)
		a.Rates = rates
		a.Cfg.UsageRates = rates
	}
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

// SetApprovalMode changes the approval policy safely even while a turn is running.
func (a *App) SetApprovalMode(mode string) error {
	if err := approvalmode.Validate(mode); err != nil {
		return err
	}
	a.Cfg.SetApprovalMode(mode)
	return nil
}

// ToolMode returns the loop's active tool set (agent.ToolModeDefault
// or ToolModePlan). Cfg.Mode independently selects the approval policy.
func (a *App) ToolMode() agent.ToolMode {
	if a.toolMode == "" {
		return agent.ToolModeDefault
	}
	return a.toolMode
}

// SetToolMode switches the loop's tool set between turns: the next
// turn's tools are rebuilt from the full list, and a reminder for its
// persisted context snapshot is queued so the model is told the tools changed.
// Switching to the current mode is a no-op (no reminder). An unknown
// mode is an error. Only ever called between turns — the loop reads
// Cfg.Tools fresh every step, so no locking is needed.
func (a *App) SetToolMode(m agent.ToolMode) error {
	if !m.Valid() {
		return fmt.Errorf("unknown tool mode %q (want default or plan)", string(m))
	}
	if m == a.ToolMode() {
		return nil
	}
	a.toolMode = m
	if a.Cfg != nil {
		a.Cfg.Tools = agent.ToolsForMode(m, a.allTools)
	}
	a.pendingReminder = agent.ReminderFor(m)
	return nil
}

// resetToolMode returns to the default tool set (new or resumed
// session): the mode is session-only and never persists.
func (a *App) resetToolMode() {
	a.toolMode = agent.ToolModeDefault
	a.pendingReminder = ""
	if a.Cfg != nil {
		a.Cfg.Tools = agent.ToolsForMode(agent.ToolModeDefault, a.allTools)
	}
}

// sessionID is the current session's id ("" when there is no store,
// e.g. a test App built without Setup).
func (a *App) sessionID() string {
	if a.Store == nil {
		return ""
	}
	return a.Store.ID
}

// fireSessionEvents runs the session_start / session_switch hooks
// after a session becomes current. oldID is "" when there was no
// previous session. Hooks are notifications: a failing hook is
// warned about and never fails the switch. (The initial session at
// Setup fires nothing — plugins are not loaded yet when it is
// created, and Setup builds it directly, not through NewSession.)
func (a *App) fireSessionEvents(oldID, newID string) {
	if a.Plugins == nil {
		return
	}
	ctx := context.Background()
	a.Plugins.RunSessionStartHooks(ctx, newID)
	if oldID != "" && oldID != newID {
		a.Plugins.RunSessionSwitchHooks(ctx, newID, oldID)
	}
}

// NewSession starts a fresh transcript while retaining stores with outstanding jobs.
func (a *App) NewSession() (*session.Store, error) {
	return a.newSession(true)
}

// newSession creates a fresh session and makes it current. fire
// controls whether the session_start / session_switch hooks run:
// ResetSession (/clear) creates a fresh session too, but the design
// says it does NOT fire the session events, so it passes false.
func (a *App) newSession(fire bool) (*session.Store, error) {
	store, err := session.Open("", session.OpenOptions{Workspace: a.Root})
	if err != nil {
		return nil, err
	}
	a.orphansMu.Lock()
	old := a.Store
	oldID := ""
	if old != nil {
		oldID = old.ID
	}
	a.Store = store
	a.orphansMu.Unlock()
	a.History = nil
	a.PendingImages = nil
	a.PendingFiles = nil
	a.ResumedMessage = ""
	a.RecoveredJobs = nil
	a.resetToolMode()
	if a.Cfg != nil {
		a.Cfg.Session = store
		a.Cfg.ResetSessionState()
		if a.Cfg.Client != nil {
			// The cache key follows the current session: a new
			// session must not reuse the previous session's key.
			a.Cfg.Client.PromptCacheKey = store.ID
		}
		if a.Cfg.Exec != nil {
			a.Cfg.Exec.Audit = func(e tools.AuditEntry) { _ = store.Audit(e) }
			// Bash output files follow the current session too.
			a.Cfg.Exec.OutputDir = store.OutputDir()
			a.Cfg.Exec.OutputID = store.OutputID()
			a.Cfg.Exec.JobsDir = store.JobsDir()
			a.Cfg.Exec.SwitchSession(store.ID)
		}
	}
	a.retainOrClose(old)
	a.RefreshOrphanedJobs()
	if fire {
		a.fireSessionEvents(oldID, store.ID)
	}
	return store, nil
}

// LoadSession switches to an existing session: its history is loaded
// (like --resume) and the store is opened for appending. The id must
// name a session file; a missing one is an error, not a new session.
// A session another process is writing returns ErrSessionBusy. Pass steal
// only after the user confirms: the lock is taken and the session reopened
// as one handoff, or the steal is refused when ownership cannot be proven.
func (a *App) LoadSession(id string) ([]llm.Message, error) {
	return a.loadSession(id, false, false)
}

// StealSession is LoadSession after an explicit steal confirmation.
func (a *App) StealSession(id string, rebind bool) ([]llm.Message, error) {
	return a.loadSession(id, true, rebind)
}

func (a *App) loadSession(id string, steal, rebind bool) ([]llm.Message, error) {
	if err := session.ValidateSessionID(id); err != nil {
		return nil, err
	}
	a.orphansMu.Lock()
	old := a.Store
	store := old
	reuse := old != nil && old.Owns(id)
	if !reuse {
		for retained := range a.retainedStores {
			if retained.Owns(id) {
				store, reuse = retained, true
				break
			}
		}
	}
	a.orphansMu.Unlock()
	if reuse {
		if err := store.CheckWorkspace(a.Root); err != nil {
			return nil, err
		}
	}
	if !reuse {
		var err error
		if steal {
			store, err = session.StealSession(id, a.Root, rebind)
		} else {
			store, err = session.Open(id, session.OpenOptions{Workspace: a.Root, Existing: true})
		}
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("unknown session %q (see /sessions): %w", id, err)
		}
		if err != nil {
			return nil, err
		}
	} else if steal {
		return nil, fmt.Errorf("session %q: %w (already owned by current process)", id, session.ErrSessionBusy)
	}
	history, err := session.Load(store.ID)
	if err != nil {
		if !reuse {
			store.Close()
		}
		return nil, err
	}
	oldID := ""
	if old != nil {
		oldID = old.ID
	}
	var recovered []tools.RecoveredJob
	resumedMsg := fmt.Sprintf("Resumed session %s (%d messages).", id, len(history))
	if steal {
		resumedMsg = fmt.Sprintf("Stole and resumed session %s (%d messages).", id, len(history))
	}
	if !reuse && a.Cfg != nil && a.Cfg.Exec != nil {
		exec := a.Cfg.Exec
		prevDir, prevOutID, prevJobsDir := exec.OutputDir, exec.OutputID, exec.JobsDir
		prevAudit := exec.Audit
		exec.Audit = func(e tools.AuditEntry) { _ = store.Audit(e) }
		exec.OutputDir = store.OutputDir()
		exec.OutputID = store.OutputID()
		exec.JobsDir = store.JobsDir()
		exec.ReacquireOutput(store.OutputDir())
		exec.SwitchSession(store.ID)
		var recErr error
		recovered, recErr = agent.RecoverExecutorJobs(exec, store.ID, history)
		if recErr != nil {
			exec.Audit = prevAudit
			exec.OutputDir = prevDir
			exec.OutputID = prevOutID
			exec.JobsDir = prevJobsDir
			if prevDir != "" {
				exec.ReacquireOutput(prevDir)
			}
			exec.SwitchSession(oldID)
			_ = store.Close()
			return nil, recErr
		}
		if summary := tools.FormatRecoveredJobsSummary(recovered); summary != "" {
			resumedMsg += "\n" + summary
		}
	}
	a.orphansMu.Lock()
	a.Store = store
	delete(a.retainedStores, store)
	a.orphansMu.Unlock()
	a.hydrateImages(history)
	a.History = history
	a.PendingImages = nil
	a.PendingFiles = nil
	a.ResumedMessage = resumedMsg
	a.RecoveredJobs = recovered
	a.resetToolMode()
	if a.Cfg != nil {
		a.Cfg.Session = store
		a.Cfg.ResetSessionState()
		if a.Cfg.Client != nil {
			// The cache key follows the current session.
			a.Cfg.Client.PromptCacheKey = store.ID
		}
		if a.Cfg.Exec != nil && reuse {
			a.Cfg.Exec.Audit = func(e tools.AuditEntry) { _ = store.Audit(e) }
			a.Cfg.Exec.OutputDir = store.OutputDir()
			a.Cfg.Exec.OutputID = store.OutputID()
			a.Cfg.Exec.JobsDir = store.JobsDir()
			a.Cfg.Exec.SwitchSession(store.ID)
		}
	}
	if old != store {
		a.retainOrClose(old)
	}
	a.RefreshOrphanedJobs()
	a.fireSessionEvents(oldID, store.ID)
	return history, nil
}

// RenameSession keeps the executor and plugin state under the transcript's id.
func (a *App) RenameSession(oldID, newID string) error {
	var err error
	a.orphansMu.Lock()
	current := a.Store != nil && a.Store.Owns(oldID)
	owner := a.Store
	if !current {
		owner = nil
		for retained := range a.retainedStores {
			if retained.Owns(oldID) {
				owner = retained
				break
			}
		}
	}
	a.orphansMu.Unlock()
	if owner != nil {
		// A filesystem alias can select this writer, but registries and
		// plugin ownership use the actual ID captured at launch.
		oldID = owner.ID
		err = owner.Rename(newID)
	} else {
		err = session.RenameSession(oldID, newID)
	}
	if err != nil {
		return err
	}
	if a.Cfg != nil && a.Cfg.Exec != nil {
		a.Cfg.Exec.RenameSession(oldID, newID)
		if current {
			a.Cfg.Exec.OutputDir = a.Store.OutputDir()
		}
	}
	if current && a.Cfg != nil && a.Cfg.Client != nil {
		a.Cfg.Client.PromptCacheKey = newID
	}
	if a.Plugins != nil {
		a.Plugins.RunSessionRenameHooks(context.Background(), newID, oldID)
	}
	return nil
}

// Print is the line printer (plain: fmt.Printf; TUI: sent as a message
// so it stays ordered with the model's own scrollback prints).
// Session totals accumulate across turns. Cost is estimated only when
// rates are configured (App.Rates); the estimate is always labelled
// with its source, and the provider's real price stays unverified.
type Runner struct {
	historyLoadErr          error
	App                     *App
	SessionUsage            llm.Usage
	RunUsage                llm.Usage
	RunCost                 float64
	SessionMetering         session.UsageTotals
	usageStore              *session.Store
	runBaseline             session.UsageTotals
	MeteringAvailable       bool
	runAccountingIncomplete bool
	SessionCost             float64 // USD estimate, accumulated per turn
	// Turns counts completed agent turns (slash commands excluded),
	// for the TUI meta line.
	Turns int
	// LastErr is the error of the most recent turn (nil on success).
	// The RPC server reads it to report a turn's outcome; the plain
	// CLI and TUI already print the error through Print.
	LastErr error
	// ConfirmSteal asks before /steal may signal another process. The CLI and
	// TUI set it; a nil callback refuses the steal instead of guessing. The
	// context ends when the frontend shuts down, so a pending prompt cannot
	// block process exit.
	ConfirmSteal func(ctx context.Context, id string) (bool, error)
	// OnCompact, when set, fires after a successful compaction
	// (manual or automatic) with the new message count and the
	// summary's token count. The RPC server sets it to emit the
	// compacted event; the CLI and TUI leave it nil and keep their
	// [compact] print.
	OnCompact func(messages, summaryTokens int)
	Print     func(format string, args ...any)
}

// ResetSession starts a fresh session and zeroes the runner's
// totals. It returns the new session id.
func (r *Runner) ResetSession() (string, error) {
	// A fresh session, but the session events do NOT fire here
	// (see App.newSession): /clear starts over, it does not switch
	// between sessions.
	store, err := r.App.newSession(false)
	if err != nil {
		return "", err
	}
	if err := r.RefreshUsage(); err != nil {
		return "", err
	}
	r.Turns = 0
	r.LastErr = nil
	return store.ID, nil
}

// ResumeSession restores lifetime totals and starts fresh current-run totals. It returns how many messages were loaded.
// Another process holding the writer returns ErrSessionBusy; use StealSession after the user confirms.
func (r *Runner) ResumeSession(id string) (int, error) {
	return r.finishResume(r.App.LoadSession(id))
}

// StealSession resumes id by taking its writer lease in the same handoff.
// rebind adopts a legacy transcript or moves the session, and only after confirmation.
func (r *Runner) StealSession(id string, rebind bool) (int, error) {
	return r.finishResume(r.App.StealSession(id, rebind))
}

func (r *Runner) printResumed(id string, n int) {
	if r.App.ResumedMessage != "" {
		r.Print("%s", r.App.ResumedMessage)
		return
	}
	r.Print("Resumed session %s (%d messages).", id, n)
}

func (r *Runner) finishResume(history []llm.Message, err error) (int, error) {
	if err != nil {
		return 0, err
	}
	r.usageStore = nil
	if err := r.RefreshUsage(); err != nil {
		return 0, err
	}
	r.Turns = 0
	r.LastErr = nil
	return len(history), nil
}

// costLine renders the estimate suffix for a usage line, or the
// unverified label when no rates are configured. The label names the
// provider from the base URL: a second provider must not inherit
// OpenConsole's name.
func (r *Runner) costLine(cost float64) string {
	label := r.App.Cfg.Client.ProviderName() + " price unverified"
	if !r.App.Rates.Configured {
		return "fee: " + label
	}
	return fmt.Sprintf("est. cost: ~$%.4f (rates: %s; %s)", cost, r.App.Rates.Source, label)
}

// ParsePluginCommand splits a "/name" or "/name args" line into the
// command name and the raw args string. It reports false for anything
// that is not a slash line, or whose first word is empty — the caller
// then checks HasCommand, and an unknown name falls through to the
// model as a task, like any other line.
func ParsePluginCommand(line string) (name, args string, ok bool) {
	if !strings.HasPrefix(line, "/") {
		return "", "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, "/"))
	if rest == "" {
		return "", "", false
	}
	name, args, _ = strings.Cut(rest, " ")
	return name, strings.TrimSpace(args), true
}

// RunCommand runs one plugin command by name and returns its output
// text. Handle prints it (REPL/TUI); the RPC server answers with it.
func (r *Runner) RunCommand(ctx context.Context, name, args string) (string, error) {
	if r.App.Plugins == nil {
		return "", fmt.Errorf("no plugins loaded")
	}
	return r.App.Plugins.CallCommand(ctx, name, args)
}

// Handle runs one input line. It returns true when the UI should quit.
func (r *Runner) Handle(ctx context.Context, line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if line == "/attach" || strings.HasPrefix(line, "/attach ") {
		r.handleAttach(strings.TrimSpace(strings.TrimPrefix(line, "/attach")))
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
	if line == "/plugins" {
		r.printPlugins()
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
			if errors.Is(err, session.ErrSessionBusy) {
				r.Print("Session %s is open for writing. /resume does not take it. Use /steal %s and confirm to take a proven stalled or dead owner.", id, id)
			}
			return false
		}
		r.printResumed(id, n)
		return false
	}
	if line == "/steal" || strings.HasPrefix(line, "/steal ") {
		id := strings.TrimSpace(strings.TrimPrefix(line, "/steal"))
		if id == "" {
			r.Print("Usage: /steal <id> (confirms, then takes a busy session whose owner is proven stalled or dead).")
			return false
		}
		if r.ConfirmSteal == nil {
			r.Print("[error] steal needs an explicit confirmation, and this frontend has none.")
			return false
		}
		ok, err := r.ConfirmSteal(ctx, id)
		if err != nil {
			r.Print("[error] %v", err)
			return false
		}
		if !ok {
			r.Print("Steal of session %s was not confirmed.", id)
			return false
		}
		n, err := r.StealSession(id, false)
		if err != nil {
			r.Print("[error] %v", err)
			return false
		}
		r.printResumed(id, n)
		return false
	}
	if line == "/rename" || strings.HasPrefix(line, "/rename ") {
		args := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "/rename")))
		if len(args) != 2 {
			r.Print("Usage: /rename <old> <new>.")
			return false
		}
		old, newID := args[0], args[1]
		err := r.App.RenameSession(old, newID)
		if err != nil {
			r.Print("[error] %v", err)
			return false
		}
		r.Print("Renamed session %s → %s.", old, newID)
		return false
	}
	if line == "/rollback" || strings.HasPrefix(line, "/rollback ") {
		arg := strings.TrimSpace(strings.TrimPrefix(line, "/rollback"))
		n := 1
		if arg != "" {
			if v, err := strconv.Atoi(arg); err != nil || v <= 0 {
				r.Print("Usage: /rollback [N] (drop the last N turns, default 1).")
				return false
			} else {
				n = v
			}
		}
		dropped, archive, err := r.Rollback(n)
		if err != nil {
			r.Print("[error] %v", err)
			return false
		}
		r.Print("Rolled back %d turn(s) (%d messages dropped). Pre-rollback transcript saved to %s.", n, dropped, archive)
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
	if line == "/permissions" || strings.HasPrefix(line, "/permissions ") {
		mode := strings.TrimSpace(strings.TrimPrefix(line, "/permissions"))
		if mode == "" {
			r.Print("Approval mode is %s. Use /permissions with %s.", r.App.Cfg.ApprovalMode(), approvalmode.Choices)
		} else if err := r.App.SetApprovalMode(mode); err != nil {
			r.Print("[error] %v", err)
		} else {
			r.Print("Approval mode set to %s.", mode)
		}
		return false
	}
	if arg, matched := ParseJobsStopArg(line); matched {
		if arg == "" {
			r.Print("Usage: /jobs stop <job-id>")
			return false
		}
		res, err := r.App.Cfg.Exec.StopJob(arg)
		if r.App.Store != nil {
			if saved, saveErr := agent.SaveJobResultsMessages(r.App.Store, r.App.Cfg.Exec.Jobs()); saveErr == nil && len(saved) > 0 {
				r.App.History = append(r.App.History, saved...)
			}
		}
		if err != nil {
			r.Print("[error] %v", err)
			if res.Text != "" {
				r.Print("%s", res.Text)
			}
			return false
		}
		r.Print("%s", res.Text)
		return false
	}
	switch line {
	case "/quit", "/exit":
		if err := r.App.Close(); err != nil {
			r.Print("[error] close session jobs: %v", err)
			return false
		}
		return true
	case "/usage":
		if err := r.RefreshUsage(); err != nil {
			r.LastErr = err
			r.Print("[error] %v", err)
			return false
		}
		r.Print("[usage] session: %s | %s | %s\n[usage] current run: %s | %s", r.sessionUsageLine(), r.sessionCostLine(), r.usageCoverage(), r.runUsageLine(), r.runCostLine())
		return false
	case "/prune-artifacts":
		report, err := session.PruneArtifacts(ctx, time.Now())
		r.Print("[retention] removed %d unreferenced image blobs (%d bytes)", report.DeletedBlobs, report.ReclaimedBytes)
		if err != nil {
			r.LastErr = err
			r.Print("[error] %v", err)
		}
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
		if err := r.RefreshUsage(); err != nil {
			r.LastErr = err
			r.Print("[error] %v", err)
			return false
		}
		r.doCompact(ctx)
		return false
	}
	// Plugin commands (/plan, /execute, ...): after the built-ins,
	// before an unknown slash line falls through to the model as a
	// task. A command is user-invoked, so it needs no approval.
	if name, args, ok := ParsePluginCommand(line); ok {
		if r.App.Plugins != nil && r.App.Plugins.HasCommand(name) {
			out, err := r.RunCommand(ctx, name, args)
			if err != nil {
				r.Print("[error] %v", err)
			} else {
				r.Print("%s", out)
			}
			return false
		}
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
	r.Print("/resume <id> to switch, /steal <id> to take a busy session after confirmation, /rename <old> <new> to rename.")
}

// printPlugins lists every discovered plugin with its tools and
// state (loaded / disabled / skipped with reason), in directory
// order. A plugin that never loaded still shows, so a typo in
// plugins.disabled or a broken entry is visible here. Like
// /sessions, this goes through the normal Print path, so it works
// in the TUI too. Restart to pick up changes: there is no hot
// reload, and disabling is config-file editing only.
func (r *Runner) printPlugins() {
	reg := r.App.Plugins
	if reg == nil {
		r.Print("No plugins found.")
		return
	}
	infos := reg.Infos()
	if len(infos) == 0 {
		r.Print("No plugins found in %s.", reg.Dir())
		return
	}
	r.Print("Plugins in %s:", reg.Dir())
	for _, info := range infos {
		parts := []string{info.Name}
		if info.Version != "" {
			parts = append(parts, info.Version)
		}
		parts = append(parts, "["+info.State+"]")
		if info.Description != "" {
			parts = append(parts, info.Description)
		}
		if len(info.Tools) > 0 {
			parts = append(parts, "tools: "+strings.Join(info.Tools, ", "))
		}
		if info.Reason != "" {
			parts = append(parts, "("+info.Reason+")")
		}
		r.Print("%s", strings.Join(parts, "  "))
	}
	r.Print("Edit plugins.disabled in the config file and restart to change which plugins load.")
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

// ParseJobsStopArg parses interactive "/jobs stop [id]" or "/jobs kill [id]".
func ParseJobsStopArg(line string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) >= 2 && fields[0] == "/jobs" && (fields[1] == "stop" || fields[1] == "kill") {
		if len(fields) == 2 {
			return "", true
		}
		return strings.Join(fields[2:], " "), true
	}
	return "", false
}

// printJobs lists the background bash jobs of this process, oldest
// first: id, status, elapsed time, command, and exit code once done.
func (r *Runner) printJobs() {
	r.App.RefreshOrphanedJobs()
	jobs := r.App.Cfg.Exec.Jobs()
	if len(jobs) == 0 {
		r.Print("No background jobs.")
		if notice := r.App.OrphanedNotice(); notice != "" {
			r.Print("%s", notice)
		}
		return
	}
	for _, j := range jobs {
		snap := j.Snapshot()
		cmd := snap.Command
		if len(cmd) > 80 {
			cmd = cmd[:77] + "…"
		}
		line := fmt.Sprintf("%s  %s  %s  %s", snap.ID, snap.Status, j.Elapsed().Round(time.Second), cmd)
		if snap.Identity.PID > 0 {
			line += fmt.Sprintf("  pid=%d", snap.Identity.PID)
			if snap.Adopted {
				line += " (adopted)"
			}
		}
		if snap.Cwd != "" {
			line += fmt.Sprintf("  cwd=%s", snap.Cwd)
		}
		if snap.OutputFile != "" {
			line += fmt.Sprintf("  output=%s", snap.OutputFile)
		}
		if snap.Status == tools.JobDone {
			if snap.Result.ExitCodeUnknown {
				line += "  exit unavailable"
			} else {
				line += fmt.Sprintf("  exit %d", snap.Result.ExitCode)
			}
			if snap.Result.Err != nil {
				line += fmt.Sprintf("  (%v)", snap.Result.Err)
			}
		} else if (snap.Status == tools.JobUnknown || snap.WaitError != "") && snap.RecoveryNote != "" {
			line += fmt.Sprintf("  (%s)", snap.RecoveryNote)
		}
		r.Print("%s", line)
	}
	if notice := r.App.OrphanedNotice(); notice != "" {
		r.Print("%s", notice)
	}
}

func (r *Runner) runOnce(ctx context.Context, task string) {
	if err := r.RefreshUsage(); err != nil {
		r.LastErr = err
		r.Print("[error] %v", err)
		return
	}
	if r.historyLoadErr != nil {
		msgs, err := session.Load(r.App.Store.ID)
		if err != nil {
			r.LastErr = fmt.Errorf("cannot continue with stale history: %w", err)
			r.Print("[error] %v", r.LastErr)
			return
		}
		r.App.hydrateImages(msgs)
		r.App.History = msgs
		r.historyLoadErr = nil
	}
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
					r.App.History = append(r.App.History, llm.Message{Role: "user", Kind: "skill", Content: "Skill " + name + ":\n" + body})
					_ = r.App.Store.Append(r.App.History[len(r.App.History)-1])
				}
				return
			}
		}
		r.Print("Unknown skill: %s", name)
		return
	}
	userMsg := llm.Message{
		Role: "user", Kind: "task", Content: task,
		Images: r.App.PendingImages, Files: r.App.PendingFiles,
	}
	if err := r.App.checkAttachmentContext(userMsg); err != nil {
		r.LastErr = err
		r.Print("[error] %v", err)
		return
	}
	if err := r.App.Store.Append(userMsg); err != nil {
		r.LastErr = fmt.Errorf("save session: %w", err)
		r.Print("[error] %v", r.LastErr)
		return
	}
	r.App.PendingImages = nil
	r.App.PendingFiles = nil
	r.App.History = append(r.App.History, userMsg)
	r.Turns++
	// Context hooks retain their explicit, transient rewrite semantics.
	// The harness snapshot is appended afterwards, so mode and plugin state
	// no longer change the system prefix or disappear on the next turn.
	history := agent.WithoutHistoricalGuidance(r.App.History)
	if r.App.Plugins != nil {
		history = r.App.Plugins.RunContextHooks(ctx, r.App.sessionID(), string(r.App.ToolMode()), history)
	}
	contrib := ""
	if r.App.Plugins != nil {
		contrib = r.App.Plugins.RunBeforeAgentStartHooks(ctx, r.App.sessionID(), string(r.App.ToolMode()))
	}
	// Hooks may switch the tool mode. Capture both the final mode and its
	// queued reminder only after all before-agent-start hooks have finished.
	turnContext := r.App.turnContext(contrib)
	if err := r.App.Store.Append(turnContext); err != nil {
		r.LastErr = fmt.Errorf("save turn context: %w", err)
		r.Print("[error] %v", r.LastErr)
		return
	}
	r.App.pendingReminder = ""
	r.App.History = append(r.App.History, turnContext)
	history = append(append([]llm.Message(nil), history...), turnContext)
	r.configureCompaction()
	previousCost := r.SessionCost
	previousMetering := r.SessionMetering
	text, usage, err := r.App.Cfg.RunTask(ctx, history, userMsg.Content)
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
	} else {
		loadErr := fmt.Errorf("reload session: %w", err)
		r.historyLoadErr = loadErr
		r.LastErr = errors.Join(r.LastErr, loadErr)
		r.App.Cfg.LastTerminalReason = agent.TerminalError
		r.App.Cfg.LastTaskOutcome = agent.OutcomeIncomplete
		if r.App.Cfg.OnEvent != nil {
			r.App.Cfg.OnEvent(agent.Event{Kind: agent.EventError, Err: r.LastErr, Text: text, TerminalReason: agent.TerminalError, TaskOutcome: agent.OutcomeIncomplete})
		}
		r.Print("[error] %v", loadErr)
	}
	turnCost := r.accountRun(usage, previousCost)
	// One Print call for both lines: consecutive tea.Println calls
	// reverse their order in Bubble Tea v2 (verified), so the turn
	// and session lines must travel as a single print.
	turnLine := fmt.Sprintf("[usage] turn: %s", usage)
	if note := r.unenforcedCostNote(); note != "" {
		turnLine += " | " + note
	} else if r.MeteringAvailable && r.App.Rates.Configured && r.SessionMetering.UnpricedAttempts() == previousMetering.UnpricedAttempts() {
		turnLine += " | " + r.costLine(turnCost)
	}
	if !r.MeteringAvailable {
		r.Print("\n%s\n[usage] session: unavailable (accounting error)", turnLine)
		return
	}
	r.Print("\n%s\n[usage] session: %s | %s", turnLine, r.sessionUsageLine(), r.sessionCostLine())
}

func (a *App) turnContext(contrib string) llm.Message {
	parts := []string{fmt.Sprintf("[harness turn context]\nState at the start of this user task. Tool mode: %s. Earlier snapshots are historical. Later tool results and conversation summaries may update this state.", a.ToolMode())}
	if a.pendingReminder != "" {
		parts = append(parts, a.pendingReminder)
	}
	if contrib != "" {
		parts = append(parts, contrib)
	}
	return llm.Message{Role: "user", Kind: "turn_context", Content: strings.Join(parts, "\n")}
}

// Rollback drops the last n turns from the session — Codex's
// thread/rollback, as the GUI's undo. The pre-rollback transcript
// is archived first (like compaction), then the session file is
// rewritten to the kept prefix and the in-memory history follows.
// It returns the dropped message count and the archive path. The
// session usage totals are not rewound: the tokens were spent.
func (r *Runner) Rollback(n int) (dropped int, archive string, err error) {
	if n <= 0 {
		return 0, "", fmt.Errorf("rollback needs a positive turn count, got %d", n)
	}
	kept, dropped := session.Rollback(r.App.History, n)
	if dropped == 0 {
		return 0, "", fmt.Errorf("nothing to roll back: no turns in this session")
	}
	archive, err = r.App.Store.ArchiveCopy(fmt.Sprintf("%s-pre-rollback-%d", r.App.Store.ID, time.Now().UnixNano()))
	if err != nil {
		return 0, "", err
	}
	if err := r.App.Store.Rewrite(kept); err != nil {
		return 0, archive, err
	}
	r.App.History = kept
	if r.Turns > n {
		r.Turns -= n
	} else {
		r.Turns = 0
	}
	r.LastErr = nil
	_ = r.App.Store.Audit(tools.AuditEntry{TS: time.Now(), Kind: "rollback", Args: fmt.Sprintf("turns=%d dropped=%d", n, dropped), ExitCode: -1})
	return dropped, archive, nil
}

// doCompact summarises history, saves the original transcript to a
// separate file, and replaces history with the summary.
func (r *Runner) doCompact(ctx context.Context) {
	r.LastErr = nil
	if len(r.App.History) == 0 {
		r.Print("[compact] nothing to compact.")
		return
	}
	r.configureCompaction()
	previousCost := r.SessionCost
	_, usage, err := r.App.Cfg.Compact(ctx, r.App.History)
	r.accountRun(usage, previousCost)
	if err != nil {
		r.LastErr = err
		r.Print("[compact] failed: %v (history unchanged)", err)
		if note := r.unenforcedCostNote(); note != "" {
			r.Print("[compact] %s", note)
		}
		return
	}
	if note := r.unenforcedCostNote(); note != "" {
		r.Print("[compact] %s", note)
	}
	r.App.Cfg.LastPromptTokens = 0
}

// configureCompaction keeps persistence in the agent and rendering in the app.
func (r *Runner) configureCompaction() {
	r.App.Cfg.CompactHistory = r.compactHistory
	r.App.Cfg.OnCompact = func(history []llm.Message, usage llm.Usage, archive string) {
		r.App.History = history
		if r.OnCompact != nil {
			r.OnCompact(len(history), usage.CompletionTokens)
		}
		r.Print("[compact] context checkpoint saved; original transcript saved to %s", archive)
	}
}

// compactHistory supplies plugin instructions; the agent commits the checkpoint.
func (r *Runner) compactHistory(ctx context.Context, history []llm.Message) (string, llm.Usage, error) {
	var extras []string
	if r.App.Plugins != nil {
		extras = r.App.Plugins.RunSessionBeforeCompactHooks(ctx, r.App.sessionID(), len(history))
	}
	return r.App.Cfg.Summarize(ctx, history, extras)
}
