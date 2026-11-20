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

func TestUsagePendingExistsBeforeHTTPDispatch(t *testing.T) {
	c, s := usageRegressionConfig(t, "unused")
	calls := 0
	c.Client.HTTP.Transport = stoppedJobTransport(func(*http.Request) (*http.Response, error) {
		calls++
		entries, err := os.ReadDir(filepath.Dir(s.OutputDir()))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range entries {
			found = found || strings.HasPrefix(e.Name(), "usage-pending-")
		}
		if !found {
			t.Error("provider dispatched before durable pending evidence")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"))}, nil
	})
	if _, _, err := c.Run(context.Background(), []llm.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	totals, err := s.UsageTotals()
	if err != nil || totals.RecoveryGap || totals.Attempts != 1 {
		t.Fatalf("%+v %v", totals, err)
	}
}
