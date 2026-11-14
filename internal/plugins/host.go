package plugins

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/MoeForge/moejs"
)

// installHostAPI installs the single `harness` global the plugin
// sees. It is installed before the entry loads, so registerTool runs
// at the top level of the entry file.
func (p *Plugin) installHostAPI() error {
	return p.rt.SetGlobal("harness", map[string]any{
		"registerTool":    moejs.NativeFunc(p.nativeRegisterTool),
		"registerCommand": moejs.NativeFunc(p.nativeRegisterCommand),
		"on":              moejs.NativeFunc(p.nativeOn),
		"setMode":         moejs.NativeFunc(p.nativeSetMode),
		"getMode":         moejs.NativeFunc(p.nativeGetMode),
		"log":             moejs.NativeFunc(p.nativeLog),
		"readFile":        moejs.NativeFunc(p.nativeReadFile),
		"writeFile":       moejs.NativeFunc(p.nativeWriteFile),
	})
}

// builtinCommands are the harness's own slash command names (without
// the leading "/"): a plugin may not register one, or the user's
// "/model" would stop switching models.
var builtinCommands = map[string]bool{
	"image": true, "sessions": true, "plugins": true, "resume": true,
	"rename": true, "model": true, "quit": true, "exit": true,
	"usage": true, "jobs": true, "clear": true, "compact": true,
	"skill": true,
}

// nativeRegisterCommand implements
// harness.registerCommand(name, description, handler). The handler
// takes the raw args string (the text after "/name", unparsed) and
// returns the output text. Registration is only valid while the
// entry's top level runs, like registerTool: the command list is
// snapshotted right after load. A registration error throws, which
// fails the plugin's load.
func (p *Plugin) nativeRegisterCommand(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	if !p.loading {
		return moejs.Undefined(), fmt.Errorf("registerCommand must be called at the top level of the plugin entry, during load")
	}
	name, err := toGoString(r, moejs.Arg(args, 0))
	if err != nil {
		return moejs.Undefined(), err
	}
	if !validToolName.MatchString(name) {
		return moejs.Undefined(), fmt.Errorf("invalid command name %q: use lowercase letters, digits and underscores, starting with a letter", name)
	}
	if builtinCommands[name] {
		return moejs.Undefined(), fmt.Errorf("command %q collides with a built-in command", name)
	}
	if _, ok := p.commandsByName[name]; ok {
		return moejs.Undefined(), fmt.Errorf("command %q is already registered by this plugin", name)
	}
	desc, err := toGoString(r, moejs.Arg(args, 1))
	if err != nil {
		return moejs.Undefined(), err
	}
	if strings.TrimSpace(desc) == "" {
		return moejs.Undefined(), fmt.Errorf("command %q needs a description", name)
	}
	handler := moejs.Arg(args, 2)
	if !handler.IsObject() {
		return moejs.Undefined(), fmt.Errorf("command %q handler must be a function", name)
	}
	c := &Command{Name: name, Description: desc, handler: handler, plugin: p}
	p.commands = append(p.commands, c)
	p.commandsByName[name] = c
	return moejs.Undefined(), nil
}

// nativeOn implements harness.on(event, handler), the lifecycle
// hooks: "tool_call" (before a tool runs), "tool_result" (after it
// returns), "before_agent_start" (per-turn context injection),
// "context" (rewrite the turn's starting messages),
// "session_before_compact" (extra instructions for the summary),
// "session_start" and "session_switch" (notifications when a session
// becomes current), and "stop" (turn-end gate: return a non-empty
// string to veto the turn ending, e.g. a failed check). At most one
// handler per event per plugin. Like registerTool, only valid while
// the entry's top level runs.
func (p *Plugin) nativeOn(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	if !p.loading {
		return moejs.Undefined(), fmt.Errorf("harness.on must be called at the top level of the plugin entry, during load")
	}
	event, err := toGoString(r, moejs.Arg(args, 0))
	if err != nil {
		return moejs.Undefined(), err
	}
	handler := moejs.Arg(args, 1)
	if !handler.IsObject() {
		return moejs.Undefined(), fmt.Errorf("harness.on(%q) handler must be a function", event)
	}
	switch event {
	case "tool_call":
		if p.hasToolCallHook {
			return moejs.Undefined(), fmt.Errorf("plugin %s already has a tool_call handler", p.Name)
		}
		p.toolCallHook = handler
		p.hasToolCallHook = true
	case "tool_result":
		if p.hasToolResultHook {
			return moejs.Undefined(), fmt.Errorf("plugin %s already has a tool_result handler", p.Name)
		}
		p.toolResultHook = handler
		p.hasToolResultHook = true
	case "before_agent_start":
		if p.hasBeforeAgentStartHook {
			return moejs.Undefined(), fmt.Errorf("plugin %s already has a before_agent_start handler", p.Name)
		}
		p.beforeAgentStartHook = handler
		p.hasBeforeAgentStartHook = true
	case "context":
		if p.hasContextHook {
			return moejs.Undefined(), fmt.Errorf("plugin %s already has a context handler", p.Name)
		}
		p.contextHook = handler
		p.hasContextHook = true
	case "session_before_compact":
		if p.hasSessionBeforeCompactHook {
			return moejs.Undefined(), fmt.Errorf("plugin %s already has a session_before_compact handler", p.Name)
		}
		p.sessionBeforeCompactHook = handler
		p.hasSessionBeforeCompactHook = true
	case "session_start":
		if p.hasSessionStartHook {
			return moejs.Undefined(), fmt.Errorf("plugin %s already has a session_start handler", p.Name)
		}
		p.sessionStartHook = handler
		p.hasSessionStartHook = true
	case "session_switch":
		if p.hasSessionSwitchHook {
			return moejs.Undefined(), fmt.Errorf("plugin %s already has a session_switch handler", p.Name)
		}
		p.sessionSwitchHook = handler
		p.hasSessionSwitchHook = true
	case "stop":
		if p.hasStopHook {
			return moejs.Undefined(), fmt.Errorf("plugin %s already has a stop handler", p.Name)
		}
		p.stopHook = handler
		p.hasStopHook = true
	default:
		return moejs.Undefined(), fmt.Errorf("unknown hook event %q (want tool_call, tool_result, before_agent_start, context, session_before_compact, session_start, session_switch or stop)", event)
	}
	return moejs.Undefined(), nil
}

// registryModeSetter / registryModeGetter read the mode callbacks
// off the owning registry (nil registry = a plugin built outside
// Load, which never happens in production).
func (p *Plugin) registryModeSetter() func(string) error {
	if p.registry == nil {
		return nil
	}
	return p.registry.ModeSetter
}

func (p *Plugin) registryModeGetter() func() string {
	if p.registry == nil {
		return nil
	}
	return p.registry.ModeGetter
}

// nativeSetMode implements harness.setMode(name): switch the agent's
// tool mode (core-defined: "default" or "plan"). The mode lives on
// the App, so this calls the registry's ModeSetter, wired by
// app.Setup; an unknown mode is a throw, which the command handler
// reports as the command's error.
func (p *Plugin) nativeSetMode(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	name, err := toGoString(r, moejs.Arg(args, 0))
	if err != nil {
		return moejs.Undefined(), err
	}
	setter := p.registryModeSetter()
	if setter == nil {
		return moejs.Undefined(), fmt.Errorf("mode switching is not available in this run")
	}
	if err := setter(name); err != nil {
		return moejs.Undefined(), err
	}
	return moejs.Undefined(), nil
}

// nativeGetMode implements harness.getMode(): the current tool mode
// ("default" when no setter is wired, e.g. a standalone registry in
// tests).
func (p *Plugin) nativeGetMode(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	getter := p.registryModeGetter()
	if getter == nil {
		return moejs.String("default"), nil
	}
	return moejs.String(getter()), nil
}

// nativeRegisterTool implements
// harness.registerTool(name, description, parameters, handler).
// parameters is a JSON schema object for the tool's arguments;
// handler is a JS function taking the args object and returning the
// observation (a string as-is, anything else as JSON). Registration
// is only valid while the entry's top level runs: the tools are
// snapshotted into the agent's ToolDefs right after load, so a tool
// registered later would never be offered to the model. A
// registration error throws, which fails the plugin's load with a
// clear message instead of a half-registered plugin.
func (p *Plugin) nativeRegisterTool(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	if !p.loading {
		return moejs.Undefined(), fmt.Errorf("registerTool must be called at the top level of the plugin entry, during load")
	}
	name, err := toGoString(r, moejs.Arg(args, 0))
	if err != nil {
		return moejs.Undefined(), err
	}
	if !validToolName.MatchString(name) {
		return moejs.Undefined(), fmt.Errorf("invalid tool name %q: use lowercase letters, digits and underscores, starting with a letter", name)
	}
	if builtinTools[name] {
		return moejs.Undefined(), fmt.Errorf("tool %q collides with a built-in tool", name)
	}
	if _, ok := p.byName[name]; ok {
		return moejs.Undefined(), fmt.Errorf("tool %q is already registered by this plugin", name)
	}
	desc, err := toGoString(r, moejs.Arg(args, 1))
	if err != nil {
		return moejs.Undefined(), err
	}
	if strings.TrimSpace(desc) == "" {
		return moejs.Undefined(), fmt.Errorf("tool %q needs a description (the model reads it)", name)
	}
	paramsGo := r.ToGo(moejs.Arg(args, 2))
	paramsMap, ok := paramsGo.(map[string]any)
	if !ok {
		return moejs.Undefined(), fmt.Errorf("tool %q parameters must be a JSON schema object", name)
	}
	if t, _ := paramsMap["type"].(string); t == "" {
		// The model needs a valid schema: an absent type is the
		// common authoring slip, so default it rather than fail.
		paramsMap["type"] = "object"
	} else if t != "object" {
		return moejs.Undefined(), fmt.Errorf("tool %q parameters type must be \"object\", got %q", name, t)
	}
	paramsJSON, err := json.Marshal(paramsMap)
	if err != nil {
		return moejs.Undefined(), fmt.Errorf("tool %q parameters are not JSON: %w", name, err)
	}
	handler := moejs.Arg(args, 3)
	if !handler.IsObject() {
		return moejs.Undefined(), fmt.Errorf("tool %q handler must be a function", name)
	}
	t := &Tool{Name: name, Description: desc, Parameters: json.RawMessage(paramsJSON), handler: handler, plugin: p}
	p.tools = append(p.tools, t)
	p.byName[name] = t
	return moejs.Undefined(), nil
}

// nativeLog implements harness.log(...args): one line on stderr, so
// plugin output never pollutes stdout (the RPC protocol owns it).
func (p *Plugin) nativeLog(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = jsValueString(r, a)
	}
	fmt.Fprintf(os.Stderr, "[plugin %s] %s\n", p.Name, strings.Join(parts, " "))
	return moejs.Undefined(), nil
}

// resolveStorePath jails a store path to the plugin's data dir: the
// path must be relative, and the resolved path must stay inside the
// data dir (the same jail pattern as tools.resolve).
func (p *Plugin) resolveStorePath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be relative to the plugin data dir: %s", path)
	}
	clean := filepath.Join(p.DataDir, filepath.Clean(path))
	rel, err := filepath.Rel(p.DataDir, clean)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path outside the plugin data dir: %s", path)
	}
	return clean, nil
}

// nativeReadFile implements harness.readFile(path): the file's text.
// A missing file is an error the plugin can catch (a todo plugin
// reads a state file that does not exist yet).
func (p *Plugin) nativeReadFile(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	path, err := toGoString(r, moejs.Arg(args, 0))
	if err != nil {
		return moejs.Undefined(), err
	}
	full, err := p.resolveStorePath(path)
	if err != nil {
		return moejs.Undefined(), err
	}
	store, err := p.openStore(false)
	if err != nil {
		return moejs.Undefined(), err
	}
	defer store.Close()
	rel, _ := filepath.Rel(p.DataDir, full)
	f, err := store.Open(rel)
	if err != nil {
		return moejs.Undefined(), fmt.Errorf("readFile %q: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxStoreBytes+1))
	if err != nil {
		return moejs.Undefined(), fmt.Errorf("readFile %q: %w", path, err)
	}
	if len(data) > MaxStoreBytes {
		return moejs.Undefined(), fmt.Errorf("readFile %q: file too large (%d bytes, max %d)", path, len(data), MaxStoreBytes)
	}
	return moejs.String(string(data)), nil
}

// nativeWriteFile implements harness.writeFile(path, content). The
// data dir is created on first write.
func (p *Plugin) nativeWriteFile(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	path, err := toGoString(r, moejs.Arg(args, 0))
	if err != nil {
		return moejs.Undefined(), err
	}
	content, err := toGoString(r, moejs.Arg(args, 1))
	if err != nil {
		return moejs.Undefined(), err
	}
	if len(content) > MaxStoreBytes {
		return moejs.Undefined(), fmt.Errorf("writeFile %q: content too large (%d bytes, max %d)", path, len(content), MaxStoreBytes)
	}
	full, err := p.resolveStorePath(path)
	if err != nil {
		return moejs.Undefined(), err
	}
	store, err := p.openStore(true)
	if err != nil {
		return moejs.Undefined(), fmt.Errorf("writeFile %q: %w", path, err)
	}
	defer store.Close()
	rel, _ := filepath.Rel(p.DataDir, full)
	if err := store.WriteFile(rel, []byte(content), 0o644); err != nil {
		return moejs.Undefined(), fmt.Errorf("writeFile %q: %w", path, err)
	}
	return moejs.Undefined(), nil
}

// toGoString coerces a JS value to a Go string (JS String()
// semantics: numbers and booleans stringify, objects become
// "[object Object]").
func toGoString(r *moejs.Realm, v moejs.Value) (string, error) {
	s, err := r.ToString(v)
	if err != nil {
		return "", err
	}
	return s.GoString(), nil
}

// jsValueString renders a JS value for log(): strings as-is, null /
// undefined as their names, everything else as compact JSON.
func jsValueString(r *moejs.Realm, v moejs.Value) string {
	g := r.ToGo(v)
	switch t := g.(type) {
	case nil:
		return "undefined"
	case string:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}

func (p *Plugin) openStore(create bool) (*os.Root, error) {
	root, err := os.OpenRoot(p.Dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if create {
		if err := root.MkdirAll("data", 0755); err != nil {
			return nil, err
		}
	}
	return root.OpenRoot("data")
}
