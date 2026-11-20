package agent

import (
	"context"
	"io"
	"minimalharness/internal/llm"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageRegressionInvalidNestedOnly(t *testing.T) {
	c, s := usageRegressionConfig(t, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}],\"usage\":{\"completion_tokens_details\":{\"reasoning_tokens\":-1}}}\n\ndata: [DONE]\n\n")
	_, spent, err := c.Run(context.Background(), []llm.Message{{Role: "user", Content: "hi"}})
	got, loadErr := s.UsageTotals()
	t.Logf("spent=%+v err=%v totals=%+v loadErr=%v", spent, err, got, loadErr)
	if err == nil || got.InvalidAttempts != 1 {
		t.Error("invalid wire usage was treated as ordinary missing usage")
	}
}

func TestUsageRegressionMismatchedTotalPricing(t *testing.T) {
	c, s := usageRegressionConfig(t, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":100}}\n\ndata: [DONE]\n\n")
	c.UsageRates = llm.Rates{Configured: true, Complete: true, InputPerMTok: 1000, CachedPerMTok: 1000, OutputPerMTok: 2000}
	_, _, err := c.Run(context.Background(), []llm.Message{{Role: "user", Content: "hi"}})
	got, loadErr := s.UsageTotals()
	t.Logf("err=%v totals=%+v loadErr=%v", err, got, loadErr)
	if got.PricedAttempts != 0 {
		t.Error("unexplained tokens counted as fully priced")
	}
}

func TestUsageRegressionInvalidNestedOnlyAllowsTools(t *testing.T) {
	c, s := usageRegressionConfig(t, "unused")
	c.Tools = ToolDefs()
	calls := 0
	c.Client.HTTP.Transport = stoppedJobTransport(func(*http.Request) (*http.Response, error) {
		calls++
		body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"w\",\"type\":\"function\",\"function\":{\"name\":\"write\",\"arguments\":\"{\\\"path\\\":\\\"proof.txt\\\",\\\"content\\\":\\\"ran\\\"}\"}}]}}],\"usage\":{\"completion_tokens_details\":{\"reasoning_tokens\":-1}}}\n\ndata: [DONE]\n\n"
		if calls > 1 {
			body = "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	_, _, err := c.Run(context.Background(), []llm.Message{{Role: "user", Content: "hi"}})
	got, _ := s.UsageTotals()
	b, readErr := os.ReadFile(filepath.Join(c.Exec.Root, "proof.txt"))
	t.Logf("provider calls=%d err=%v totals=%+v file=%q readErr=%v", calls, err, got, b, readErr)
	if readErr == nil {
		t.Error("tool ran after invalid usage")
	}
	if calls > 1 {
		t.Error("next provider attempt ran after invalid usage")
	}
}
