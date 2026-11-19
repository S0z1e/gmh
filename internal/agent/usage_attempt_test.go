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

func TestUsageIncludesFailedAndRetriedProviderAttempts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	store, err := session.Open("usage", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	calls := 0
	cfg := &Config{Session: store, Exec: &tools.Executor{Root: root}, Mode: "auto", MaxLLMAttempts: 2}
	cfg.Client = &llm.Client{API: "chat", Model: "fake", APIKey: "fake", BaseURL: "https://example.invalid", HTTP: &http.Client{Transport: stoppedJobTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `data: {"choices":[{"delta":{"content":"answer"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n"
		if calls%2 == 0 {
			body += "data: [DONE]\n\n"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	_, u, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "hello"}})
	if err != nil || calls != 2 || u.TotalTokens != 30 {
		t.Fatalf("usage=%+v calls=%d err=%v", u, calls, err)
	}
	got, err := store.UsageTotals()
	if err != nil || got.Attempts != 2 || got.Usage.TotalTokens != 30 {
		t.Fatalf("ledger=%+v %v", got, err)
	}
	cfg.MaxLLMAttempts = 1
	_, u, err = cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "fail"}})
	if err == nil || u.TotalTokens != 15 {
		t.Fatalf("failed usage=%+v err=%v", u, err)
	}
	got, err = store.UsageTotals()
	if err != nil || got.Attempts != 3 || got.Usage.TotalTokens != 45 {
		t.Fatalf("ledger=%+v %v", got, err)
	}
}

func TestUsageLedgerFailureStopsBeforeTools(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	store, err := session.Open("usage", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	// Closing the writer simulates persistence failure after a fully received reply.
	store.Close()
	cfg := &Config{Session: store, Exec: &tools.Executor{Root: root}, Tools: ToolDefs(), Mode: "auto"}
	cfg.Client = &llm.Client{API: "chat", Model: "fake", APIKey: "fake", BaseURL: "https://example.invalid", HTTP: &http.Client{Transport: stoppedJobTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}],\"usage\":{\"total_tokens\":7}}\n\ndata: [DONE]\n\n"))}, nil
	})}}
	_, u, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "hello"}})
	if err == nil || !strings.Contains(err.Error(), "usage") || u.TotalTokens != 7 {
		t.Fatalf("usage=%+v err=%v", u, err)
	}
}

func TestUsageMissingCountersDifferFromReportedZero(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	store, err := session.Open("usage", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	calls := 0
	cfg := &Config{Session: store, Exec: &tools.Executor{Root: root}, Mode: "auto"}
	cfg.Client = &llm.Client{API: "chat", Model: "fake", APIKey: "fake", BaseURL: "https://example.invalid", HTTP: &http.Client{Transport: stoppedJobTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `data: {"choices":[{"delta":{"content":"answer"}}]`
		if calls == 2 {
			body += `,"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`
		}
		body += "}\n\ndata: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	for range 2 {
		if _, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "hello"}}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.UsageTotals()
	if err != nil || got.Attempts != 2 || got.UnreportedAttempts != 1 || got.PartialAttempts != 0 {
		t.Fatalf("metering=%+v %v", got, err)
	}
}
