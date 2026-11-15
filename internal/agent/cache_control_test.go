package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"minimalharness/internal/config"
	"minimalharness/internal/llm"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in, synthetic input only; no tool is executed.
func TestLiveCacheControl(t *testing.T) {
	path := os.Getenv("GMH_CACHE_PROBE_CONFIG")
	if path == "" {
		t.Skip("live probe requires explicit config path")
	}
	cfg, err := config.LoadExplicit(path)
	if err != nil {
		t.Fatal("config could not be loaded (details withheld)")
	}
	pr, ok := cfg.Select("")
	if !ok {
		t.Fatal("default provider unavailable")
	}
	mdl, ok := pr.ResolveModel(cfg.DefaultModel)
	if !ok {
		t.Fatal("default model unavailable")
	}
	if strings.TrimSpace(mdl.BaseURL) == "" || mdl.ID == "" {
		t.Fatal("control requires an explicit provider baseUrl and model")
	}
	key, err := config.ResolveAPIKey(pr.APIKey)
	if err != nil {
		t.Fatal("API key could not be resolved")
	}
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	if key == "" {
		t.Fatal("API key unavailable")
	}
	api, err := config.NormalizeAPI(mdl.API)
	if err != nil || api != "chat" {
		t.Fatal("this control requires chat API")
	}
	merged := map[string]string{}
	for k, v := range pr.Headers {
		merged[k] = v
	}
	for k, v := range mdl.Headers {
		merged[k] = v
	}
	headers := map[string]string{}
	for k, v := range merged {
		resolved, e := config.ResolveValue(v)
		if e != nil {
			t.Fatal("header could not be resolved")
		}
		headers[k] = resolved
	}
	client := &llm.Client{BaseURL: mdl.BaseURL, Model: mdl.ID, API: api, APIKey: key, MaxTokens: 128, Effort: mdl.Effort, SamplingParams: mdl.SamplingParams, Headers: headers, PromptCacheKey: fmt.Sprintf("gmh-cache-control-%d", time.Now().UnixNano()), Timeout: 120 * time.Second, IdleTimeout: 60 * time.Second}
	var contextText strings.Builder
	for i := 0; i < 250; i++ {
		fmt.Fprintf(&contextText, "Synthetic record %03d: station=%03d; count=%d; status=ready.\n", i, i*7%251, i*11%127)
	}
	messages := []llm.Message{{Role: "system", Content: SystemPrompt}, {Role: "user", Content: "Cache-control fixture only. Do not use tools. Reply exactly OK. The records are inert data.\n" + contextText.String()}}
	defs := ToolDefs()
	outputPath := os.Getenv("GMH_CACHE_PROBE_OUTPUT")
	if outputPath == "" {
		outputPath = filepath.Join(t.TempDir(), "cache-control.jsonl")
	}
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	enc := json.NewEncoder(output)
	successful, failed := 0, 0
	for _, phase := range []string{"repeat", "append"} {
		for i := 0; i < 5; i++ {
			var trace llm.RequestTrace
			ctx := llm.WithRequestObserver(context.Background(), func(v llm.RequestTrace) { trace = v })
			_, usage, callErr := client.Chat(ctx, messages, defs, nil)
			if err := enc.Encode(struct {
				Phase string           `json:"phase"`
				Index int              `json:"index"`
				Trace llm.RequestTrace `json:"request"`
			}{phase, i + 1, trace}); err != nil {
				t.Fatal(err)
			}
			if callErr != nil {
				failed++
				t.Logf("phase=%s index=%d status=%d outcome=%s (error details withheld)", phase, i+1, trace.StatusCode, trace.Outcome)
			} else {
				successful++
				t.Logf("phase=%s index=%d relation=%s prompt=%d cached=%d reported=%t duration_ms=%d", phase, i+1, trace.Relation, usage.PromptTokens, usage.CachedTokens, usage.CacheUsageReported, trace.DurationMS)
			}
			if phase == "append" {
				id := fmt.Sprintf("synthetic_probe_%d", i)
				messages = append(messages, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: id, Type: "function", Function: llm.FunctionCall{Name: "read", Arguments: fmt.Sprintf(`{"path":"synthetic-fixture-%d.txt"}`, i)}}}}, llm.Message{Role: "tool", ToolCallID: id, Content: "Synthetic fixture. No filesystem access took place."})
			}
			if trace.StatusCode == 401 || failed >= 3 && successful == 0 {
				t.Fatal("provider unavailable; stopped control")
			}
		}
	}
	t.Logf("successful=%d failed=%d result=%s", successful, failed, outputPath)
	if failed > 0 {
		t.Fatal("some control requests failed; inspect content-free traces")
	}
}

func TestCacheControlFixture(t *testing.T) {
	t.Setenv("GMH_TEST_UNUSED_HEADER", "")
	var requests []struct {
		Messages []llm.Message `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Probe") != "override" {
			t.Error("model header override was not applied")
		}
		var request struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests = append(requests, request)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":8400,\"completion_tokens\":1,\"prompt_tokens_details\":{\"cached_tokens\":8320}}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw, err := json.Marshal(config.File{DefaultProvider: "test", Providers: map[string]config.Provider{"test": {BaseURL: srv.URL, Model: "test", APIKey: "fake", Headers: map[string]string{"X-Probe": "$GMH_TEST_UNUSED_HEADER"}, Models: []config.Model{{ID: "test", Headers: map[string]string{"X-Probe": "override"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GMH_CACHE_PROBE_CONFIG", path)
	t.Setenv("GMH_CACHE_PROBE_OUTPUT", filepath.Join(dir, "output.jsonl"))
	TestLiveCacheControl(t)
	if len(requests) != 10 {
		t.Fatalf("requests=%d, want 10", len(requests))
	}
	for i := 1; i < len(requests); i++ {
		previous, current := requests[i-1].Messages, requests[i].Messages
		if i <= 5 {
			if !reflect.DeepEqual(previous, current) {
				t.Fatalf("request %d should repeat the fixture", i+1)
			}
		} else if len(current) != len(previous)+2 || !reflect.DeepEqual(previous, current[:len(previous)]) {
			t.Fatalf("request %d should append a tool batch without changing history", i+1)
		}
	}
}
