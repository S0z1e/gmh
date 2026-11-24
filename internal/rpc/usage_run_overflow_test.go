package rpc

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	"minimalharness/internal/turnbudget"
)

func TestUsageRegressionCurrentRunOverflowRemainsValid(t *testing.T) {
	s, out := usageRegressionServer(t, false)
	zero := int64(0)
	s.app.Cfg.TurnBudget = &turnbudget.Policy{MaxTokens: &zero, MaxSessionTokens: &zero}
	count := 0
	s.app.Cfg.Client.HTTP.Transport = usageRegressionTransport(func(*http.Request) (*http.Response, error) {
		count++
		u := fmt.Sprintf(`"prompt_tokens":%d,"completion_tokens":0,"total_tokens":%d`, math.MaxInt, math.MaxInt)
		if count == 2 {
			u = `"prompt_tokens":0,"completion_tokens":1,"total_tokens":1`
		}
		body := `data: {"choices":[{"delta":{"content":"answer"}}],"usage":{` + u + "}}\n\ndata: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	s.runTurn(Request{Message: "first"})
	s.runTurn(Request{Message: "second"})
	out.Reset()
	s.handleQuery(Request{Type: "usage"})
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	t.Logf("response=%s; run=%+v; lastErr=%v", out.Bytes(), s.runner.RunUsage, s.runner.LastErr)
	if s.runner.MeteringAvailable {
		t.Error("overflow remained available")
	}
	if !s.runner.RunUsage.Valid() {
		t.Fatal("current-run aggregate contains invalid counters despite persisted overflow marker")
	}
}
