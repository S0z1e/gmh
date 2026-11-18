package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/checkpoint"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"minimalharness/internal/verification"
)

func preservedState(t *testing.T, history []llm.Message) checkpoint.State {
	t.Helper()
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Kind == "checkpoint" {
			state, err := checkpoint.Decode([]byte(history[i].Content))
			if err != nil {
				t.Fatal(err)
			}
			return state
		}
	}
	t.Fatal("missing structured checkpoint")
	return checkpoint.State{}
}

func sourceHas(items []checkpoint.Text, content string) bool {
	for _, item := range items {
		if item.Content == content {
			return true
		}
	}
	return false
}

func checkpointBatch(id, args, output string, failed bool) []llm.Message {
	call := llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: "bash", Arguments: args}}
	inv := llm.ToolInvocation{ID: "inv-" + id, CallIndex: 0, ToolCallID: id, ToolName: "bash", OriginalArguments: args, Arguments: args}
	var runErr error
	if failed {
		runErr = errors.New("exit status 1")
	}
	return []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{call}, ToolInvocations: []llm.ToolInvocation{inv}},
		{Role: "tool", ToolCallID: id, InvocationID: inv.ID, Content: output, ToolError: failed, Observation: executionObservation("bash", args, CanonicalToolKey("bash", args), output, "", runErr, nil)},
	}
}

func TestStructuredCheckpointRepeatedResumeAndRollback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("structured-repeat")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := "Keep the public API; never publish credentials."
	latest := "Fix cancellation; keep that original constraint."
	decision := "Use context cancellation rather than a global flag."
	args := `{"command":"go test ./...","cwd":"."}`
	output := "HEAD FAILURE\n" + strings.Repeat("middle\n", 10000) + "\nTAIL ASSERTION"
	history := []llm.Message{{Role: "user", Kind: "task", Content: first}, {Role: "assistant", Content: "Keep the existing interface."}}
	now := time.Now().UTC()
	exit := 0
	evidence := verification.Evidence{Check: "test", Command: "go test ./...", Cwd: t.TempDir(), Version: "old-version", ExitCode: &exit, Started: now, Finished: now}
	for _, code := range []int{0, 1} {
		evidence.ExitCode = &code
		if code != 0 {
			evidence.Error = "latest check failed"
		}
		data, _ := json.Marshal(evidence)
		history = append(history, llm.Message{Role: "user", Kind: "verification", Content: "[harness verification evidence]\n" + string(data)})
	}
	history = append(history, llm.Message{Role: "user", Kind: "task", Content: latest, Images: []llm.Image{{Path: "screen.png", MediaType: "image/png", Data: "aGVsbG8="}}})
	history = append(history, checkpointBatch("failed", args, output, true)...)
	running := checkpoint.Job{ID: "bg-1", Started: now, InvocationID: "job-inv", Command: "build", OutputFile: "build-output.txt", Status: checkpoint.Running}
	history = append(history, llm.Message{Role: "user", Kind: "job", Content: "build is still running", Observation: &llm.ToolObservation{Tool: "bash", Jobs: []checkpoint.Job{running}}})
	for _, m := range history {
		if err := store.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{Session: store, ContextLimit: DefaultContextLimit, CompactHistory: func(_ context.Context, input []llm.Message) (string, llm.Usage, error) {
		state := preservedState(t, input)
		if !sourceHas(state.Constraints, first) || state.ActiveTask == nil || state.ActiveTask.Content != latest {
			t.Fatal("summary input omitted source facts")
		}
		return "Model claims all tests passed and forgets the constraints.", llm.Usage{}, nil
	}}
	out, _, err := cfg.Compact(context.Background(), history)
	if err != nil {
		t.Fatal(err)
	}
	state := preservedState(t, out)
	if len(state.Constraints) != 2 || state.ActiveTask == nil || state.ActiveTask.Content != latest || len(state.Verification) != 1 || state.Verification[0].ExitCode == nil || *state.Verification[0].ExitCode != 1 || state.Verification[0].Version != "old-version" {
		t.Fatalf("lost source/evidence: %+v", state)
	}
	if len(state.Failures) != 1 || !strings.Contains(state.Failures[0].Output.Head, "HEAD FAILURE") || !strings.Contains(state.Failures[0].Output.Tail, "TAIL ASSERTION") {
		t.Fatalf("lost failure ends: %+v", state.Failures)
	}
	if len(state.Jobs) != 1 || state.Jobs[0].Status != checkpoint.Unknown {
		t.Fatalf("missing registry claimed running: %+v", state.Jobs)
	}
	foundImage := false
	for _, a := range state.Artifacts {
		if a.Kind == "image" && a.Path == "screen.png" && a.SHA256 == factDigest("hello") {
			foundImage = true
		}
	}
	if !foundImage || out[1].Images[0].Data != "aGVsbG8=" {
		t.Fatal("lost immutable image reference/retained task bytes")
	}
	loaded, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	loaded = append(loaded, llm.Message{Role: "assistant", Content: decision})
	if err := store.Append(loaded[len(loaded)-1]); err != nil {
		t.Fatal(err)
	}
	for _, m := range checkpointBatch("rerun", args, "passed", false) {
		loaded = append(loaded, m)
		if err := store.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	out, _, err = cfg.Compact(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	state = preservedState(t, resumed)
	if len(state.Constraints) != 2 || !sourceHas(state.Decisions, decision) || len(state.Failures) != 0 || len(state.Pending) != 0 || *state.Verification[0].ExitCode != 1 {
		t.Fatalf("repeat lost facts or invented current verification: %+v", state)
	}
	archives := 0
	for _, a := range state.Artifacts {
		if a.Kind != "transcript" {
			continue
		}
		archives++
		data, err := os.ReadFile(a.Path)
		if err != nil || len(data) == 0 {
			t.Fatalf("missing referenced archive %s: %v", a.Path, err)
		}
	}
	if archives != 2 {
		t.Fatalf("transcript references=%d", archives)
	}
	kept, dropped := session.Rollback(resumed, 1)
	if dropped == 0 || len(kept) != 1 {
		t.Fatalf("wrong rollback boundary: %+v", kept)
	}
	if err := store.Rewrite(kept); err != nil {
		t.Fatal(err)
	}
	rolled, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	prior := preservedState(t, rolled)
	if !sourceHas(prior.Constraints, first) || sourceHas(prior.Constraints, latest) || sourceHas(prior.Decisions, decision) {
		t.Fatalf("rolled-back instructions resurrected: %+v", prior)
	}
}

func TestStructuredCheckpointUsesDurableSourcesOverHookRewrite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("durable-facts")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	original := llm.Message{Role: "user", Kind: "task", Content: "Do not publish."}
	if err := store.Append(original); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Session: store, CompactHistory: func(_ context.Context, input []llm.Message) (string, llm.Usage, error) {
		state := preservedState(t, input)
		if !sourceHas(state.Constraints, original.Content) || sourceHas(state.Constraints, "Publish now.") {
			t.Fatal("transient hook replaced durable source facts")
		}
		return "summary", llm.Usage{}, nil
	}}
	out, _, err := cfg.Compact(context.Background(), []llm.Message{{Role: "user", Kind: "task", Content: "Publish now."}})
	if err != nil || out[0].Content != original.Content {
		t.Fatalf("durable task lost: %+v %v", out, err)
	}
}

func TestStructuredCheckpointUnfitAndOverflowPreserveSession(t *testing.T) {
	for _, size := range []int{10000, checkpoint.MaxTextBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			store, err := session.New("unfit-facts")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			task := llm.Message{Role: "user", Kind: "task", Content: strings.Repeat("x", size)}
			if err := store.Append(task); err != nil {
				t.Fatal(err)
			}
			callbacks := 0
			cfg := &Config{Session: store, ContextLimit: 1000, CompactHistory: func(context.Context, []llm.Message) (string, llm.Usage, error) {
				callbacks++
				return "summary", llm.Usage{}, nil
			}}
			if _, _, err := cfg.Compact(context.Background(), []llm.Message{task}); err == nil {
				t.Fatal("unfit facts committed")
			}
			loaded, err := session.Load(store.ID)
			if err != nil || !reflect.DeepEqual(loaded, []llm.Message{task}) {
				t.Fatalf("failed compact changed session: %+v %v", loaded, err)
			}
			archives, err := filepath.Glob(filepath.Join(session.Dir(), "*-pre-compact-*.jsonl"))
			if err != nil || len(archives) != 0 {
				t.Fatal("unfit checkpoint archived before preflight")
			}
			if size > checkpoint.MaxTextBytes && callbacks != 0 {
				t.Fatal("oversized source reached summarizer")
			}
		})
	}
}

func TestStructuredCheckpointPendingAndFailureResolution(t *testing.T) {
	cfg := &Config{}
	history := checkpointBatch("bad", `{"command":"test"}`, "failed", true)
	history = append(history, checkpointBatch("other", `{"command":"true"}`, "ok", false)...)
	pending := llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "pending", Function: llm.FunctionCall{Name: "write", Arguments: `{"path":"x","content":"y"}`}}}}
	history = append(history, pending)
	state, _, _, err := cfg.compactState(history)
	if err != nil || len(state.Failures) != 1 || len(state.Pending) != 1 {
		t.Fatalf("unrelated success erased unfinished work: %+v %v", state, err)
	}
	marker, err := checkpointMessage(state)
	if err != nil {
		t.Fatal(err)
	}
	replay := append([]llm.Message{marker}, checkpointBatch("good", `{"command":"test"}`, "ok", false)...)
	state, _, _, err = cfg.compactState(replay)
	if err != nil || len(state.Failures) != 0 || len(state.Pending) != 1 {
		t.Fatalf("rerun did not resolve only matching failure: %+v %v", state, err)
	}
}

func TestPruneCheckpointRetainsFailedOutput(t *testing.T) {
	history := []llm.Message{{Role: "user", Kind: "task", Content: "task"}}
	for i := 0; i < 8; i++ {
		history = appendFixtureBatch(history, i, 16000)
	}
	history[2].ToolError = true
	cfg := &Config{ContextLimit: 40000}
	out, ok := cfg.pruneCheckpoint(history, cfg.Tools)
	if !ok || out[2].Content != history[2].Content || !out[2].ToolError {
		t.Fatal("pruning discarded unresolved failed observation")
	}
}

func TestCompletedJobCheckpointCapturesActualFailureBeforeDisplayHooks(t *testing.T) {
	exec := &tools.Executor{Root: t.TempDir(), OutputDir: t.TempDir()}
	args := `{"command":"printf 'HEAD'; printf 'TAIL'; exit 7","background":true}`
	key := CanonicalToolKey("bash", args)
	job, err := exec.BashBackground(tools.WithFailureKey(tools.WithInvocation(context.Background(), "inv", "call"), key), tools.BashArgs{Command: "printf 'HEAD'; printf 'TAIL'; exit 7", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.Done():
	case <-time.After(3 * time.Second):
		job.Cancel()
		t.Fatal("job did not finish")
	}
	res, _ := job.Result()
	msg := completedJobMessage(job, res, "display hook claims success")
	cfg := &Config{Exec: exec}
	state, _, _, err := cfg.compactState([]llm.Message{msg})
	if err != nil || len(state.Failures) != 1 || !strings.Contains(state.Failures[0].Output.Head, "HEADTAIL") || len(state.Jobs) != 1 || state.Jobs[0].Status != checkpoint.Done || !state.Jobs[0].Delivered || state.Jobs[0].ExitCode == nil || *state.Jobs[0].ExitCode != 7 {
		t.Fatalf("lost actual background outcome: %+v %v", state, err)
	}
}

func TestStructuredCheckpointImageOnlyTaskDoesNotReuseOldActiveText(t *testing.T) {
	history := []llm.Message{{Role: "user", Kind: "task", Content: "old task"}, {Role: "user", Kind: "task", Images: []llm.Image{{Path: "image.png", MediaType: "image/png", Data: "aGVsbG8="}}}}
	cfg := &Config{CompactHistory: func(context.Context, []llm.Message) (string, llm.Usage, error) { return "summary", llm.Usage{}, nil }}
	out, _, err := cfg.Compact(context.Background(), history)
	if err != nil {
		t.Fatal(err)
	}
	state := preservedState(t, out)
	if state.ActiveTask != nil || len(state.Constraints) != 1 || len(state.Artifacts) != 1 || len(out[1].Images) != 1 || out[1].Images[0].Data != "aGVsbG8=" {
		t.Fatalf("wrong active image task: %+v, %+v", state, out)
	}
}

func TestPruneCheckpointRetainsUserAnswer(t *testing.T) {
	history := []llm.Message{{Role: "user", Kind: "task", Content: "task"}}
	for i := 0; i < 8; i++ {
		history = appendFixtureBatch(history, i, 16000)
	}
	history[1].ToolCalls[0].Function.Name = "ask_user"
	cfg := &Config{ContextLimit: 40000}
	out, ok := cfg.pruneCheckpoint(history, cfg.Tools)
	if !ok || out[2].Content != history[2].Content {
		t.Fatal("pruning discarded a user answer")
	}
	state, _, _, err := cfg.compactState(out)
	if err != nil || !sourceHas(state.Decisions, history[2].Content) {
		t.Fatalf("answer not preserved as a source: %v", err)
	}
	for _, source := range state.Decisions {
		if source.Content == history[2].Content && !strings.HasPrefix(source.Source, "unattributed_tool/") {
			t.Fatal("legacy display text was attributed to a human without evidence")
		}
	}
}

func TestStructuredCheckpointRejectsMalformedResumeBeforeProvider(t *testing.T) {
	cfg := &Config{}
	_, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Kind: "checkpoint", Content: `{"version":1,"constraints":[] ,"con\u017ftraints":[]}`}})
	if err == nil || !strings.Contains(err.Error(), "invalid historical checkpoint") {
		t.Fatalf("malformed facts reached provider: %v", err)
	}
}

func TestStructuredCheckpointLegacyJobsKeepDistinctFailures(t *testing.T) {
	exec := &tools.Executor{Root: t.TempDir()}
	var history []llm.Message
	for i := 0; i < 2; i++ {
		job, err := exec.BashBackground(context.Background(), tools.BashArgs{Command: "exit 7", Background: true})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-job.Done():
		case <-time.After(3 * time.Second):
			job.Cancel()
			t.Fatal("job did not finish")
		}
		res, _ := job.Result()
		history = append(history, completedJobMessage(job, res, "failed"))
	}
	state, _, _, err := (&Config{Exec: exec}).compactState(history)
	if err != nil || len(state.Failures) != 2 || len(state.Jobs) != 2 {
		t.Fatalf("legacy job identity collision: %+v %v", state, err)
	}
}

func TestCheckpointOriginalUserAnswerSurvivesDisplayRewrite(t *testing.T) {
	args := `{"questions":[{"question":"API?","options":["keep","change"]}]}`
	call := llm.ToolCall{ID: "answer", Type: "function", Function: llm.FunctionCall{Name: "ask_user", Arguments: args}}
	obs := executionObservation("ask_user", args, CanonicalToolKey("ask_user", args), "Keep the public API.", "", nil, nil)
	answer := "Keep the public API."
	obs.Answer = &answer
	history := []llm.Message{{Role: "assistant", ToolCalls: []llm.ToolCall{call}}, {Role: "tool", ToolCallID: "answer", Content: "Display hook says change the API.", Observation: obs}}
	state, _, _, err := (&Config{}).compactState(history)
	if err != nil || !sourceHas(state.Decisions, "Keep the public API.") || sourceHas(state.Decisions, "Display hook says change the API.") {
		t.Fatalf("human answer replaced by presentation: %+v %v", state.Decisions, err)
	}
}

func TestCheckpointPreflightReservesAnthropicDefaultOutput(t *testing.T) {
	cfg := &Config{ContextLimit: 20000, Client: &llm.Client{API: "anthropic"}, CompactHistory: func(context.Context, []llm.Message) (string, llm.Usage, error) { return "summary", llm.Usage{}, nil }}
	history := []llm.Message{{Role: "system", Content: strings.Repeat("x", 20000)}, {Role: "user", Kind: "task", Content: "work"}}
	if _, _, err := cfg.Compact(context.Background(), history); err == nil {
		t.Fatal("committed checkpoint that exceeds actual input reservation")
	}
}
func TestCheckpointManualPreflightIncludesConfiguredSystem(t *testing.T) {
	cfg := &Config{ContextLimit: 1000, System: strings.Repeat("x", 4000), CompactHistory: func(context.Context, []llm.Message) (string, llm.Usage, error) { return "summary", llm.Usage{}, nil }}
	history := []llm.Message{{Role: "user", Kind: "task", Content: "work"}}
	if _, _, err := cfg.Compact(context.Background(), history); err == nil {
		t.Fatal("manual compaction ignored system in next request")
	}
}

func TestCheckpointDoesNotAttributeQuestionFallbackToHuman(t *testing.T) {
	args := `{"questions":[{"question":"Which?","options":[{"label":"A"},{"label":"B"}]}]}`
	for _, name := range []string{"headless", "cancelled", "empty"} {
		t.Run(name, func(t *testing.T) {
			cfg := &Config{}
			if name != "headless" {
				cfg.AskQuestion = func(QuestionRequest) (QuestionAnswers, error) {
					if name == "cancelled" {
						return QuestionAnswers{}, context.Canceled
					}
					return QuestionAnswers{}, nil
				}
			}
			text, answer, err := cfg.askUser(context.Background(), args)
			if err != nil {
				t.Fatal(err)
			}
			call := llm.ToolCall{ID: "ask", Function: llm.FunctionCall{Name: "ask_user", Arguments: args}}
			observation := executionObservation("ask_user", args, CanonicalToolKey("ask_user", args), text, "", nil, nil)
			observation.Answer = answer
			state, _, _, err := cfg.compactState([]llm.Message{{Role: "assistant", ToolCalls: []llm.ToolCall{call}}, {Role: "tool", ToolCallID: "ask", Content: text, Observation: observation}})
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range state.Decisions {
				if strings.HasPrefix(record.Source, "user_answer/") {
					t.Fatalf("generated fallback attributed to human: %+v", record)
				}
			}
		})
	}
}

func TestCheckpointHumanReplyKeepsOriginalWhitespaceAndProvenance(t *testing.T) {
	args := `{"questions":[{"question":"Which?","options":[{"label":"A"},{"label":"B"}]}]}`
	original := "\n    preserve this indentation\n"
	cfg := &Config{AskQuestion: func(QuestionRequest) (QuestionAnswers, error) {
		return QuestionAnswers{Answers: []QuestionAnswer{{Question: 0, Custom: original}}}, nil
	}}
	text, answer, err := cfg.askUser(context.Background(), args)
	if err != nil || answer == nil {
		t.Fatalf("missing actual answer: %v", err)
	}
	var decoded struct {
		Answers []QuestionAnswer `json:"answers"`
	}
	if err := json.Unmarshal([]byte(*answer), &decoded); err != nil || len(decoded.Answers) != 1 || decoded.Answers[0].Custom != original {
		t.Fatalf("original reply changed: %s, %v", *answer, err)
	}
	obs := executionObservation("ask_user", args, CanonicalToolKey("ask_user", args), text, "", nil, nil)
	obs.Answer = answer
	for _, display := range []string{"", "plugin display"} {
		state, _, _, err := cfg.compactState([]llm.Message{{Role: "tool", Content: display, Observation: obs}})
		if err != nil || len(state.Decisions) != 1 || state.Decisions[0].Content != *answer || !strings.HasPrefix(state.Decisions[0].Source, "user_answer/") {
			t.Fatalf("actual reply lost behind display: %+v %v", state.Decisions, err)
		}
	}
}
