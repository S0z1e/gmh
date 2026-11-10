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
// load is skipped with a warning; it never fails Setup.
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
	tools   []*Tool
	byName  map[string]*Tool
}

// Registry is the loaded plugins of one run, indexed by tool name.
type Registry struct {
	plugins []*Plugin
	byTool  map[string]*Tool
	// CallTimeout bounds one plugin tool call (0 = DefaultCallTimeout).
	CallTimeout time.Duration
}

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
func Load(root string) (*Registry, []error) {
	reg := &Registry{byTool: map[string]*Tool{}}
	if v := os.Getenv("HARNESS_PLUGINS"); v == "0" || strings.EqualFold(v, "false") {
		return reg, nil
	}
	dir := filepath.Join(root, ".agents", "plugins")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return reg, []error{fmt.Errorf("read plugins dir %s: %w", dir, err)}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var errs []error
	for _, en := range entries {
		if !en.IsDir() {
			continue
		}
		p, err := loadOne(filepath.Join(dir, en.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin %s: %w", en.Name(), err))
			continue
		}
		for _, t := range p.tools {
			if _, ok := reg.byTool[t.Name]; ok {
				errs = append(errs, fmt.Errorf("plugin %s: tool %q is already registered by another plugin, skipped", p.Name, t.Name))
				continue
			}
			reg.byTool[t.Name] = t
		}
		reg.plugins = append(reg.plugins, p)
	}
	return reg, errs
}

func loadOne(dir string) (*Plugin, error) {
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
		byName:  map[string]*Tool{},
		loading: true,
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
	timeout := r.CallTimeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	return t.plugin.call(ctx, t, argsJSON, timeout)
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
	done := make(chan callResult, 1)
	go func() {
		res, err := p.rt.CallFunction(t.handler, moejs.Undefined(), argVal)
		if err == nil {
			// An async handler returns a Promise: unwrap it. The
			// jobs that settle it run before CallFunction returns,
			// so a handler that only awaits settled values is
			// already fulfilled here.
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
		}
		done <- callResult{res: res, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case cr := <-done:
		return p.finishCall(t, cr)
	case <-ctx.Done():
		// The turn was aborted (or the harness is shutting down):
		// stop the call the same way a timeout does.
		p.interruptAndWait(done, ctx.Err())
		return "", fmt.Errorf("plugin %s tool %s: %w", p.Name, t.Name, ctx.Err())
	case <-timer.C:
		p.interruptAndWait(done, errors.New("plugin tool call timed out"))
		return "", fmt.Errorf("plugin %s tool %s: timed out after %s", p.Name, t.Name, timeout)
	}
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

// finishCall converts a finished call into the observation, or the
// error. A memory-limit or interrupt error also resets the runtime
// (ClearInterrupt + ResetAllocation) so the next call does not fail
// with the stale interrupt.
func (p *Plugin) finishCall(t *Tool, cr callResult) (string, error) {
	if cr.err != nil {
		var ie *moejs.InterruptedError
		if errors.Is(cr.err, moejs.ErrMemoryLimit) || errors.As(cr.err, &ie) {
			p.rt.ClearInterrupt()
			p.rt.ResetAllocation()
		}
		return "", fmt.Errorf("plugin %s tool %s: %w", p.Name, t.Name, cr.err)
	}
	return p.resultString(cr.res)
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
