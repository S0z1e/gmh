package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/agent"
	"minimalharness/internal/tools"
)

func mockServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi there\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestSetupResolvesConfigFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := t.TempDir()
	cfg := `{
		"defaultProvider": "discovery",
		"providers": {
			"openconsole": {"baseUrl": "https://www.openconsole.cn/v1", "api": "chat", "model": "deepseek-v4.1-flash"},
			"discovery": {"baseUrl": "https://discovery-api.intern-ai.org.cn/v1", "api": "anthropic", "model": "Agents-A1"}
		},
		"mode": "auto",
		"contextLimit": 12345,
		"tuiTheme": "terminal"
	}`
	if err := os.WriteFile(filepath.Join(root, ".harness.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Setup(Params{Root: root, MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	if a.Cfg.Client.BaseURL != "https://discovery-api.intern-ai.org.cn/v1" {
		t.Fatalf("baseURL = %q", a.Cfg.Client.BaseURL)
	}
	if a.Cfg.Client.Model != "Agents-A1" || a.Cfg.Client.API != "anthropic" {
		t.Fatalf("client = api %q model %q", a.Cfg.Client.API, a.Cfg.Client.Model)
	}
	if a.Cfg.Mode != "auto" {
		t.Fatalf("mode = %q", a.Cfg.Mode)
	}
	if a.ContextLimit != 12345 {
		t.Fatalf("contextLimit = %d", a.ContextLimit)
	}
	if a.TUITheme != "terminal" {
		t.Fatalf("tuiTheme = %q", a.TUITheme)
	}
	// -provider picks another entry; env still wins over the file.
	a2, err := Setup(Params{Root: root, MaxSteps: 5, Provider: "openconsole"})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Store.Close()
	if a2.Cfg.Client.Model != "deepseek-v4.1-flash" || a2.Cfg.Client.API != "chat" {
		t.Fatalf("provider openconsole = api %q model %q", a2.Cfg.Client.API, a2.Cfg.Client.Model)
	}
	t.Setenv("OPENAI_MODEL", "env-model")
	a3, err := Setup(Params{Root: root, MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a3.Store.Close()
	if a3.Cfg.Client.Model != "env-model" {
		t.Fatalf("env should win over config, model = %q", a3.Cfg.Client.Model)
	}
	// Unknown provider is a setup error.
	if _, err := Setup(Params{Root: root, MaxSteps: 5, Provider: "ghost"}); err == nil {
		t.Fatal("unknown provider should fail setup")
	}
}

// The Pi-shaped models array: per-model api/contextWindow/maxTokens,
// apiKey "$NAME" resolution, headers, and the Pi defaults when a
// model omits a value.
func TestSetupResolvesModelsArray(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "fallback-key")
	t.Setenv("DISC_TEST_KEY", "disc-key")
	t.Setenv("MODEL_HDR", "model-hdr")
	root := t.TempDir()
	cfg := `{
		"defaultProvider": "discovery",
		"defaultModel": "Agents-A1",
		"providers": {
			"discovery": {
				"baseUrl": "https://discovery-api.intern-ai.org.cn/v1",
				"api": "anthropic-messages",
				"apiKey": "$DISC_TEST_KEY",
				"headers": {"X-Prov": "d1"},
				"models": [
					{"id": "Agents-A1", "reasoning": true, "contextWindow": 1000000, "maxTokens": 16384, "samplingParams": {"temperature": 0.7}, "headers": {"X-Model": "$MODEL_HDR"}},
					{"id": "deepseek-v4-flash-0731", "api": "openai-completions", "contextWindow": 256000, "maxTokens": 8192}
				]
			},
			"openconsole": {"baseUrl": "https://www.openconsole.cn/v1", "api": "chat", "model": "deepseek-v4.1-flash"}
		}
	}`
	if err := os.WriteFile(filepath.Join(root, ".harness.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	// Default: the defaultModel entry, Pi spelling normalised.
	a, err := Setup(Params{Root: root, MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	c := a.Cfg.Client
	if c.BaseURL != "https://discovery-api.intern-ai.org.cn/v1" || c.Model != "Agents-A1" || c.API != "anthropic" {
		t.Fatalf("client = base %q api %q model %q", c.BaseURL, c.API, c.Model)
	}
	if c.APIKey != "disc-key" {
		t.Fatalf("apiKey = %q, want the $DISC_TEST_KEY value", c.APIKey)
	}
	if a.ContextLimit != 1000000 {
		t.Fatalf("contextLimit = %d, want the model contextWindow", a.ContextLimit)
	}
	if c.MaxTokens != 16384 {
		t.Fatalf("maxTokens = %d, want 16384", c.MaxTokens)
	}
	if c.Headers["X-Prov"] != "d1" || c.Headers["X-Model"] != "model-hdr" {
		t.Fatalf("headers = %v", c.Headers)
	}
	if c.SamplingParams["temperature"] != 0.7 {
		t.Fatalf("samplingParams = %v", c.SamplingParams)
	}
	if !a.Model.Reasoning || len(a.Model.Input) != 1 || a.Model.Input[0] != "text" {
		t.Fatalf("resolved model = %+v", a.Model)
	}
	// Second model on the same provider overrides the api.
	a2, err := Setup(Params{Root: root, MaxSteps: 5, Model: "deepseek-v4-flash-0731"})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Store.Close()
	if a2.Cfg.Client.API != "chat" || a2.ContextLimit != 256000 || a2.Cfg.Client.MaxTokens != 8192 {
		t.Fatalf("second model: api %q contextLimit %d maxTokens %d", a2.Cfg.Client.API, a2.ContextLimit, a2.Cfg.Client.MaxTokens)
	}
	// A provider without an apiKey ref falls back to OPENAI_API_KEY,
	// and a model that omits limits gets the Pi defaults.
	a3, err := Setup(Params{Root: root, MaxSteps: 5, Provider: "openconsole"})
	if err != nil {
		t.Fatal(err)
	}
	defer a3.Store.Close()
	if a3.Cfg.Client.APIKey != "fallback-key" || a3.Cfg.Client.Model != "deepseek-v4.1-flash" {
		t.Fatalf("openconsole: key %q model %q", a3.Cfg.Client.APIKey, a3.Cfg.Client.Model)
	}
	if a3.ContextLimit != 128000 || a3.Cfg.Client.MaxTokens != 16384 {
		t.Fatalf("Pi defaults: contextLimit %d maxTokens %d", a3.ContextLimit, a3.Cfg.Client.MaxTokens)
	}
	// Unknown model id on a provider with a list is a setup error.
	if _, err := Setup(Params{Root: root, MaxSteps: 5, Model: "ghost"}); err == nil {
		t.Fatal("unknown model should fail setup")
	}
	// No config at all is a setup error now: providers must be
	// configured by the user, there is no built-in default.
	empty := t.TempDir()
	if _, err := Setup(Params{Root: empty, MaxSteps: 5}); err == nil || !strings.Contains(err.Error(), "no providers configured") {
		t.Fatalf("no-config setup err = %v, want no-providers-configured", err)
	}
}

// writeMockProvider gives a test root a configured provider, now that
// Setup requires one. OPENAI_BASE_URL still wins for the actual calls.
func writeMockProvider(t *testing.T, root string) {
	t.Helper()
	cfg := `{"providers": {"mock": {"baseUrl": "http://127.0.0.1:1", "api": "chat", "model": "mock"}}}`
	if err := os.WriteFile(filepath.Join(root, ".harness.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerHandleTaskAndUsage(t *testing.T) {
	srv := mockServer(t)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	root := t.TempDir()
	writeMockProvider(t, root)
	a, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	var events []agent.Event
	a.Cfg.OnEvent = func(e agent.Event) { events = append(events, e) }
	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	if quit := r.Handle(context.Background(), "say hi"); quit {
		t.Fatal("task should not quit")
	}
	if len(a.History) != 2 { // user + assistant
		t.Fatalf("history = %d messages, want 2", len(a.History))
	}
	if r.SessionUsage.TotalTokens != 15 {
		t.Fatalf("session usage total = %d, want 15", r.SessionUsage.TotalTokens)
	}
	joined := strings.Join(printed, "\n")
	if !strings.Contains(joined, "[usage] turn: prompt=10") {
		t.Fatalf("turn usage not printed: %v", printed)
	}
	if quit := r.Handle(context.Background(), "/usage"); quit {
		t.Fatal("/usage should not quit")
	}
	if !strings.Contains(strings.Join(printed, "\n"), "[usage] session: prompt=10") {
		t.Fatalf("session usage not printed: %v", printed)
	}
}

func TestRunnerHandleClear(t *testing.T) {
	srv := mockServer(t)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	root := t.TempDir()
	writeMockProvider(t, root)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	r := &Runner{App: a, Print: func(format string, args ...any) {}}
	r.Handle(context.Background(), "say hi")
	if len(a.History) == 0 {
		t.Fatal("history should have messages before clear")
	}
	if quit := r.Handle(context.Background(), "/clear"); quit {
		t.Fatal("/clear should not quit")
	}
	if len(a.History) != 0 {
		t.Fatalf("history after /clear = %d, want 0", len(a.History))
	}
}

// The keyword trigger: a task naming a skill loads its body into the
// turn before the model sees the task.
func TestRunnerKeywordTriggerLoadsSkill(t *testing.T) {
	srv := mockServer(t)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	root := t.TempDir()
	writeMockProvider(t, root)
	skillDir := filepath.Join(root, ".agents", "skills", "greeter")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: greeter\ndescription: Greets people.\n---\n# Greeter\nSay hello warmly.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	if len(a.Skills) != 1 || a.Skills[0].Name != "greeter" {
		t.Fatalf("skills = %v, want [greeter]", a.Skills)
	}
	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	r.Handle(context.Background(), "use the greeter skill to say hi")
	if len(a.History) < 3 {
		t.Fatalf("history = %d messages, want >= 3 (skill + task + assistant)", len(a.History))
	}
	if !strings.Contains(a.History[0].Content, "Say hello warmly.") {
		t.Fatalf("first history message = %q, want the skill body", a.History[0].Content)
	}
	if !strings.Contains(strings.Join(printed, "\n"), "Auto-loaded skill greeter") {
		t.Fatalf("no auto-load notice printed: %v", printed)
	}
	// A task that does not name the skill loads nothing.
	a2, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5, SessionID: "other-session"})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Store.Close()
	r2 := &Runner{App: a2, Print: func(format string, args ...any) {}}
	r2.Handle(context.Background(), "just do something unrelated")
	if len(a2.History) != 2 { // task + assistant only
		t.Fatalf("history = %d messages, want 2 (no skill loaded)", len(a2.History))
	}
}

// Cost estimates appear only when rates are configured, labelled with
// the rate source, and the session cost accumulates across turns.
func TestRunnerCostEstimate(t *testing.T) {
	srv := mockServer(t)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("OPENAI_PRICE_INPUT_PER_MTOK", "1000")
	t.Setenv("OPENAI_PRICE_OUTPUT_PER_MTOK", "2000")
	t.Setenv("OPENAI_PRICE_SOURCE", "test rates")
	root := t.TempDir()
	writeMockProvider(t, root)
	a, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	if !a.Rates.Configured {
		t.Fatal("rates should be configured from env")
	}
	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	r.Handle(context.Background(), "say hi")
	// Mock usage is prompt 10 / completion 5: (10*1000 + 5*2000)/1e6 = $0.02.
	joined := strings.Join(printed, "\n")
	if !strings.Contains(joined, "est. cost: ~$0.0200") {
		t.Fatalf("turn cost not printed: %v", printed)
	}
	if !strings.Contains(joined, "rates: test rates") {
		t.Fatalf("rate source not labelled: %v", printed)
	}
	if !strings.Contains(joined, "實價未驗證") {
		t.Fatalf("unverified label missing: %v", printed)
	}
	if r.SessionCost != 0.02 {
		t.Fatalf("session cost = %v, want 0.02", r.SessionCost)
	}
	r.Handle(context.Background(), "say hi again")
	if r.SessionCost != 0.04 {
		t.Fatalf("session cost after 2 turns = %v, want 0.04", r.SessionCost)
	}
}

// Without rates, no estimate appears — only the unverified label.
func TestRunnerNoCostWithoutRates(t *testing.T) {
	srv := mockServer(t)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	root := t.TempDir()
	writeMockProvider(t, root)
	a, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	r.Handle(context.Background(), "say hi")
	joined := strings.Join(printed, "\n")
	if strings.Contains(joined, "est. cost") {
		t.Fatalf("no estimate should appear without rates: %v", printed)
	}
	// The label names the provider from the base URL: the mock server
	// is not OpenConsole, so it shows its own host. The mapping
	// itself is covered by TestAnthropicEndpointNormalisation.
	if !strings.Contains(joined, "fee: 127.0.0.1") || !strings.Contains(joined, "實價未驗證") {
		t.Fatalf("unverified label missing: %v", printed)
	}
}

// /jobs lists background jobs: none, then running, then done with exit code.
func TestRunnerJobsCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := t.TempDir()
	writeMockProvider(t, root)
	a, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	if quit := r.Handle(context.Background(), "/jobs"); quit {
		t.Fatal("/jobs should not quit")
	}
	if !strings.Contains(strings.Join(printed, "\n"), "No background jobs.") {
		t.Fatalf("/jobs with no jobs printed: %v", printed)
	}
	job, err := a.Cfg.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "sleep 2; echo done"})
	if err != nil {
		t.Fatal(err)
	}
	printed = nil
	if quit := r.Handle(context.Background(), "/jobs"); quit {
		t.Fatal("/jobs should not quit")
	}
	joined := strings.Join(printed, "\n")
	if !strings.Contains(joined, "bg-1") || !strings.Contains(joined, "running") || !strings.Contains(joined, "sleep 2") {
		t.Fatalf("/jobs should list the running job: %v", printed)
	}
	<-job.Done()
	printed = nil
	if quit := r.Handle(context.Background(), "/jobs"); quit {
		t.Fatal("/jobs should not quit")
	}
	joined = strings.Join(printed, "\n")
	if !strings.Contains(joined, "bg-1") || !strings.Contains(joined, "done") || !strings.Contains(joined, "exit 0") {
		t.Fatalf("/jobs should list the finished job with exit 0: %v", printed)
	}
}

func TestRunnerHandleQuit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := t.TempDir()
	writeMockProvider(t, root)
	a, err := Setup(Params{Root: root, Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	r := &Runner{App: a, Print: func(format string, args ...any) {}}
	if !r.Handle(context.Background(), "/quit") {
		t.Fatal("/quit should return true")
	}
	if r.Handle(context.Background(), "hello") {
		t.Fatal("a task should not quit")
	}
}
