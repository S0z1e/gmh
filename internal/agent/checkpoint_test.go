package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func appendFixtureBatch(msgs []llm.Message, i, size int) []llm.Message {
	id := fmt.Sprint(i)
	return append(msgs,
		llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: id, Type: "function", Function: llm.FunctionCall{Name: "read", Arguments: fmt.Sprintf(`{"path":"fixture-%d"}`, i)}}}},
		llm.Message{Role: "tool", ToolCallID: id, Content: strings.Repeat("x", size)})
}

func TestPromptViewIsAppendOnlyUntilCheckpoint(t *testing.T) {
	cfg := &Config{}
	msgs := []llm.Message{{Role: "user", Kind: "task", Content: "task"}}
	previous := cfg.promptView(msgs)
	for i := 0; i < 100; i++ {
		msgs = appendFixtureBatch(msgs, i, 16*1024)
		current := cfg.promptView(msgs)
		if !isPrefixExtension(previous, current) {
			t.Fatalf("ordinary rendering rewrote sent history at batch %d", i)
		}
		previous = current
	}
}

func TestPruneCheckpointPersistsAndPreservesNewestBatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("prune-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	msgs := []llm.Message{{Role: "user", Kind: "task", Content: "finish the task"}}
	for i := 0; i < 8; i++ {
		msgs = appendFixtureBatch(msgs, i, 16000)
	}
	for _, m := range msgs {
		if err := store.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	original := append([]llm.Message(nil), msgs...)
	archive := ""
	cfg := &Config{Session: store, ContextLimit: 40000, OnCompact: func(_ []llm.Message, _ llm.Usage, path string) { archive = path }, CompactHistory: func(context.Context, []llm.Message) (string, llm.Usage, error) {
		t.Fatal("sufficient old tool output should be pruned without a summary call")
		return "", llm.Usage{}, nil
	}}
	if !cfg.needsCompact(msgs) {
		t.Fatal("fixture did not reach context budget")
	}
	out, _, err := cfg.compactInLoop(context.Background(), msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.needsCompact(out) {
		t.Fatal("checkpoint did not reclaim context")
	}
	if !reflect.DeepEqual(out[len(out)-2:], original[len(original)-2:]) {
		t.Fatal("newest tool batch changed")
	}
	if !reflect.DeepEqual(msgs, original) {
		t.Fatal("checkpoint mutated its input")
	}
	resumed, err := session.Load(store.ID)
	if err != nil || !reflect.DeepEqual(resumed, out) {
		t.Fatalf("resumed checkpoint differs: %v", err)
	}
	data, err := os.ReadFile(archive)
	if err != nil || !strings.Contains(string(data), strings.Repeat("x", 16000)) {
		t.Fatal("archive did not retain the full observations")
	}
	previous := cfg.promptView(out)
	out = appendFixtureBatch(out, 9, 1000)
	if !isPrefixExtension(previous, cfg.promptView(out)) {
		t.Fatal("view changed immediately after a checkpoint")
	}
}

func TestCheckpointFallsBackToSummaryWithoutEnoughHeadroom(t *testing.T) {
	msgs := []llm.Message{{Role: "user", Kind: "task", Content: "keep the sources"}, {Role: "assistant", Content: strings.Repeat("decision", 5000)}}
	msgs = appendFixtureBatch(msgs, 0, 1000)
	calls := 0
	cfg := &Config{ContextLimit: 20000, CompactHistory: func(context.Context, []llm.Message) (string, llm.Usage, error) {
		calls++
		return "task remains", llm.Usage{}, nil
	}}
	if _, _, err := cfg.compactInLoop(context.Background(), msgs, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("insufficient headroom did not fall back to summary")
	}
}

func TestPruneCheckpointDoesNotPersistTransientContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("transient-context")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	msgs := []llm.Message{{Role: "user", Kind: "task", Content: "task"}}
	for i := 0; i < 8; i++ {
		msgs = appendFixtureBatch(msgs, i, 16000)
	}
	for _, m := range msgs {
		if err := store.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	msgs = append(msgs, llm.Message{Role: "user", Content: "transient context hook instruction"})
	cfg := &Config{Session: store, ContextLimit: 40000}
	if _, ok := cfg.pruneCheckpoint(msgs); ok {
		t.Fatal("transient context must not be committed as raw history")
	}
	saved, err := session.Load(store.ID)
	if err != nil || len(saved) != len(msgs)-1 {
		t.Fatal("transient context reached the session")
	}
}

func TestCheckpointLargeRawResultsFallBackToSummary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("large-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	msgs := []llm.Message{{Role: "user", Kind: "task", Content: "task"}}
	for i := 0; i < 9; i++ {
		msgs = appendFixtureBatch(msgs, i, 1<<20)
	}
	for _, m := range msgs {
		if err := store.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	called := false
	cfg := &Config{Session: store, ContextLimit: DefaultContextLimit, CompactHistory: func(context.Context, []llm.Message) (string, llm.Usage, error) {
		called = true
		return "summary", llm.Usage{}, nil
	}}
	if _, _, err := cfg.compactInLoop(context.Background(), msgs, nil); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("oversized raw checkpoint did not use summary")
	}
	if _, err := session.Load(store.ID); err != nil {
		t.Fatalf("checkpoint cannot be resumed: %v", err)
	}
}

func TestPruneCheckpointAcceptsHydratedImages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("image-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	msgs := []llm.Message{{Role: "user", Kind: "task", Content: "task", Images: []llm.Image{{Path: "image.png", MediaType: "image/png", Data: "aGVsbG8="}}}}
	for i := 0; i < 8; i++ {
		msgs = appendFixtureBatch(msgs, i, 16000)
	}
	for _, m := range msgs {
		if err := store.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{Session: store, ContextLimit: 40000}
	out, ok := cfg.pruneCheckpoint(msgs)
	if !ok || out[0].Images[0].Data != msgs[0].Images[0].Data {
		t.Fatal("hydrated image blocked pruning or lost its data")
	}
}

func TestGraceCallCheckpointsNewResultsBeforeFinalRequest(t *testing.T) {
	msgs := []llm.Message{{Role: "user", Kind: "task", Content: "task"}}
	for i := 0; i < 30; i++ {
		msgs = appendFixtureBatch(msgs, i, 32000)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if estimatePromptTokens(request.Messages, nil) >= DefaultContextLimit {
			t.Error("grace request exceeds context limit")
		}
		fmt.Fprint(w, textSSE("final report"))
	}))
	defer srv.Close()
	cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, Model: "mock", APIKey: "fake"}, ContextLimit: DefaultContextLimit}
	text, _, ok := cfg.graceCall(context.Background(), msgs, "summarize now", nil)
	if !ok || text != "final report" {
		t.Fatalf("grace call=%q, %t", text, ok)
	}
}

func TestGraceCheckpointUsageIncludesFinalAttempt(t *testing.T) {
	for _, finish := range []string{"stop", "length"} {
		t.Run(finish, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"final\"},\"finish_reason\":%q}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n", finish)
			}))
			defer srv.Close()
			var events llm.Usage
			cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, Model: "mock", APIKey: "fake"}, ContextLimit: 10000,
				OnEvent: func(e Event) {
					if e.Kind == EventStepUsage {
						events.Add(e.Usage)
					}
				},
				CompactHistory: func(context.Context, []llm.Message) (string, llm.Usage, error) {
					return "summary", llm.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7}, nil
				},
			}
			cfg.LastPromptTokens = 8000
			_, usage, ok := cfg.graceCall(context.Background(), []llm.Message{{Role: "user", Kind: "task", Content: "finish"}}, "summarize now", nil)
			if usage.TotalTokens != 30 || events.TotalTokens != 30 || ok != (finish == "stop") {
				t.Fatalf("usage=%d events=%d ok=%t", usage.TotalTokens, events.TotalTokens, ok)
			}
		})
	}
}

func TestCheckpointRewriteFrequency(t *testing.T) {
	cfg := &Config{ContextLimit: DefaultContextLimit}
	msgs := []llm.Message{{Role: "user", Kind: "task", Content: "task"}}
	checkpoints := 0
	for i := 0; i < 100; i++ {
		msgs = appendFixtureBatch(msgs, i, 16*1024)
		if cfg.needsCompact(msgs) {
			var err error
			msgs, _, err = cfg.compactInLoop(context.Background(), msgs, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkpoints++
		}
		if cfg.needsCompact(msgs) {
			t.Fatal("checkpoint left context over budget")
		}
	}
	if checkpoints == 0 || checkpoints > 15 {
		t.Fatalf("100 batches caused %d rewrites, want occasional checkpoints", checkpoints)
	}
	t.Logf("100 x 16 KiB batches: %d checkpoints", checkpoints)
}

func TestCheckpointKeepsWireRequestsStableBetweenRewrites(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 100; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("fixture-%d", i)), []byte(strings.Repeat("x", 16*1024)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	defs := ToolDefs()
	calls, checkpoints := 0, 0
	var traces []llm.RequestTrace
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		input := estimatePromptTokens(request.Messages, defs)
		if calls < 100 {
			call := llm.ToolCall{ID: fmt.Sprint(calls), Type: "function", Function: llm.FunctionCall{Name: "read", Arguments: fmt.Sprintf(`{"path":"fixture-%d"}`, calls)}}
			chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": call.ID, "function": call.Function}}}}}})
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
		}
		calls++
		fmt.Fprintf(w, "data: {\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n", input)
	}))
	defer srv.Close()
	cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, Model: "mock", APIKey: "fake"}, Exec: &tools.Executor{Root: root}, Tools: defs, Mode: "auto", ContextLimit: DefaultContextLimit, TraceRequests: true,
		OnEvent: func(e Event) {
			if e.Request != nil {
				traces = append(traces, *e.Request)
			}
		},
		OnCompact: func(_ []llm.Message, _ llm.Usage, _ string) { checkpoints++ },
		CompactHistory: func(context.Context, []llm.Message) (string, llm.Usage, error) {
			t.Fatal("tool output checkpoints should suffice")
			return "", llm.Usage{}, nil
		},
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Kind: "task", Content: "read all fixtures"}})
	if err != nil || text != "done" || len(traces) != 101 {
		t.Fatalf("Run=%q, %v, requests=%d", text, err, len(traces))
	}
	rewrites := 0
	for _, trace := range traces[1:] {
		if trace.Relation == "rewrite" {
			rewrites++
		} else if trace.Relation != "append" {
			t.Fatalf("unexpected relation %s", trace.Relation)
		}
	}
	if rewrites != checkpoints || rewrites == 0 || rewrites > 15 {
		t.Fatalf("wire rewrites=%d checkpoints=%d", rewrites, checkpoints)
	}
	t.Logf("101 wire requests: %d checkpoints, %d append-only transitions", checkpoints, 100-rewrites)
}

func TestBlockedGraceCheckpointIncludesDrainedJob(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASH_ENV", "")
	root := t.TempDir()
	store, err := session.New("blocked-drain-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task := llm.Message{Role: "user", Kind: "task", Content: "run checks"}
	if err := store.Append(task); err != nil {
		t.Fatal(err)
	}
	ex := &tools.Executor{Root: root, Plugins: &fakePluginRunner{result: map[string]string{"read": strings.Repeat("x", 2000)}}}
	job, err := ex.BashBackground(context.Background(), tools.BashArgs{Command: "while [ ! -f release ]; do sleep 0.01; done; echo UNIQUE-JOB-RESULT", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	defer job.Cancel()
	calls := 0
	compacts := 0
	summarySawJob := false
	cfg := &Config{Exec: ex, Session: store, Tools: ToolDefs(), Mode: "auto", ContextLimit: 10000, DrainTimeout: time.Second}
	cfg.OnEvent = func(e Event) {
		if e.Kind == EventBackgroundWait {
			if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			<-job.Done()
		}
	}
	cfg.CompactHistory = func(_ context.Context, h []llm.Message) (string, llm.Usage, error) {
		compacts++
		for _, m := range h {
			if strings.Contains(m.Content, "UNIQUE-JOB-RESULT") {
				summarySawJob = true
			}
		}
		return "summary of available history", llm.Usage{}, nil
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := func() string {
			calls++
			if calls > 3 {
				return textSSE("blocked report")
			}
			msg := llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: fmt.Sprint(calls), Type: "function", Function: llm.FunctionCall{Name: "read", Arguments: fmt.Sprintf(`{"path":"missing-%d"}`, calls)}}}}
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": msg.ToolCalls[0].ID, "function": msg.ToolCalls[0].Function}}}}}})
			prompt := 10
			if calls == 3 {
				prompt = 6900
			}
			return fmt.Sprintf("data: %s\n\ndata: {\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n", b, prompt)
		}()
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	cfg.Client = &llm.Client{BaseURL: srv.URL, Model: "mock", APIKey: "fake"}
	_, _, err = cfg.Run(context.Background(), []llm.Message{task})
	if err == nil || cfg.LastTerminalReason != TerminalBlocked {
		t.Fatalf("err=%v reason=%s", err, cfg.LastTerminalReason)
	}
	saved, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	savedSawJob := false
	for _, m := range saved {
		if strings.Contains(m.Content, "UNIQUE-JOB-RESULT") {
			savedSawJob = true
		}
	}
	t.Logf("compacts=%d job delivered=%v summarizer saw job=%v saved history retains job=%v", compacts, job.Delivered(), summarySawJob, savedSawJob)
	if compacts != 1 {
		t.Fatalf("expected grace checkpoint; got %d", compacts)
	}
	if job.Delivered() && !summarySawJob && !savedSawJob {
		t.Error("checkpoint lost delivered job result without summarizing it")
	}
}
