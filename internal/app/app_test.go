package app

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/agent"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
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

// writeTestPNG writes a tiny valid PNG to root.
func writeTestPNG(t *testing.T, root, name string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Images: -image loads at Setup, /image queues for the next turn and
// rides on the task message, and a bad path fails Setup.
func TestImageAttachAndSend(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi there\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	root := t.TempDir()
	cfg := `{"providers": {"mock": {"baseUrl": "http://127.0.0.1:1", "api": "chat", "models": [{"id": "mock", "input": ["text", "image"]}]}}}`
	if err := os.WriteFile(filepath.Join(root, ".harness.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestPNG(t, root, "pic.png")

	a, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5, Images: []string{"pic.png"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	if len(a.PendingImages) != 1 || a.PendingImages[0].MediaType != "image/png" {
		t.Fatalf("pending = %+v", a.PendingImages)
	}
	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	if quit := r.Handle(context.Background(), "describe the image"); quit {
		t.Fatal("task should not quit")
	}
	if len(a.PendingImages) != 0 {
		t.Fatal("pending images should clear after the turn")
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], "image_url") || !strings.Contains(bodies[0], "data:image/png;base64,") {
		t.Fatalf("request body has no image part: %v", bodies)
	}
	// /image queues for the next turn.
	if quit := r.Handle(context.Background(), "/image pic.png"); quit {
		t.Fatal("/image should not quit")
	}
	if len(a.PendingImages) != 1 {
		t.Fatalf("pending after /image = %d, want 1", len(a.PendingImages))
	}
	if !strings.Contains(strings.Join(printed, "\n"), "Attached pic.png") {
		t.Fatalf("no attach confirmation: %v", printed)
	}
	if quit := r.Handle(context.Background(), "again"); quit {
		t.Fatal("task should not quit")
	}
	if len(bodies) != 2 || !strings.Contains(bodies[1], "image_url") {
		t.Fatalf("second request has no image part: %v", bodies)
	}
	// /image with no argument lists pending (none left).
	printed = nil
	r.Handle(context.Background(), "/image")
	if !strings.Contains(strings.Join(printed, "\n"), "No images attached") {
		t.Fatalf("/image with no arg = %v", printed)
	}
	// A bad -image path fails Setup.
	if _, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5, Images: []string{"gone.png"}}); err == nil {
		t.Fatal("missing -image file should fail setup")
	}
}

// A model whose input is text-only refuses images, at Setup and at
// /image, with an error that says what to change.
func TestImageRefusedWithoutInput(t *testing.T) {
	srv := mockServer(t)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	root := t.TempDir()
	writeMockProvider(t, root)
	writeTestPNG(t, root, "pic.png")

	a, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	r.Handle(context.Background(), "/image pic.png")
	if len(a.PendingImages) != 0 {
		t.Fatal("refused image must not queue")
	}
	if !strings.Contains(strings.Join(printed, "\n"), "image input is not enabled") {
		t.Fatalf("/image refusal = %v", printed)
	}
	if _, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5, Images: []string{"pic.png"}}); err == nil || !strings.Contains(err.Error(), "image input is not enabled") {
		t.Fatalf("-image setup err = %v, want the image-input hint", err)
	}
}

// Two providers with distinct models, apis and limits, for /model.
func writeSwitchConfig(t *testing.T, root string) {
	t.Helper()
	cfg := `{
		"defaultProvider": "prov-a",
		"defaultModel": "a1",
		"providers": {
			"prov-a": {
				"baseUrl": "http://127.0.0.1:11/v1", "api": "chat", "apiKey": "$KEY_A",
				"models": [
					{"id": "a1", "contextWindow": 1000, "maxTokens": 100, "input": ["text"]},
					{"id": "a2", "api": "anthropic-messages", "contextWindow": 2000, "maxTokens": 200, "input": ["text", "image"]}
				]
			},
			"prov-b": {
				"baseUrl": "http://127.0.0.1:12/v1", "api": "openai-responses", "apiKey": "$KEY_B",
				"models": [{"id": "b1", "contextWindow": 3000, "maxTokens": 300}]
			},
			"prov-nokey": {
				"baseUrl": "http://127.0.0.1:13/v1", "api": "chat", "apiKey": "$KEY_MISSING",
				"models": [{"id": "n1"}]
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(root, ".harness.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KEY_A", "key-a")
	t.Setenv("KEY_B", "key-b")
	root := t.TempDir()
	writeSwitchConfig(t, root)
	a, err := Setup(Params{Root: root, MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	if a.Model.ID != "a1" || a.Cfg.Client.API != "chat" || a.ContextLimit != 1000 {
		t.Fatalf("initial model = %s/%s limit %d", a.Model.ID, a.Cfg.Client.API, a.ContextLimit)
	}
	// Same provider, different api: the client is rebuilt, the
	// context limit and image-input flag follow the new model.
	prov, model, api, err := a.SwitchModel("", "a2")
	if err != nil {
		t.Fatal(err)
	}
	if prov != "prov-a" || model != "a2" || api != "anthropic" {
		t.Fatalf("switch = %s/%s/%s", prov, model, api)
	}
	if a.Cfg.Client.Model != "a2" || a.Cfg.Client.API != "anthropic" || a.Cfg.Client.MaxTokens != 200 {
		t.Fatalf("client = %+v", a.Cfg.Client)
	}
	if a.ContextLimit != 2000 || !a.Cfg.Exec.ImageInput {
		t.Fatalf("limit = %d, imageInput = %v", a.ContextLimit, a.Cfg.Exec.ImageInput)
	}
	// Another provider by name: base URL and key follow it.
	prov, model, api, err = a.SwitchModel("prov-b", "b1")
	if err != nil {
		t.Fatal(err)
	}
	if prov != "prov-b" || model != "b1" || api != "responses" {
		t.Fatalf("switch = %s/%s/%s", prov, model, api)
	}
	if a.Cfg.Client.BaseURL != "http://127.0.0.1:12/v1" || a.Cfg.Client.APIKey != "key-b" {
		t.Fatalf("client = %+v", a.Cfg.Client)
	}
	if a.ContextLimit != 3000 || a.Cfg.Exec.ImageInput {
		t.Fatalf("limit = %d, imageInput = %v", a.ContextLimit, a.Cfg.Exec.ImageInput)
	}
	// A bare model id finds its provider (current provider does not
	// have it, prov-a does).
	prov, model, api, err = a.SwitchModel("", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if prov != "prov-a" || model != "a1" || api != "chat" {
		t.Fatalf("bare-name switch = %s/%s/%s", prov, model, api)
	}
	// Errors leave the current model untouched.
	if _, _, _, err := a.SwitchModel("", "nope"); err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("unknown model err = %v", err)
	}
	if _, _, _, err := a.SwitchModel("nope", "a1"); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("unknown provider err = %v", err)
	}
	if _, _, _, err := a.SwitchModel("prov-nokey", "n1"); err == nil || !strings.Contains(err.Error(), "KEY_MISSING") {
		t.Fatalf("missing key err = %v", err)
	}
	if a.Model.ID != "a1" || a.Provider != "prov-a" {
		t.Fatalf("failed switches moved the model to %s/%s", a.Provider, a.Model.ID)
	}
	// FindModel: current provider first, then the rest.
	if p, ok := a.FindModel("b1"); !ok || p != "prov-b" {
		t.Fatalf("FindModel(b1) = %q %v", p, ok)
	}
	if p, ok := a.FindModel("a2"); !ok || p != "prov-a" {
		t.Fatalf("FindModel(a2) = %q %v", p, ok)
	}
	if _, ok := a.FindModel("nope"); ok {
		t.Fatal("FindModel should fail for an unknown id")
	}
}

// /model lists, switches by provider/model and by bare name, and
// refuses unknown models without moving.
func TestModelCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KEY_A", "key-a")
	t.Setenv("KEY_B", "key-b")
	root := t.TempDir()
	writeSwitchConfig(t, root)
	a, err := Setup(Params{Root: root, MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	r.Handle(context.Background(), "/model")
	joined := strings.Join(printed, "\n")
	for _, want := range []string{"* prov-a/a1 (chat)", "prov-a/a2 (anthropic)", "prov-b/b1 (responses)", "Usage: /model"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("/model list = %v, want %q", printed, want)
		}
	}
	printed = nil
	r.Handle(context.Background(), "/model prov-a/a2")
	if a.Model.ID != "a2" || a.Cfg.Client.API != "anthropic" {
		t.Fatalf("after /model, model = %s/%s", a.Model.ID, a.Cfg.Client.API)
	}
	if !strings.Contains(strings.Join(printed, "\n"), "Switched to prov-a/a2 (anthropic)") {
		t.Fatalf("switch confirmation = %v", printed)
	}
	printed = nil
	r.Handle(context.Background(), "/model b1")
	if a.Model.ID != "b1" || a.Provider != "prov-b" {
		t.Fatalf("bare /model b1 = %s/%s", a.Provider, a.Model.ID)
	}
	printed = nil
	r.Handle(context.Background(), "/model nope")
	if a.Model.ID != "b1" {
		t.Fatal("failed /model moved the model")
	}
	if !strings.Contains(strings.Join(printed, "\n"), "[error]") {
		t.Fatalf("bad /model = %v, want an error line", printed)
	}
}

// Setup loads AGENTS.md from the root into the system prompt.
func TestSetupLoadsProjectContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := t.TempDir()
	writeMockProvider(t, root)
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Project codename: ZEBRA-7.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Setup(Params{Root: root, MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	if !strings.Contains(a.Cfg.System, "ZEBRA-7") {
		t.Fatalf("system prompt = %q, want the AGENTS.md content", a.Cfg.System)
	}
}

// /sessions lists, /resume switches, /rename moves (current and other).
func TestSessionsCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := t.TempDir()
	writeMockProvider(t, root)
	a, err := Setup(Params{Root: root, MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	current := a.Store.ID

	other, err := session.New("other")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Append(llm.Message{Role: "user", Content: "task about beta"}); err != nil {
		t.Fatal(err)
	}
	other.Close()

	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	// List: current marked *, other with its preview.
	r.Handle(context.Background(), "/sessions")
	joined := strings.Join(printed, "\n")
	if !strings.Contains(joined, "* "+current) || !strings.Contains(joined, "other") || !strings.Contains(joined, "task about beta") {
		t.Fatalf("/sessions = %v", printed)
	}
	// Resume the other session: history loads, store id follows.
	printed = nil
	r.Handle(context.Background(), "/resume other")
	if a.Store.ID != "other" {
		t.Fatalf("after /resume, store = %q", a.Store.ID)
	}
	if len(a.History) != 1 || a.History[0].Content != "task about beta" {
		t.Fatalf("after /resume, history = %v", a.History)
	}
	if !strings.Contains(strings.Join(printed, "\n"), "Resumed session other (1 messages)") {
		t.Fatalf("resume confirmation = %v", printed)
	}
	// Rename the (now current) session: the store follows.
	printed = nil
	r.Handle(context.Background(), "/rename other renamed")
	if a.Store.ID != "renamed" {
		t.Fatalf("after /rename current, store = %q", a.Store.ID)
	}
	if !strings.Contains(strings.Join(printed, "\n"), "Renamed session other → renamed.") {
		t.Fatalf("rename confirmation = %v", printed)
	}
	// Rename a session that is not current.
	third, err := session.New("third")
	if err != nil {
		t.Fatal(err)
	}
	third.Close()
	printed = nil
	r.Handle(context.Background(), "/rename third third2")
	if _, err := os.Stat(filepath.Join(session.Dir(), "third2.jsonl")); err != nil {
		t.Fatalf("renamed file missing: %v", err)
	}
	// Errors: unknown resume, rename onto an existing id, bad id.
	for _, bad := range []string{"/resume nope", "/rename renamed third2", "/rename renamed bad/id", "/resume", "/rename renamed"} {
		printed = nil
		r.Handle(context.Background(), bad)
		if !strings.Contains(strings.Join(printed, "\n"), "[error]") && !strings.Contains(strings.Join(printed, "\n"), "Usage:") {
			t.Fatalf("%q = %v, want an error or usage line", bad, printed)
		}
	}
	if a.Store.ID != "renamed" {
		t.Fatal("failed commands moved the session")
	}
	// The list now shows the renamed session.
	printed = nil
	r.Handle(context.Background(), "/sessions")
	joined = strings.Join(printed, "\n")
	if !strings.Contains(joined, "* renamed") || strings.Contains(joined, " other ") {
		t.Fatalf("/sessions after rename = %v", printed)
	}
}
