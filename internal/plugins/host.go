package plugins

import (
	"encoding/json"
	"fmt"
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
		"registerTool": moejs.NativeFunc(p.nativeRegisterTool),
		"log":          moejs.NativeFunc(p.nativeLog),
		"readFile":     moejs.NativeFunc(p.nativeReadFile),
		"writeFile":    moejs.NativeFunc(p.nativeWriteFile),
	})
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
	data, err := os.ReadFile(full)
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
	if err := os.MkdirAll(p.DataDir, 0o755); err != nil {
		return moejs.Undefined(), fmt.Errorf("writeFile %q: %w", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
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
