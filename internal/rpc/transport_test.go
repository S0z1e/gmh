package rpc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRPCOutputDisconnectCancelsProviderWithoutStdinEOF(t *testing.T) {
	for _, operation := range []string{"prompt", "compact"} {
		t.Run(operation, func(t *testing.T) {
			s, _ := usageRegressionServer(t, false)
			if operation == "compact" {
				m := llm.Message{Role: "user", Content: "preserve this history"}
				if err := s.app.Store.Append(m); err != nil {
					t.Fatal(err)
				}
				s.app.History = []llm.Message{m}
			}
			started, canceled := make(chan struct{}), make(chan struct{})
			s.app.Cfg.Client.HTTP.Transport = usageRegressionTransport(func(r *http.Request) (*http.Response, error) {
				close(started)
				<-r.Context().Done()
				close(canceled)
				return nil, r.Context().Err()
			})
			inR, inW := io.Pipe()
			outR, outW := io.Pipe()
			defer inW.Close()
			defer outR.Close()
			done := make(chan error, 1)
			go func() { done <- Run(s.app, s.runner, inR, outW) }()
			reader := bufio.NewReader(outR)
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(inW, "{\"id\":\"p\",\"type\":\""+operation+"\",\"message\":\"hello\"}\n"); err != nil {
				t.Fatal(err)
			}
			// Consume the prompt response/start so the only pending work is provider I/O.
			go func() { _, _ = io.Copy(io.Discard, reader) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("provider did not start")
			}
			outR.Close()
			_, _ = io.WriteString(inW, "{\"type\":\"get_state\"}\n")
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("stdout disconnect did not cancel provider")
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("output failure was silently ignored")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("server waited for stdin EOF")
			}

		})
	}
}

func TestRPCEventCorrelationAcrossRepeatedToolIDs(t *testing.T) {
	s, _ := usageRegressionServer(t, false)
	s.app.Cfg.Tools = tools.Defs()
	if err := os.WriteFile(filepath.Join(s.app.Root, "input.txt"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.app.Cfg.Client.HTTP.Transport = usageRegressionTransport(func(*http.Request) (*http.Response, error) {
		calls++
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\ndata: [DONE]\n\n"
		if calls%2 == 1 {
			body = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"same","type":"function","function":{"name":"read","arguments":"{\"path\":\"input.txt\"}"}}]}}]}` + "\n\ndata: [DONE]\n\n"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	d := startDriver(t, s.app, s.runner)
	ready := d.waitFor("ready", time.Second)
	connection := ready["connection_id"]
	if connection == nil {
		t.Fatal("ready has no connection identity")
	}
	turns := map[string]bool{}
	invocations := map[string]bool{}
	for _, id := range []string{"first", "second"} {
		d.send(map[string]any{"id": id, "type": "prompt", "message": "read the file"})
		accepted := d.waitFor("response", time.Second)
		start := d.waitFor("turn_started", time.Second)
		turn, ok := start["turn_id"].(string)
		if !ok || turn == "" || turns[turn] {
			t.Fatalf("turn identity reused: %+v", start)
		}
		turns[turn] = true
		data, ok := accepted["data"].(map[string]any)
		if !ok || data["turn_id"] != turn || accepted["sequence"].(float64) >= start["sequence"].(float64) {
			t.Fatalf("acceptance missing or out of order: %+v then %+v", accepted, start)
		}
		tool := d.waitFor("tool_start", time.Second)
		end := d.waitFor("tool_end", time.Second)
		settled := d.waitFor("agent_settled", time.Second)
		for _, ev := range []map[string]any{start, tool, end, settled} {
			if ev["turn_id"] != turn || ev["request_id"] != id || ev["session"] != "native" || ev["connection_id"] != connection {
				t.Fatalf("uncorrelated event: %+v", ev)
			}
		}
		if tool["step_id"] == nil || tool["step_id"] != end["step_id"] {
			t.Fatalf("tool step mismatch: %+v %+v", tool, end)
		}
		inv, ok := tool["invocation_id"].(string)
		if !ok || inv == "" || invocations[inv] || end["invocation_id"] != inv {
			t.Fatalf("invocation mismatch: %+v %+v", tool, end)
		}
		invocations[inv] = true
		if tool["sequence"].(float64) >= end["sequence"].(float64) || end["sequence"].(float64) >= settled["sequence"].(float64) {
			t.Fatal("wire sequence out of order")
		}
	}
	d.send(map[string]any{"type": "quit"})
	select {
	case err := <-d.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("quit did not complete")
	}
}
