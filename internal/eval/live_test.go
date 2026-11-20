package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveRequiresExplicitBudgetAndModel(t *testing.T) {
	good := LiveOptions{Config: filepath.Join(t.TempDir(), "config.json"), Model: "pinned", MaxCostUSD: 1, MaxTokens: 100}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []LiveOptions{{}, {Config: good.Config, Model: good.Model, MaxTokens: 100}, {Config: good.Config, Model: good.Model, MaxCostUSD: 1}, {Config: good.Config, MaxCostUSD: 1, MaxTokens: 100}} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
func TestLiveFileOracleRejectsClaimAndScopeChanges(t *testing.T) {
	root := t.TempDir()
	before := map[string]string{"keep.txt": "keep"}
	want := map[string]string{"result.txt": "correct"}
	if err := writeFixture(root, before); err != nil {
		t.Fatal(err)
	}
	if _, err := oracle(root, before, want); err == nil {
		t.Fatal("accepted missing result")
	}
	os.WriteFile(filepath.Join(root, "result.txt"), []byte("wrong but model says done"), 0600)
	if _, err := oracle(root, before, want); err == nil {
		t.Fatal("trusted completion claim")
	}
	os.WriteFile(filepath.Join(root, "result.txt"), []byte("correct"), 0600)
	if _, err := oracle(root, before, want); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(root, "extra"), 0700)
	if scope, err := oracle(root, before, want); err == nil || len(scope) == 0 {
		t.Fatal("ignored extra empty directory")
	}
}

type evalTransport func(*http.Request) (*http.Response, error)

func (f evalTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestLiveBudgetGateRejectsExhaustedAndUnknownUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := session.Open("budget", session.OpenOptions{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cost := 0.1
	a := session.UsageAttempt{ID: "first", Request: llm.RequestTrace{ID: "first", Usage: &llm.Usage{PromptTokens: 10, TotalTokens: 10}, TokensComplete: true}, EstimateUSD: &cost}
	if err := s.RecordUsage(a); err != nil {
		t.Fatal(err)
	}
	calls := 0
	base := evalTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid", nil)
	for _, opts := range []LiveOptions{{MaxCostUSD: 1, MaxTokens: 10}, {MaxCostUSD: 0.1, MaxTokens: 100}} {
		gate := budgetTransport{base: base, store: s, options: opts}
		if _, err := gate.RoundTrip(req); err == nil {
			t.Fatal("ignored exhausted suite budget")
		}
	}
	a.ID = "unknown"
	a.Request.ID = a.ID
	a.Request.Usage = nil
	a.EstimateUSD = nil
	if err := s.RecordUsage(a); err != nil {
		t.Fatal(err)
	}
	gate := budgetTransport{base: base, store: s, options: LiveOptions{MaxCostUSD: 1, MaxTokens: 100}}
	if _, err := gate.RoundTrip(req); err == nil {
		t.Fatal("unknown consumption permitted another paid call")
	}
	if calls != 0 {
		t.Fatalf("calls=%d", calls)
	}
}
func TestLiveTaskUsesActualHarnessAndIndependentOracle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, key := range []string{"OPENAI_PRICE_INPUT_PER_MTOK", "OPENAI_PRICE_CACHED_PER_MTOK", "OPENAI_PRICE_OUTPUT_PER_MTOK"} {
		t.Setenv(key, "1")
	}
	task := liveTasks()[0]
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		delta := map[string]any{"content": "done"}
		if calls <= 2 {
			name := "write"
			args := map[string]any{"path": "value.txt", "content": "42", "overwrite": true}
			if calls == 2 {
				name = "bash"
				args = map[string]any{"command": task.check}
			}
			b, _ := json.Marshal(args)
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("call-%d", calls), "type": "function", "function": map[string]any{"name": name, "arguments": string(b)}}}}
		}
		frame := map[string]any{"choices": []any{map[string]any{"delta": delta}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}
		b, _ := json.Marshal(frame)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	config := filepath.Join(t.TempDir(), "config.json")
	data, _ := json.Marshal(map[string]any{"providers": map[string]any{"fixture": map[string]any{"api": "chat", "baseUrl": srv.URL, "apiKey": "fake", "model": "fixture-model"}}})
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	result, totals, err := runLiveTask(context.Background(), task, LiveOptions{Config: config, Model: "fixture-model", MaxCostUSD: 1, MaxTokens: 1000}, 0, 0)
	if err != nil || !result.Passed || calls != 3 || result.ProviderTokens == nil || *result.ProviderTokens != 45 || totals.Attempts != 3 || result.CostUSD == nil || result.FalseCompletion {
		t.Fatalf("result=%+v totals=%+v calls=%d err=%v", result, totals, calls, err)
	}
}
