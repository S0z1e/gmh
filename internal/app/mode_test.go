package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/agent"
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

func setupWithPlan(t *testing.T) (*App, *Runner, *[]string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := t.TempDir()
	writeMockProvider(t, root)
	installPlanPlugin(t, root)
	a, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	return a, r, &printed
}

func toolNames(tools []string) map[string]bool {
	names := map[string]bool{}
	for _, n := range tools {
		names[n] = true
	}
	return names
}

func TestRunnerPlanModeSwitchesTools(t *testing.T) {
	a, r, printed := setupWithPlan(t)
	defer a.Store.Close()
	ctx := context.Background()

	names := func() map[string]bool {
		var ns []string
		for _, d := range a.Cfg.Tools {
			ns = append(ns, d.Function.Name)
		}
		return toolNames(ns)
	}
	if !names()["write"] || !names()["edit"] || !names()["bash"] || !names()["plan_create"] {
		t.Fatalf("default tools = %v, want write/edit/bash + plan_create", names())
	}
	if a.ToolMode() != agent.ToolModeDefault {
		t.Fatalf("initial tool mode = %q", a.ToolMode())
	}

	if quit := r.Handle(ctx, "/plan"); quit {
		t.Fatal("/plan should not quit")
	}
	if !strings.Contains(strings.Join(*printed, "\n"), "Plan mode: read-only") {
		t.Fatalf("printed = %v, want the plan banner", *printed)
	}
	if a.ToolMode() != agent.ToolModePlan {
		t.Fatalf("tool mode after /plan = %q", a.ToolMode())
	}
	// Plan mode drops the mutating built-ins but keeps plugin tools.
	if names()["write"] || names()["edit"] || names()["bash"] {
		t.Fatalf("plan tools = %v, want no write/edit/bash", names())
	}
	if !names()["plan_create"] || !names()["read"] {
		t.Fatalf("plan tools = %v, want plan_create + read", names())
	}

	if quit := r.Handle(ctx, "/plan"); quit {
		t.Fatal("/plan again should not quit")
	}
	if !strings.Contains(strings.Join(*printed, "\n"), "Already in plan mode.") {
		t.Fatalf("printed = %v, want the already-in-plan line", *printed)
	}

	if quit := r.Handle(ctx, "/execute"); quit {
		t.Fatal("/execute should not quit")
	}
	if a.ToolMode() != agent.ToolModeDefault {
		t.Fatalf("tool mode after /execute = %q", a.ToolMode())
	}
	if !names()["write"] || !names()["bash"] {
		t.Fatalf("default tools after /execute = %v, want write + bash back", names())
	}
	if quit := r.Handle(ctx, "/execute"); quit {
		t.Fatal("/execute again should not quit")
	}
	if !strings.Contains(strings.Join(*printed, "\n"), "Already in default mode.") {
		t.Fatalf("printed = %v, want the already-in-default line", *printed)
	}
}

func TestSetToolModeUnknown(t *testing.T) {
	a, _, _ := setupWithPlan(t)
	defer a.Store.Close()
	if err := a.SetToolMode(agent.ToolMode("bogus")); err == nil {
		t.Fatal("unknown mode accepted without an error")
	}
	if a.ToolMode() != agent.ToolModeDefault {
		t.Fatalf("tool mode = %q after a rejected switch", a.ToolMode())
	}
}

func TestNewSessionResetsToolMode(t *testing.T) {
	a, r, _ := setupWithPlan(t)
	defer a.Store.Close()
	ctx := context.Background()
	if quit := r.Handle(ctx, "/plan"); quit {
		t.Fatal("/plan should not quit")
	}
	if a.ToolMode() != agent.ToolModePlan {
		t.Fatalf("tool mode = %q", a.ToolMode())
	}
	if _, err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if a.ToolMode() != agent.ToolModeDefault {
		t.Fatalf("tool mode after NewSession = %q, want default", a.ToolMode())
	}
	var write bool
	for _, d := range a.Cfg.Tools {
		if d.Function.Name == "write" {
			write = true
		}
	}
	if !write {
		t.Fatal("write not back in Tools after NewSession")
	}
}

func TestRunnerModeSwitchInjectsReminder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := t.TempDir()
	writeMockProvider(t, root)
	installPlanPlugin(t, root)
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi there\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	a, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	ctx := context.Background()
	r.Handle(ctx, "/plan")
	if quit := r.Handle(ctx, "say hi"); quit {
		t.Fatal("task should not quit")
	}
	if len(bodies) != 1 {
		t.Fatalf("got %d model requests, want 1", len(bodies))
	}
	if !strings.Contains(bodies[0], agent.PlanModeReminder) {
		t.Fatal("the plan reminder was not injected into the turn's messages")
	}
	// The reminder is one-turn only: the next turn has no reminder.
	if quit := r.Handle(ctx, "again"); quit {
		t.Fatal("task should not quit")
	}
	if len(bodies) != 2 {
		t.Fatalf("got %d model requests, want 2", len(bodies))
	}
	if strings.Contains(bodies[1], agent.PlanModeReminder) {
		t.Fatal("the plan reminder leaked into the following turn")
	}
}

func TestRunnerUnknownCommandFallsThrough(t *testing.T) {
	srv := mockServer(t)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	root := t.TempDir()
	writeMockProvider(t, root)
	installPlanPlugin(t, root)
	a, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	// An unknown slash line is a task for the model, not an error:
	// the mock server answers it and history grows.
	if quit := r.Handle(context.Background(), "/nosuchthing"); quit {
		t.Fatal("unknown command should not quit")
	}
	if len(a.History) != 2 {
		t.Fatalf("history = %d messages, want 2 (the line reached the model)", len(a.History))
	}
	if a.History[1].Content != "hi there" {
		t.Fatalf("assistant message = %q, want the model's answer", a.History[1].Content)
	}
}

func TestParsePluginCommand(t *testing.T) {
	name, args, ok := ParsePluginCommand("/plan")
	if !ok || name != "plan" || args != "" {
		t.Fatalf("parse /plan = %q %q %v", name, args, ok)
	}
	name, args, ok = ParsePluginCommand("/execute  now please")
	if !ok || name != "execute" || args != "now please" {
		t.Fatalf("parse /execute = %q %q %v", name, args, ok)
	}
	if _, _, ok := ParsePluginCommand("not a command"); ok {
		t.Fatal("a plain line parsed as a command")
	}
	if _, _, ok := ParsePluginCommand("/"); ok {
		t.Fatal("a bare slash parsed as a command")
	}
}
