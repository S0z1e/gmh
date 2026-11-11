// Package plugins runs JavaScript/TypeScript plugins in moejs runtimes
// and exposes the tools they register to the agent loop.
//
// A plugin is one directory under <root>/.agents/plugins/ holding a
// plugin.json manifest ({name, description, version, entry}) and the
// entry file (.js, or .ts compiled with moejs CompileTS, which strips
// types only). One moejs runtime per plugin, memory-limited, no eval
// (DisableDynamicCode), one tool call at a time per runtime, with a
// timeout backed by Interrupt.
//
// The host API a plugin sees is the single global `harness`:
//   - harness.registerTool(name, description, parameters, handler)
//   - harness.registerCommand(name, description, handler)
//   - harness.on("tool_call" | "tool_result", handler)
//   - harness.on("before_agent_start" | "context" |
//     "session_before_compact" | "session_start" |
//     "session_switch", handler)
//   - harness.setMode(name) / harness.getMode()
//   - harness.log(...args)
//   - harness.readFile(path) / harness.writeFile(path, content)
//
// The file functions are jailed to the plugin's own data directory
// (<root>/.agents/plugins/<name>/data/): no arbitrary fs, no bash,
// no network in Phase 1. Plugin tools run JS only, so they need no
// approval in suggest mode.
//
// Loading is disabled with HARNESS_PLUGINS=0 (or false). A missing
// plugins directory loads nothing, no error. A plugin that fails to
// load is skipped with a warning; it never fails Setup. Load also
// records what it discovered (Registry.Infos): every plugin
// directory, loaded, disabled, or skipped with its reason — so
// /plugins and RPC list_plugins can show plugins that never loaded.
package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moejs"

	"minimalharness/internal/llm"
)

const (
	// DefaultMemoryLimit bounds one plugin runtime's JS heap.
	DefaultMemoryLimit = 16 << 20
	// DefaultCallTimeout bounds one plugin tool call.
	DefaultCallTimeout = 10 * time.Second
	// interruptGrace is how long Call waits for a runaway call to stop
	// after Interrupt before giving up on the runtime.
	interruptGrace = 5 * time.Second
	// MaxStoreBytes caps one file in a plugin's data dir, read or written.
	MaxStoreBytes = 1 << 20
	// MaxObservationRunes caps one plugin tool's observation text.
	MaxObservationRunes = 32 << 10
)

// builtinTools are the harness's own tool names: a plugin may not
// register one, or the model could not tell whose tool it called.
var builtinTools = map[string]bool{
	"read": true, "write": true, "edit": true, "bash": true,
	"grep": true, "glob": true, "load_skill": true, "webfetch": true,
}

var validToolName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Manifest is a plugin's plugin.json.
type Manifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Entry       string `json:"entry"`
}

func validateManifest(m Manifest) error {
	if !validToolName.MatchString(m.Name) {
		return fmt.Errorf("name %q: use lowercase letters, digits and underscores, starting with a letter", m.Name)
	}
	if strings.TrimSpace(m.Description) == "" {
		return fmt.Errorf("description is required (the model reads it)")
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("version is required")
	}
	if strings.TrimSpace(m.Entry) == "" {
		return fmt.Errorf("entry is required (the .js or .ts file)")
	}
	return nil
}

// Tool is one function a plugin registered with registerTool.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	handler     moejs.Value
	plugin      *Plugin
}

// Command is one slash command a plugin registered with
// registerCommand: user-invoked (never model-invoked), so it needs
// no approval and never enters ToolDefs.
type Command struct {
	Name        string
	Description string
	handler     moejs.Value
	plugin      *Plugin
}

// Plugin is one loaded plugin: its runtime, its data dir, and the
// tools it registered while loading.
type Plugin struct {
	Name        string
	Description string
	Version     string
	Dir         string
	DataDir     string

	rt      *moejs.Runtime
	mu      sync.Mutex // serialises calls into this runtime (a runtime runs on one goroutine; only Interrupt is concurrent-safe)
	loading bool       // true while the entry's top level runs: registerTool is only valid then
	broken  bool       // true when a call never stopped after Interrupt: refuse further calls
	// registry is the owning registry: setMode/getMode read its
	// ModeSetter/ModeGetter, which app.Setup wires after Load.
	registry *Registry
	tools    []*Tool
	byName   map[string]*Tool
	// commands registered at load time, by name.
	commands       []*Command
	commandsByName map[string]*Command
	// Hooks, at most one per event per plugin (see harness.on).
	toolCallHook      moejs.Value
	hasToolCallHook   bool
	toolResultHook    moejs.Value
	hasToolResultHook bool
	// Phase 4 lifecycle hooks, at most one per event per plugin.
	// All four fail open (unlike tool_call, which fails closed):
	// they add context, instructions or notifications, never a gate.
	beforeAgentStartHook        moejs.Value
	hasBeforeAgentStartHook     bool
	contextHook                 moejs.Value
	hasContextHook              bool
	sessionBeforeCompactHook    moejs.Value
	hasSessionBeforeCompactHook bool
	sessionStartHook            moejs.Value
	hasSessionStartHook         bool
	sessionSwitchHook           moejs.Value
	hasSessionSwitchHook        bool
}

// Registry is the loaded plugins of one run, indexed by tool name.
type Registry struct {
	plugins   []*Plugin
	byTool    map[string]*Tool
	byCommand map[string]*Command
	// CallTimeout bounds one plugin call (tool, command, or hook)
	// (0 = DefaultCallTimeout).
	CallTimeout time.Duration
	// ModeSetter switches the agent's tool mode; ModeGetter reads
	// it. app.Setup wires both after Load (the mode lives on the
	// App, not here); a registry without them reports "default" and
	// refuses setMode, so standalone uses never panic.
	ModeSetter func(string) error
	ModeGetter func() string
	// dir is the plugins directory Load read (<root>/.agents/plugins).
	dir string
	// infos is the discovery outcome: one entry per plugin directory
	// found, in directory order (loaded, disabled, or skipped).
	infos []Info
}

// Info is what Load discovered about one plugin directory: a loaded
// plugin (with the tools the model can actually call), a disabled
// one, or a skipped one with its reason.
type Info struct {
	Name        string
	Version     string
	Description string
	// Tools are the plugin's tool names, in registration order.
	// Only the tools that survived loading are listed (a tool lost
	// in a cross-plugin name clash is not listed).
	Tools []string
	// State is "loaded", "disabled" or "skipped".
	State string
	// Reason says why a plugin is disabled or skipped ("" otherwise).
	Reason string
}

// Dir returns the plugins directory Load read.
func (r *Registry) Dir() string { return r.dir }

// Infos returns the discovery outcome: one entry per plugin
// directory found, in directory order.
func (r *Registry) Infos() []Info { return r.infos }

// Plugins returns the loaded plugins in directory order.
func (r *Registry) Plugins() []*Plugin { return r.plugins }

// ToolDefs returns the llm.ToolDef for every registered plugin tool,
// in load order (plugin directory order, then registration order).
// Tools skipped for a cross-plugin name clash are not listed.
func (r *Registry) ToolDefs() []llm.ToolDef {
	var out []llm.ToolDef
	for _, p := range r.plugins {
		for _, t := range p.tools {
			if r.byTool[t.Name] != t {
				continue
			}
			out = append(out, llm.Tool(t.Name, t.Description, string(t.Parameters)))
		}
	}
	return out
}

// Has reports whether a plugin tool of this name is registered.
func (r *Registry) Has(name string) bool {
	_, ok := r.byTool[name]
	return ok
}

// Load discovers and loads plugins from <root>/.agents/plugins/.
// HARNESS_PLUGINS=0 (or false) disables loading. A missing plugins
// directory loads nothing. Each failure is reported, one error per
// bad plugin, and the bad plugin is skipped; Load never fails the
// run. Directories are loaded in sorted order, so a tool-name clash
// between two plugins is resolved deterministically (first wins).
//
// disabled names plugins to skip entirely: their manifest is not
// read for tool purposes, their entry is never compiled, and no
// runtime is created. They still appear in Registry.Infos as
// disabled, with the manifest's name/version/description when it
// parses. A disabled name that matches no plugin directory is
// reported as an error (a visible warning at load, so a typo is
// caught), not a load failure.
func Load(root string, disabled []string) (*Registry, []error) {
	reg := &Registry{byTool: map[string]*Tool{}, byCommand: map[string]*Command{}}
	dir := filepath.Join(root, ".agents", "plugins")
	reg.dir = dir
	disabledSet := map[string]bool{}
	for _, d := range disabled {
		disabledSet[d] = true
	}
	if v := os.Getenv("HARNESS_PLUGINS"); v == "0" || strings.EqualFold(v, "false") {
		// Loading is off, but the listing still shows what was
		// found, as disabled.
		if entries, err := os.ReadDir(dir); err == nil {
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			for _, en := range entries {
				if !en.IsDir() {
					continue
				}
				reg.infos = append(reg.infos, describe(filepath.Join(dir, en.Name()), en.Name(), "disabled", "plugin loading disabled (HARNESS_PLUGINS=0)"))
			}
		}
		return reg, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return reg, []error{fmt.Errorf("read plugins dir %s: %w", dir, err)}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var errs []error
	seen := map[string]bool{}
	for _, en := range entries {
		if !en.IsDir() {
			continue
		}
		seen[en.Name()] = true
		pdir := filepath.Join(dir, en.Name())
		if disabledSet[en.Name()] {
			reg.infos = append(reg.infos, describe(pdir, en.Name(), "disabled", ""))
			continue
		}
		p, err := loadOne(reg, pdir)
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin %s: %w", en.Name(), err))
			reg.infos = append(reg.infos, describe(pdir, en.Name(), "skipped", err.Error()))
			continue
		}
		for _, t := range p.tools {
			if _, ok := reg.byTool[t.Name]; ok {
				errs = append(errs, fmt.Errorf("plugin %s: tool %q is already registered by another plugin, skipped", p.Name, t.Name))
				continue
			}
			reg.byTool[t.Name] = t
		}
		for _, c := range p.commands {
			if _, ok := reg.byCommand[c.Name]; ok {
				errs = append(errs, fmt.Errorf("plugin %s: command %q is already registered by another plugin, skipped", p.Name, c.Name))
				continue
			}
			reg.byCommand[c.Name] = c
		}
		reg.plugins = append(reg.plugins, p)
		info := Info{Name: p.Name, Version: p.Version, Description: p.Description, Tools: []string{}, State: "loaded"}
		for _, t := range p.tools {
			if reg.byTool[t.Name] == t {
				info.Tools = append(info.Tools, t.Name)
			}
		}
		reg.infos = append(reg.infos, info)
	}
	for _, d := range disabled {
		if !seen[d] {
			errs = append(errs, fmt.Errorf("plugin %q is listed in plugins.disabled but no such plugin was found", d))
		}
	}
	return reg, errs
}

// describe builds the Info for a plugin that never loaded: the
// manifest's name/version/description when plugin.json parses, the
// directory name otherwise (version/description empty).
func describe(dir, dirName, state, reason string) Info {
	info := Info{Name: dirName, Tools: []string{}, State: state, Reason: reason}
	if raw, err := os.ReadFile(filepath.Join(dir, "plugin.json")); err == nil {
		var m Manifest
		if err := json.Unmarshal(raw, &m); err == nil {
			if m.Name != "" {
				info.Name = m.Name
			}
			info.Version = m.Version
			info.Description = m.Description
		}
	}
	return info
}

func loadOne(reg *Registry, dir string) (*Plugin, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return nil, fmt.Errorf("read plugin.json: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("plugin.json: %w", err)
	}
	if err := validateManifest(m); err != nil {
		return nil, err
	}
	// The directory is the plugin's identity (its data dir lives
	// under it); the manifest must agree, or the error would name a
	// plugin that is not where the author thinks it is.
	if filepath.Base(dir) != m.Name {
		return nil, fmt.Errorf("plugin.json name %q does not match directory %q", m.Name, filepath.Base(dir))
	}
	entry := filepath.Clean(m.Entry)
	if filepath.IsAbs(entry) || entry == ".." || strings.HasPrefix(entry, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("entry %q must be a relative path inside the plugin directory", m.Entry)
	}
	ext := strings.ToLower(filepath.Ext(entry))
	if ext != ".js" && ext != ".ts" {
		return nil, fmt.Errorf("entry %q must be a .js or .ts file", m.Entry)
	}
	src, err := os.ReadFile(filepath.Join(dir, entry))
	if err != nil {
		return nil, fmt.Errorf("read entry %q: %w", m.Entry, err)
	}
	var mod *moejs.Module
	if ext == ".ts" {
		// CompileTS strips types only: runtime-only TS (enum,
		// namespaces, parameter properties) fails here.
		mod, err = moejs.CompileTS(m.Entry, string(src))
	} else {
		mod, err = moejs.Compile(m.Entry, string(src))
	}
	if err != nil {
		return nil, fmt.Errorf("compile entry %q: %w", m.Entry, err)
	}
	p := &Plugin{
		Name: m.Name, Description: m.Description, Version: m.Version,
		Dir: dir, DataDir: filepath.Join(dir, "data"),
		registry:       reg,
		byName:         map[string]*Tool{},
		commandsByName: map[string]*Command{},
		loading:        true,
	}
	p.rt = moejs.NewRuntime(moejs.Options{
		MemoryLimit:        DefaultMemoryLimit,
		DisableDynamicCode: true,
	})
	if err := p.installHostAPI(); err != nil {
		return nil, fmt.Errorf("install host API: %w", err)
	}
	// Top-level code runs here: that is when registerTool is called.
	// A throw, or a registration error, fails this plugin's load.
	if err := p.rt.Load(mod); err != nil {
		return nil, fmt.Errorf("load entry %q: %w", m.Entry, err)
	}
	p.loading = false
	return p, nil
}

type callResult struct {
	res moejs.Value
	err error
}

// Call runs a plugin tool with the model's args JSON and returns the
// observation text. The handler receives the args as one JS object;
// its return value becomes the observation (a string as-is, anything
// else as JSON). An async handler is unwrapped with PromiseResult: a
// promise that is still pending when the call returns is an error —
// Phase 1 has no event loop, so an async handler must settle
// without awaiting anything host-driven.
func (r *Registry) Call(ctx context.Context, name, argsJSON string) (string, error) {
	t, ok := r.byTool[name]
	if !ok {
		return "", fmt.Errorf("unknown plugin tool: %s", name)
	}
	return t.plugin.call(ctx, t, argsJSON, r.timeout())
}

// timeout returns the call timeout for this registry (0 = default).
func (r *Registry) timeout() time.Duration {
	if r.CallTimeout > 0 {
		return r.CallTimeout
	}
	return DefaultCallTimeout
}

// HasCommand reports whether a plugin command of this name is registered.
func (r *Registry) HasCommand(name string) bool {
	_, ok := r.byCommand[name]
	return ok
}

// CallCommand runs a plugin command with the raw args string (the
// text after "/name", unparsed) and returns its output text.
func (r *Registry) CallCommand(ctx context.Context, name, args string) (string, error) {
	c, ok := r.byCommand[name]
	if !ok {
		return "", fmt.Errorf("unknown plugin command: %s", name)
	}
	return c.plugin.callCommand(ctx, c, args, r.timeout())
}

// RunToolCallHooks runs the registered tool_call hooks in load order,
// before approval. It returns the (possibly rewritten) args JSON and
// a denial: "" when the call is allowed, otherwise
// "plugin <name>: <reason>". A hook that throws, times out, or
// returns a malformed value denies too (fail closed). The first
// denial stops the chain; arg rewrites chain on to the next hook.
func (r *Registry) RunToolCallHooks(ctx context.Context, tool, argsJSON string) (string, string) {
	argsJSON = hookArgsJSON(argsJSON)
	for _, p := range r.plugins {
		if !p.hasToolCallHook {
			continue
		}
		res, err := p.callHook(ctx, p.toolCallHook, hookCallInput(tool, argsJSON), r.timeout())
		if err != nil {
			return argsJSON, fmt.Sprintf("%s: tool_call hook failed: %v", p.Name, err)
		}
		deny, rewrite, bad := interpretToolCallResult(p, res)
		if bad != "" {
			return argsJSON, fmt.Sprintf("%s: %s", p.Name, bad)
		}
		if deny != "" {
			return argsJSON, fmt.Sprintf("%s: %s", p.Name, deny)
		}
		if rewrite != "" {
			argsJSON = rewrite
		}
	}
	return argsJSON, ""
}

// RunToolResultHooks runs the registered tool_result hooks in load
// order, each seeing the previous one's rewrite, and returns the
// observation to use. A hook that throws, times out, or returns a
// non-string keeps the current result and is logged to stderr
// (fail open: the result is already in hand, denying it would only
// break the turn).
func (r *Registry) RunToolResultHooks(ctx context.Context, tool, argsJSON, result string, toolErr error) string {
	argsJSON = hookArgsJSON(argsJSON)
	for _, p := range r.plugins {
		if !p.hasToolResultHook {
			continue
		}
		res, err := p.callHook(ctx, p.toolResultHook, hookResultInput(tool, argsJSON, result, toolErr), r.timeout())
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: plugin %s tool_result hook failed: %v\n", p.Name, err)
			continue
		}
		if s, ok := hookResultString(p, res); ok {
			result = s
		}
	}
	return result
}

// RunBeforeAgentStartHooks runs the registered before_agent_start
// hooks in load order and returns the contributions as one system
// message (each prefixed "[plugin <name>] ", empty contributions
// dropped). A hook that throws, times out, or returns a non-string
// is skipped with a stderr warning (fail open: the contribution is
// additive context, never a gate).
func (r *Registry) RunBeforeAgentStartHooks(ctx context.Context, session, mode string) string {
	var parts []string
	for _, p := range r.plugins {
		if !p.hasBeforeAgentStartHook {
			continue
		}
		res, err := p.callHook(ctx, p.beforeAgentStartHook, hookSessionModeInput(session, mode), r.timeout())
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: plugin %s before_agent_start hook failed: %v\n", p.Name, err)
			continue
		}
		if s, ok := hookTextResult(p, "before_agent_start", res); ok {
			parts = append(parts, "[plugin "+p.Name+"] "+s)
		}
	}
	return strings.Join(parts, "\n")
}

// RunContextHooks runs the registered context hooks in load order,
// each seeing the previous one's rewrite, and returns the history to
// use for this turn. A hook that throws, times out, or returns an
// invalid list keeps the current list (fail open). The returned list
// is a fresh slice; the session file keeps the true history, so a
// rewrite never persists. Image bytes, which never travel as JSON,
// are restored from the original history by path (a hook cannot
// inject image bytes it never had).
func (r *Registry) RunContextHooks(ctx context.Context, session, mode string, msgs []llm.Message) []llm.Message {
	for _, p := range r.plugins {
		if !p.hasContextHook {
			continue
		}
		input, err := hookContextInput(session, mode, msgs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: plugin %s context hook input failed: %v\n", p.Name, err)
			continue
		}
		res, err := p.callHook(ctx, p.contextHook, input, r.timeout())
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: plugin %s context hook failed: %v\n", p.Name, err)
			continue
		}
		if next, ok := hookMessagesResult(p, res); ok {
			msgs = rehydrateImages(msgs, next)
		}
	}
	return msgs
}

// RunSessionBeforeCompactHooks runs the registered
// session_before_compact hooks in load order and returns the extra
// instructions, each prefixed "[plugin <name>] ". A hook that throws,
// times out, or returns a non-string is skipped with a warning
// (fail open: the compact runs with the six fixed sections).
func (r *Registry) RunSessionBeforeCompactHooks(ctx context.Context, session string, messageCount int) []string {
	var out []string
	for _, p := range r.plugins {
		if !p.hasSessionBeforeCompactHook {
			continue
		}
		res, err := p.callHook(ctx, p.sessionBeforeCompactHook, hookCompactInput(session, messageCount), r.timeout())
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: plugin %s session_before_compact hook failed: %v\n", p.Name, err)
			continue
		}
		if s, ok := hookTextResult(p, "session_before_compact", res); ok {
			out = append(out, "[plugin "+p.Name+"] "+s)
		}
	}
	return out
}

// RunSessionStartHooks runs the registered session_start hooks in load
// order. They are notifications: return values are ignored, and a
// failing hook is warned about but never fails anything.
func (r *Registry) RunSessionStartHooks(ctx context.Context, session string) {
	for _, p := range r.plugins {
		if !p.hasSessionStartHook {
			continue
		}
		if _, err := p.callHook(ctx, p.sessionStartHook, hookSessionInput(session), r.timeout()); err != nil {
			fmt.Fprintf(os.Stderr, "warning: plugin %s session_start hook failed: %v\n", p.Name, err)
		}
	}
}

// RunSessionSwitchHooks runs the registered session_switch hooks in
// load order (same notification rules as RunSessionStartHooks).
func (r *Registry) RunSessionSwitchHooks(ctx context.Context, session, previous string) {
	for _, p := range r.plugins {
		if !p.hasSessionSwitchHook {
			continue
		}
		if _, err := p.callHook(ctx, p.sessionSwitchHook, hookSessionSwitchInput(session, previous), r.timeout()); err != nil {
			fmt.Fprintf(os.Stderr, "warning: plugin %s session_switch hook failed: %v\n", p.Name, err)
		}
	}
}

// hookTextResult reads a text-returning hook's return value:
// (s, true) is a contribution; ("", false) is none (undefined,
// empty, or a malformed value, which is warned about).
func hookTextResult(p *Plugin, event string, res moejs.Value) (string, bool) {
	g, err := p.rt.ToGo(res)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: plugin %s %s hook returned an unreadable value: %v\n", p.Name, event, err)
		return "", false
	}
	switch v := g.(type) {
	case nil:
		return "", false // undefined: no contribution
	case string:
		if strings.TrimSpace(v) == "" {
			return "", false
		}
		return v, true
	default:
		fmt.Fprintf(os.Stderr, "warning: plugin %s %s hook returned a non-string value; ignoring it\n", p.Name, event)
		return "", false
	}
}

// hookMessagesResult reads a context hook's return value:
// (list, true) is the rewritten history; (nil, false) keeps the
// current list (undefined, or an invalid value, which is warned
// about). Only the roles are validated — deeper checks would reject
// legitimate rewrites — and a system message is rejected: the
// harness prepends c.System itself, so a hook must never inject
// one (it would be a forged system prompt).
func hookMessagesResult(p *Plugin, res moejs.Value) ([]llm.Message, bool) {
	g, err := p.rt.ToGo(res)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: plugin %s context hook returned an unreadable value: %v\n", p.Name, err)
		return nil, false
	}
	if g == nil {
		return nil, false // undefined: keep the current list
	}
	b, err := p.rt.AppendJSON(nil, res)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: plugin %s context hook returned a value that is not JSON: %v\n", p.Name, err)
		return nil, false
	}
	var out []llm.Message
	if err := json.Unmarshal(b, &out); err != nil {
		fmt.Fprintf(os.Stderr, "warning: plugin %s context hook returned a value that is not a message list; keeping the original history\n", p.Name)
		return nil, false
	}
	for _, m := range out {
		switch m.Role {
		case "user", "assistant", "tool":
		default:
			// "system" is rejected too: the harness prepends
			// c.System itself, so a hook must never inject a
			// system message (a forged system prompt would
			// override the harness's own instructions).
			fmt.Fprintf(os.Stderr, "warning: plugin %s context hook returned a message with role %q; keeping the original history\n", p.Name, m.Role)
			return nil, false
		}
	}
	return out, true
}

// rehydrateImages restores image bytes on a context hook's rewritten
// history: the hook input JSON carries paths only (Data is json:"-"),
// so a verbatim round-trip would silently drop the bytes. Images are
// matched by path; an image the hook invented (no matching path in
// the original) is dropped — a hook cannot inject image bytes it
// never had.
func rehydrateImages(orig, rewritten []llm.Message) []llm.Message {
	byPath := map[string]llm.Image{}
	for _, m := range orig {
		for _, img := range m.Images {
			byPath[img.Path] = img
		}
	}
	for i := range rewritten {
		if len(rewritten[i].Images) == 0 {
			continue
		}
		kept := rewritten[i].Images[:0]
		for _, img := range rewritten[i].Images {
			if src, ok := byPath[img.Path]; ok {
				img.Data = src.Data
				kept = append(kept, img)
			}
		}
		rewritten[i].Images = kept
	}
	return rewritten
}

// hookSessionInput builds the {session} input for session_start.
func hookSessionInput(session string) string {
	b, _ := json.Marshal(struct {
		Session string `json:"session"`
	}{Session: session})
	return string(b)
}

// hookSessionModeInput builds the {session, mode} input for
// before_agent_start.
func hookSessionModeInput(session, mode string) string {
	b, _ := json.Marshal(struct {
		Session string `json:"session"`
		Mode    string `json:"mode"`
	}{Session: session, Mode: mode})
	return string(b)
}

// hookSessionSwitchInput builds the {session, previous} input for
// session_switch.
func hookSessionSwitchInput(session, previous string) string {
	b, _ := json.Marshal(struct {
		Session  string `json:"session"`
		Previous string `json:"previous"`
	}{Session: session, Previous: previous})
	return string(b)
}

// hookCompactInput builds the {session, messages} input for
// session_before_compact: messages is the count, not the list —
// compacting is already expensive, and the hook adds instructions,
// it does not re-read the conversation.
func hookCompactInput(session string, count int) string {
	b, _ := json.Marshal(struct {
		Session  string `json:"session"`
		Messages int    `json:"messages"`
	}{Session: session, Messages: count})
	return string(b)
}

// hookContextInput builds the {session, mode, messages} input for
// the context hook: the turn's starting history, system message
// excluded (Run prepends c.System itself, so the hook never sees
// or changes it).
func hookContextInput(session, mode string, msgs []llm.Message) (string, error) {
	b, err := json.Marshal(struct {
		Session  string        `json:"session"`
		Mode     string        `json:"mode"`
		Messages []llm.Message `json:"messages"`
	}{Session: session, Mode: mode, Messages: msgs})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// hookArgsJSON is the args object a hook sees: the call's args JSON,
// or {} when it is empty or does not parse (the original args still
// go to Dispatch if the call is allowed, where the parse error
// surfaces properly).
func hookArgsJSON(argsJSON string) string {
	if strings.TrimSpace(argsJSON) == "" {
		return "{}"
	}
	var v any
	if json.Unmarshal([]byte(argsJSON), &v) != nil {
		return "{}"
	}
	return argsJSON
}

// hookCallInput builds the tool_call hook input: {tool, args}.
func hookCallInput(tool, argsJSON string) string {
	name, _ := json.Marshal(tool)
	return `{"tool":` + string(name) + `,"args":` + argsJSON + `}`
}

// hookResultInput builds the tool_result hook input:
// {tool, args, result, error} (error is null when the call succeeded).
func hookResultInput(tool, argsJSON, result string, toolErr error) string {
	name, _ := json.Marshal(tool)
	text, _ := json.Marshal(result)
	errJSON := "null"
	if toolErr != nil {
		b, _ := json.Marshal(toolErr.Error())
		errJSON = string(b)
	}
	return `{"tool":` + string(name) + `,"args":` + argsJSON + `,"result":` + string(text) + `,"error":` + errJSON + `}`
}

// interpretToolCallResult reads a tool_call hook's return value:
// (deny, rewrite, bad) — deny and rewrite are mutually exclusive,
// deny wins; bad is a malformed return, which denies too.
func interpretToolCallResult(p *Plugin, res moejs.Value) (deny, rewrite, bad string) {
	g, err := p.rt.ToGo(res)
	if err != nil {
		return "", "", fmt.Sprintf("tool_call hook returned an unreadable value: %v", err)
	}
	switch v := g.(type) {
	case nil:
		return "", "", "" // undefined: allow
	case bool:
		if v {
			return "", "", ""
		}
		return "", "", "tool_call hook returned false"
	case map[string]any:
		if d, ok := v["deny"]; ok {
			s, ok := d.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return "", "", "tool_call hook denied the call without a reason"
			}
			return s, "", ""
		}
		if a, ok := v["args"]; ok {
			m, ok := a.(map[string]any)
			if !ok {
				return "", "", "tool_call hook returned args that is not an object"
			}
			b, err := json.Marshal(m)
			if err != nil {
				return "", "", fmt.Sprintf("tool_call hook returned args that are not JSON: %v", err)
			}
			return "", string(b), ""
		}
		return "", "", ""
	default:
		return "", "", "tool_call hook returned an unexpected value"
	}
}

// hookResultString reads a tool_result hook's return value: (s, true)
// replaces the observation with s; ("", false) keeps the current
// result (undefined, or a malformed value, which is warned about).
func hookResultString(p *Plugin, res moejs.Value) (string, bool) {
	g, err := p.rt.ToGo(res)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: plugin %s tool_result hook returned an unreadable value: %v\n", p.Name, err)
		return "", false
	}
	switch v := g.(type) {
	case nil:
		return "", false // undefined: keep the current result
	case string:
		return v, true
	default:
		fmt.Fprintf(os.Stderr, "warning: plugin %s tool_result hook returned a non-string value; keeping the original result\n", p.Name)
		return "", false
	}
}

func (p *Plugin) call(ctx context.Context, t *Tool, argsJSON string, timeout time.Duration) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broken {
		return "", fmt.Errorf("plugin %s is unusable after a call that never stopped; restart the harness", p.Name)
	}
	if strings.TrimSpace(argsJSON) == "" {
		argsJSON = "{}"
	}
	argVal, err := p.rt.ParseJSONString(argsJSON)
	if err != nil {
		return "", fmt.Errorf("plugin %s tool %s: bad args JSON: %w", p.Name, t.Name, err)
	}
	cr, err := p.runWithTimeout(ctx, timeout, func() callResult {
		res, err := p.rt.CallFunction(t.handler, moejs.Undefined(), argVal)
		res, err = p.unwrapAsync(res, err)
		return callResult{res: res, err: err}
	})
	if err != nil {
		return "", fmt.Errorf("plugin %s tool %s: %w", p.Name, t.Name, err)
	}
	if cr.err != nil {
		return "", fmt.Errorf("plugin %s tool %s: %w", p.Name, t.Name, cr.err)
	}
	return p.resultString(cr.res)
}

// callCommand runs one command handler with the raw args string.
func (p *Plugin) callCommand(ctx context.Context, c *Command, args string, timeout time.Duration) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broken {
		return "", fmt.Errorf("plugin %s is unusable after a call that never stopped; restart the harness", p.Name)
	}
	argVal := moejs.String(args)
	cr, err := p.runWithTimeout(ctx, timeout, func() callResult {
		res, err := p.rt.CallFunction(c.handler, moejs.Undefined(), argVal)
		res, err = p.unwrapAsync(res, err)
		return callResult{res: res, err: err}
	})
	if err != nil {
		return "", fmt.Errorf("plugin %s command %s: %w", p.Name, c.Name, err)
	}
	if cr.err != nil {
		return "", fmt.Errorf("plugin %s command %s: %w", p.Name, c.Name, cr.err)
	}
	return p.resultString(cr.res)
}

// callHook runs one hook handler with the input JSON (already built
// by hookCallInput / hookResultInput) and returns the raw JS value.
func (p *Plugin) callHook(ctx context.Context, fn moejs.Value, inputJSON string, timeout time.Duration) (moejs.Value, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broken {
		return moejs.Value{}, fmt.Errorf("plugin %s is unusable after a call that never stopped; restart the harness", p.Name)
	}
	argVal, err := p.rt.ParseJSONString(inputJSON)
	if err != nil {
		return moejs.Value{}, fmt.Errorf("bad hook input JSON: %w", err)
	}
	cr, err := p.runWithTimeout(ctx, timeout, func() callResult {
		res, err := p.rt.CallFunction(fn, moejs.Undefined(), argVal)
		res, err = p.unwrapAsync(res, err)
		return callResult{res: res, err: err}
	})
	if err != nil {
		return moejs.Value{}, err
	}
	if cr.err != nil {
		return moejs.Value{}, cr.err
	}
	return cr.res, nil
}

// runWithTimeout runs fn (a JS call on this runtime's goroutine) and
// waits for it, the context, or the timeout. The caller holds p.mu.
// A memory-limit or interrupt error resets the runtime
// (ClearInterrupt + ResetAllocation) so the next call does not fail
// with the stale interrupt (spike finding 1).
func (p *Plugin) runWithTimeout(ctx context.Context, timeout time.Duration, run func() callResult) (callResult, error) {
	done := make(chan callResult, 1)
	go func() { done <- run() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case cr := <-done:
		if cr.err != nil {
			var ie *moejs.InterruptedError
			if errors.Is(cr.err, moejs.ErrMemoryLimit) || errors.As(cr.err, &ie) {
				p.rt.ClearInterrupt()
				p.rt.ResetAllocation()
			}
		}
		return cr, nil
	case <-ctx.Done():
		// The turn was aborted (or the harness is shutting down):
		// stop the call the same way a timeout does.
		p.interruptAndWait(done, ctx.Err())
		return callResult{}, ctx.Err()
	case <-timer.C:
		p.interruptAndWait(done, errors.New("plugin call timed out"))
		return callResult{}, fmt.Errorf("timed out after %s", timeout)
	}
}

// unwrapAsync unwraps an async handler's Promise: a promise that is
// still pending when the call returns is an error — Phase 1 has no
// event loop, so an async handler must settle without awaiting
// anything host-driven.
func (p *Plugin) unwrapAsync(res moejs.Value, err error) (moejs.Value, error) {
	if err != nil {
		return res, err
	}
	if state, val, ok := moejs.PromiseResult(res); ok {
		if state != moejs.PromiseFulfilled {
			err = fmt.Errorf("async handler returned a %s promise; async handlers must settle without awaiting", promiseStateName(state))
			if s, convErr := p.resultString(val); convErr == nil && s != "" {
				err = fmt.Errorf("async handler rejected: %s", s)
			}
		} else {
			res = val
		}
	}
	return res, err
}

func promiseStateName(state moejs.PromiseState) string {
	switch state {
	case moejs.PromisePending:
		return "pending"
	case moejs.PromiseFulfilled:
		return "fulfilled"
	case moejs.PromiseRejected:
		return "rejected"
	}
	return "unknown"
}

// interruptAndWait stops a runaway call, then waits for it to
// actually stop. The runtime is only reused once the call has
// returned: a limit hit or an interrupt leaves a pending interrupt
// and spent accounting on the realm (spike finding 1), so both are
// cleared here. If the call never stops, the runtime is wedged and
// the plugin is marked broken instead of hanging every later call.
func (p *Plugin) interruptAndWait(done chan callResult, cause error) {
	p.rt.Interrupt(cause)
	select {
	case <-done:
		p.rt.ClearInterrupt()
		p.rt.ResetAllocation()
	case <-time.After(interruptGrace):
		p.broken = true
	}
}

func (p *Plugin) resultString(v moejs.Value) (string, error) {
	g, err := p.rt.ToGo(v)
	if err != nil {
		return "", err
	}
	var s string
	switch t := g.(type) {
	case nil:
		s = ""
	case string:
		s = t
	default:
		b, err := p.rt.AppendJSON(nil, v)
		if err != nil {
			return "", err
		}
		s = string(b)
	}
	return capObservation(s), nil
}

// capObservation truncates a plugin observation to the display cap
// (the loop truncates tool results for the LLM anyway, but the full
// text would otherwise land in the session file unbounded).
func capObservation(s string) string {
	if len([]rune(s)) <= MaxObservationRunes {
		return s
	}
	return string([]rune(s)[:MaxObservationRunes]) + "\n[… truncated: plugin output continues, first 32 KiB shown]"
}
