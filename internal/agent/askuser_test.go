package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
)

func TestParseAskUserArgs(t *testing.T) {
	good := `{"questions":[{"question":"Which DB?","header":"Storage","options":[{"label":"SQLite","description":"local file"},{"label":"Postgres"}]},{"question":"Auth?","options":[{"label":"OAuth"},{"label":"API key"}]}]}`
	req, err := ParseAskUserArgs(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Questions) != 2 || req.Questions[0].Header != "Storage" {
		t.Fatalf("parsed request = %+v", req)
	}
	for _, bad := range []string{
		`{"questions":[]}`,
		`{"questions":[{"question":"Q?","options":[{"label":"Only"}]}]}`,
		`{"questions":[{"question":"Q?","options":[{"label":"A"},{"label":"A"}]}]}`,
		`{"questions":[{"question":"","options":[{"label":"A"},{"label":"B"}]}]}`,
		`not json`,
	} {
		if _, err := ParseAskUserArgs(bad); err == nil {
			t.Errorf("ParseAskUserArgs(%s) = no error, want one", bad)
		}
	}
}

func TestFormatAnswers(t *testing.T) {
	req, _ := ParseAskUserArgs(`{"questions":[{"question":"Which DB?","options":[{"label":"SQLite"},{"label":"Postgres"}]}]}`)
	got := FormatAnswers(req, QuestionAnswers{Answers: []QuestionAnswer{{Question: 0, Selected: []string{"Postgres"}, Custom: "only if hosted"}}})
	if !strings.Contains(got, "Postgres") || !strings.Contains(got, "only if hosted") {
		t.Fatalf("formatted answers = %q", got)
	}
}

// The ask_user tool reaches the AskQuestion seam and its answers
// come back as the tool observation the model sees next.
func TestRunAskUserTool(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		calls++
		if calls == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"ask_user\",\"arguments\":\"{\\\"questions\\\":[{\\\"question\\\":\\\"Which DB?\\\",\\\"options\\\":[{\\\"label\\\":\\\"SQLite\\\"},{\\\"label\\\":\\\"Postgres\\\"}]}]}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"using Postgres\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3,\"total_tokens\":23}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var asked []Question
	var events []Event
	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: t.TempDir()},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 5,
		OnEvent:  func(e Event) { events = append(events, e) },
		AskQuestion: func(req QuestionRequest) (QuestionAnswers, error) {
			asked = req.Questions
			return QuestionAnswers{Answers: []QuestionAnswer{{Question: 0, Selected: []string{"Postgres"}}}}, nil
		},
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "pick a DB"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "using Postgres" {
		t.Fatalf("final text = %q", text)
	}
	if len(asked) != 1 || asked[0].Question != "Which DB?" {
		t.Fatalf("asked = %+v", asked)
	}
	// The tool observation carried the answer.
	found := false
	for _, e := range events {
		if e.Kind == EventToolEnd && strings.Contains(e.ToolOutput, "Postgres") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no tool_end carried the answer: %+v", events)
	}
}

// Headless (no AskQuestion): the model is told to proceed on its
// best judgement, and the turn continues instead of blocking.
func TestRunAskUserHeadlessProceeds(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		calls++
		if calls == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"ask_user\",\"arguments\":\"{\\\"questions\\\":[{\\\"question\\\":\\\"Which DB?\\\",\\\"options\\\":[{\\\"label\\\":\\\"SQLite\\\"},{\\\"label\\\":\\\"Postgres\\\"}]}]}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"guessed SQLite\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3,\"total_tokens\":23}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: t.TempDir()},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 5,
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "pick a DB"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "guessed SQLite" {
		t.Fatalf("final text = %q", text)
	}
}
