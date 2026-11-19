package agent

import (
	"context"
	"io"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"net/http"
	"strings"
	"testing"
)

func usageRegressionConfig(t *testing.T, body string) (*Config, *session.Store) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	s, err := session.Open("native", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &Config{Session: s, Exec: &tools.Executor{Root: root}, Mode: "auto", MaxLLMAttempts: 1}
	c.Client = &llm.Client{API: "chat", Model: "fake", APIKey: "fake", BaseURL: "https://example.invalid", HTTP: &http.Client{Transport: stoppedJobTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	return c, s
}

func TestUsageRegressionEmptyUsageIsUnreported(t *testing.T) {
	c, s := usageRegressionConfig(t, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}],\"usage\":{}}\n\ndata: [DONE]\n\n")
	c.UsageRates = llm.Rates{Configured: true, InputPerMTok: 1, OutputPerMTok: 1, Source: "test"}
	if _, _, err := c.Run(context.Background(), []llm.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.UsageTotals()
	if err != nil {
		t.Fatal(err)
	}
	if got.UnreportedAttempts != 1 || got.PricedAttempts != 0 {
		t.Fatalf("counter-free usage counted as reported/priced: %+v", got)
	}
}

func TestUsageRegressionCompactionRetriesAndCallbackDedupe(t *testing.T) {
	c, s := usageRegressionConfig(t, "unused")
	calls := 0
	c.MaxLLMAttempts = 2
	c.Client.HTTP.Transport = stoppedJobTransport(func(*http.Request) (*http.Response, error) {
		calls++
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"summary\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n"
		if calls == 2 {
			body += "data: [DONE]\n\n"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	c.CompactHistory = func(ctx context.Context, h []llm.Message) (string, llm.Usage, error) { return c.Summarize(ctx, h, nil) }
	_, spent, err := c.Compact(context.Background(), []llm.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.UsageTotals()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || spent.TotalTokens != 30 || got.Attempts != 2 || got.Usage.TotalTokens != 30 {
		t.Fatalf("calls=%d spent=%+v ledger=%+v", calls, spent, got)
	}
}

func TestUsageRegressionCancellationKeepsObservedUsage(t *testing.T) {
	c, s := usageRegressionConfig(t, "data: {\"usage\":{\"prompt_tokens\":10}}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.OnEvent = func(e Event) {
		if e.Kind == EventTextDelta {
			cancel()
		}
	}
	_, spent, err := c.Run(ctx, []llm.Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected cancellation")
	}
	got, loadErr := s.UsageTotals()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if spent.TotalTokens != 10 || got.Usage.TotalTokens != 10 || got.Attempts != 1 || got.PartialAttempts != 1 {
		t.Fatalf("spent=%+v ledger=%+v err=%v", spent, got, err)
	}
}
