package rpc

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/llm"
)

func TestRequestTraceOverRPCAndAudit(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "Done."}})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p", "type": "prompt", "message": "private-user-content"})
	d.waitFor("response", 5*time.Second)
	ev := d.waitFor("llm_request", 5*time.Second)
	request, ok := ev["request"].(map[string]any)
	if !ok || request["relation"] != "baseline" || request["outcome"] != "complete" || ev["attempt"] != float64(1) {
		t.Fatalf("event=%v", ev)
	}
	usage, ok := request["usage"].(map[string]any)
	if !ok || usage["cache_usage_reported"] != false {
		t.Fatalf("cache presence=%v", usage)
	}
	settled := d.waitFor("agent_settled", 5*time.Second)
	u := settled["usage"].(map[string]any)
	if u["cache_usage_reported"] != false || u["cache_reported_prompt_tokens"] != float64(0) {
		t.Fatalf("usage=%v", u)
	}
	data, err := os.ReadFile(a.Store.AuditPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"kind":"llm_request"`) || !strings.Contains(string(data), request["request_hash"].(string)) {
		t.Fatalf("audit missing request: %s", data)
	}
	b, _ := json.Marshal(request)
	if strings.Contains(string(b), "private-user-content") || strings.Contains(string(data), "private-user-content") {
		t.Fatal("diagnostics leaked content")
	}
	d.quit(t)
}

func TestUsageJSONCachePresence(t *testing.T) {
	for _, reported := range []bool{false, true} {
		b, err := json.Marshal(usageJSON(llm.Usage{PromptTokens: 100, CacheUsageReported: reported}))
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		_ = json.Unmarshal(b, &fields)
		if fields["cache_usage_reported"] != reported {
			t.Fatalf("usage=%s", b)
		}
	}
}
