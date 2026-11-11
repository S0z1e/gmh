package rpc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installPlanPlugin copies the shipped plan pilot into the test root.
func installPlanPlugin(t *testing.T, root string) {
	t.Helper()
	repo := filepath.Join("..", "..", ".agents", "plugins", "plan")
	src, err := os.ReadFile(filepath.Join(repo, "plan.js"))
	if err != nil {
		t.Skipf("pilot plugin not found (repo layout?): %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(repo, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".agents", "plugins", "plan")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.js"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
}

func toolModeOf(t *testing.T, d *driver) string {
	t.Helper()
	d.send(map[string]any{"id": "s1", "type": "get_state"})
	resp := d.waitFor("response", 5*time.Second)
	data, _ := resp["data"].(map[string]any)
	mode, _ := data["tool_mode"].(string)
	return mode
}

// The command request runs a plugin command and get_state reports
// the tool mode it switched to.
func TestCommandRequestAndGetState(t *testing.T) {
	_, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "Done."}}, func(root string) { installPlanPlugin(t, root) })
	// setupTestApp returns the app too, but only the runner is needed
	// here; the driver holds the app.
	d := startDriver(t, runner.App, runner)
	d.waitFor("ready", 5*time.Second)

	if got := toolModeOf(t, d); got != "default" {
		t.Fatalf("initial tool_mode = %q, want default", got)
	}

	d.send(map[string]any{"id": "c1", "type": "command", "name": "plan"})
	resp := d.waitFor("response", 5*time.Second)
	data, _ := resp["data"].(map[string]any)
	if data["ok"] != true || !strings.Contains(data["output"].(string), "Plan mode") {
		t.Fatalf("command /plan = %v", data)
	}
	if got := toolModeOf(t, d); got != "plan" {
		t.Fatalf("tool_mode after /plan = %q, want plan", got)
	}

	d.send(map[string]any{"id": "c2", "type": "command", "name": "nope"})
	resp = d.waitFor("error", 5*time.Second)
	if resp["id"] != "c2" {
		t.Fatalf("unknown command = %v, want an error for c2", resp)
	}

	d.quit(t)
}

// The "/plan" slash form of a prompt behaves like the command
// request, and a later turn's agent_settled carries the tool mode.
func TestSlashCommandPromptAndSettledMode(t *testing.T) {
	_, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "Done."}}, func(root string) { installPlanPlugin(t, root) })
	d := startDriver(t, runner.App, runner)
	d.waitFor("ready", 5*time.Second)

	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "/plan"})
	resp := d.waitFor("response", 5*time.Second)
	data, _ := resp["data"].(map[string]any)
	if data["ok"] != true || !strings.Contains(data["output"].(string), "Plan mode") {
		t.Fatalf("slash /plan = %v", data)
	}

	d.send(map[string]any{"id": "p2", "type": "prompt", "message": "hello"})
	d.waitFor("response", 5*time.Second)
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["error"] != nil {
		t.Fatalf("settled with error: %v", settled["error"])
	}
	if settled["tool_mode"] != "plan" {
		t.Fatalf("agent_settled tool_mode = %v, want plan", settled["tool_mode"])
	}

	d.send(map[string]any{"id": "p3", "type": "prompt", "message": "/execute"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ = resp["data"].(map[string]any)
	if data["output"] != "Back in default mode." {
		t.Fatalf("slash /execute = %v", data)
	}
	if got := toolModeOf(t, d); got != "default" {
		t.Fatalf("tool_mode after /execute = %q, want default", got)
	}

	d.quit(t)
}
