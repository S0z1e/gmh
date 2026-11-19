package rpc

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUsageRegressionMismatchedTotalPricingIsPublishedAsComplete(t *testing.T) {
	s, out := usageRegressionServer(t, true)
	s.app.Cfg.Client.HTTP.Transport = usageRegressionTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":100}}\n\ndata: [DONE]\n\n"))}, nil
	})
	s.runTurn(Request{Message: "hello"})
	out.Reset()
	s.handleQuery(Request{Type: "usage"})
	t.Logf("response=%s", out.Bytes())
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	data := result["data"].(map[string]any)
	if _, ok := data["current_run_cost_usd"]; ok {
		t.Error("unexplained total published as completely priced current run")
	}
	if _, ok := data["cost_usd"]; ok {
		t.Error("unexplained total published as completely priced lifetime")
	}
}
