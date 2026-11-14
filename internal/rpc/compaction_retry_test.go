package rpc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"minimalharness/internal/llm"
)

func TestCompactionRetriesThroughRPC(t *testing.T) {
	for _, automatic := range []bool{true, false} {
		name := "manual"
		if automatic {
			name = "automatic"
		}
		t.Run(name, func(t *testing.T) {
			a, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "done"}})
			var summaryCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Messages []llm.Message `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				text := "verification complete"
				if len(request.Messages) > 0 && strings.Contains(request.Messages[0].Content, "compact conversation summaries") {
					if summaryCalls.Add(1) == 1 {
						w.Header().Set("Retry-After", "1")
						w.WriteHeader(http.StatusForbidden)
						fmt.Fprint(w, "RBAC: access denied")
						return
					}
					text = "verification summary"
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, sseChunk(map[string]any{"content": text}, "stop", map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15})+"data: [DONE]\n\n")
			}))
			defer srv.Close()
			a.Cfg.Client.BaseURL = srv.URL
			a.Cfg.Client.MaxTokens = 1000
			a.Cfg.ContextLimit = 10000
			if automatic {
				a.Cfg.LastPromptTokens = 8000
			}
			d := startDriver(t, a, runner)
			d.waitFor("ready", 5*time.Second)
			d.send(map[string]any{"id": "prompt", "type": "prompt", "message": "verify the build"})
			d.waitFor("response", 5*time.Second)
			if !automatic {
				d.waitFor("agent_settled", 5*time.Second)
				d.send(map[string]any{"id": "compact", "type": "compact"})
			}
			retry := d.waitFor("step_retry", 5*time.Second)
			if !strings.Contains(fmt.Sprint(retry["text"]), "Compaction failed") {
				t.Fatalf("retry not labelled as compaction: %v", retry)
			}
			d.waitFor("compacted", 5*time.Second)
			if automatic {
				settled := d.waitFor("agent_settled", 5*time.Second)
				if settled["error"] != nil || settled["reason"] != "end_turn" || settled["text"] != "verification complete" {
					t.Fatalf("transient compaction error ended the turn: %v", settled)
				}
			} else {
				d.waitFor("response", 5*time.Second)
			}
			if summaryCalls.Load() != 2 {
				t.Fatalf("summary calls = %d, want failed attempt plus retry", summaryCalls.Load())
			}
			d.quit(t)
		})
	}
}
