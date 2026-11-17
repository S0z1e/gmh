package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"minimalharness/internal/verification"
)

func testEvidence(code int, note string) verification.Evidence {
	return verification.Evidence{Check: "tests", Command: "go test ./...", Cwd: "/workspace", Version: "version", ExitCode: &code,
		Started: time.Unix(10, 0).UTC(), Finished: time.Unix(11, 0).UTC(), Error: note}
}

func observe(t *testing.T, text string, limit int) Excerpt {
	t.Helper()
	out, err := Observe(text, limit)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func roundTrip(t *testing.T, s State) State {
	t.Helper()
	b, err := Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRepeatedCheckpointsRetainInstructionsAndLatestEvidence(t *testing.T) {
	first := Text{Source: "task:1", Content: "Keep the public API. Finish the tests before reporting completion."}
	decision := Text{Source: "assistant:2", Content: "The storage format remains version one; the migration is still pending."}
	failure := Failure{Fingerprint: "test-cmd", Source: "tool:3", Tool: "bash", Command: "go test ./...", Cwd: "/workspace",
		Output: observe(t, "compile failed\n"+strings.Repeat("detail\n", 1000)+"TAIL: missing symbol", 512)}
	job := Job{ID: "job1", Started: time.Unix(12, 0).UTC(), Command: "watch tests", Status: Running, OutputFile: "/output/job1"}
	artifact := Artifact{Kind: "image", Path: "screen.png", SHA256: strings.Repeat("a", 64), Source: "task:1", MediaType: "image/png"}
	s, err := Merge(State{}, Delta{ActiveTask: &first, Decisions: []Text{decision}, Failures: []Failure{failure}, Verification: []verification.Evidence{testEvidence(0, "")}, Jobs: []Job{job}, Artifacts: []Artifact{artifact}})
	if err != nil {
		t.Fatal(err)
	}
	s = roundTrip(t, s)
	second := Text{Source: "task:4", Content: "Continue, and keep the CLI compatible too."}
	newerFailure := testEvidence(1, "latest attempt failed")
	s, err = Merge(s, Delta{ActiveTask: &second, Verification: []verification.Evidence{newerFailure}, RefreshJobs: true})
	if err != nil {
		t.Fatal(err)
	}
	s = roundTrip(t, s)
	for range 4 {
		s, err = Merge(s, Delta{})
		if err != nil {
			t.Fatal(err)
		}
		s = roundTrip(t, s)
	}
	if s.ActiveTask == nil || *s.ActiveTask != second || !reflect.DeepEqual(s.Constraints, []Text{first, second}) || !reflect.DeepEqual(s.Decisions, []Text{decision}) {
		t.Fatalf("source instructions or pending decision lost: %+v", s)
	}
	if len(s.Verification) != 1 || *s.Verification[0].ExitCode != 1 || s.Verification[0].Error != newerFailure.Error || len(s.Failures) != 1 || !strings.HasSuffix(s.Failures[0].Output.Tail, "missing symbol") {
		t.Fatalf("latest failed test evidence lost: %+v", s)
	}
	if s.Jobs[0].Status != Unknown || s.Jobs[0].OutputFile != job.OutputFile || !reflect.DeepEqual(s.Artifacts, []Artifact{artifact}) {
		t.Fatalf("restored job/artifact identity lost: %+v", s)
	}
	// An unrelated success does not clear failures. The caller must name the
	// exact observed successful rerun; verification still retains its failure.
	s, err = Merge(s, Delta{ResolvedFailures: []string{"other-command"}})
	if err != nil || len(s.Failures) != 1 {
		t.Fatalf("unrelated resolution: %+v %v", s.Failures, err)
	}
	s, err = Merge(s, Delta{ResolvedFailures: []string{failure.Fingerprint}})
	if err != nil || len(s.Failures) != 0 || *s.Verification[0].ExitCode != 1 {
		t.Fatalf("exact resolution changed verification: %+v %v", s, err)
	}
}

func TestMergeOwnsInputsAndRejectsChangedSources(t *testing.T) {
	task := Text{Source: "task", Content: "keep all constraints"}
	evidence := testEvidence(1, "failed")
	s, err := Merge(State{}, Delta{ActiveTask: &task, Verification: []verification.Evidence{evidence}})
	if err != nil {
		t.Fatal(err)
	}
	task.Content = "mutated"
	*evidence.ExitCode = 0
	if s.ActiveTask.Content != "keep all constraints" || *s.Verification[0].ExitCode != 1 {
		t.Fatal("incoming pointer was retained")
	}
	before, _ := Encode(s)
	if _, err := Merge(s, Delta{Constraints: []Text{{Source: "task", Content: "different"}}}); err == nil {
		t.Fatal("changed source accepted")
	}
	after, _ := Encode(s)
	if string(before) != string(after) {
		t.Fatal("failed merge changed previous state")
	}
	out, err := Merge(s, Delta{})
	if err != nil {
		t.Fatal(err)
	}
	out.ActiveTask.Content = "changed output"
	*out.Verification[0].ExitCode = 0
	if s.ActiveTask.Content != "keep all constraints" || *s.Verification[0].ExitCode != 1 {
		t.Fatal("output aliases previous state")
	}
}

func TestJobIncarnationsAndPendingResults(t *testing.T) {
	args := sha256.Sum256([]byte(`{"path":"source"}`))
	call := PendingCall{ID: "invocation-a", Tool: "read", ArgumentsSHA256: hex.EncodeToString(args[:])}
	old := Job{ID: "same-display-id", Started: time.Unix(20, 0).UTC(), Command: "old-command", Status: Running}
	s, err := Merge(State{}, Delta{Pending: []PendingCall{call}, Jobs: []Job{old}})
	if err != nil {
		t.Fatal(err)
	}
	changed := call
	changed.Tool = "write"
	if _, err := Merge(s, Delta{Pending: []PendingCall{changed}}); err == nil {
		t.Fatal("pending call identity was silently rewritten")
	}
	current := Job{ID: old.ID, Started: time.Unix(30, 0).UTC(), Command: "new-command", Status: Running}
	s, err = Merge(s, Delta{Jobs: []Job{current}, RefreshJobs: true, ResolvedCalls: []string{call.ID}})
	if err != nil || len(s.Jobs) != 2 || s.Jobs[0].Status != Unknown || s.Jobs[1].Status != Running || len(s.Pending) != 0 {
		t.Fatalf("incarnations merged or unresolved call retained: %+v %v", s, err)
	}
	code := 2
	current.Status, current.ExitCode, current.Error, current.Delivered = Done, &code, "failed", true
	s, err = Merge(s, Delta{Jobs: []Job{current}, RefreshJobs: true})
	if err != nil || len(s.Jobs) != 2 || s.Jobs[1].Status != Done || !s.Jobs[1].Delivered {
		t.Fatalf("finished result lost: %+v %v", s, err)
	}
}

func TestExcerptKeepsUTF8HeadTailAndDigest(t *testing.T) {
	for _, input := range []string{"", "short", strings.Repeat("中😀", 40) + "尾端錯誤"} {
		e := observe(t, input, 32)
		if !utf8.ValidString(e.Head) || !utf8.ValidString(e.Tail) || len(e.Head)+len(e.Tail) > 32 || e.Bytes != len(input) || !strings.HasPrefix(input, e.Head) || !strings.HasSuffix(input, e.Tail) {
			t.Fatalf("invalid excerpt: %+v", e)
		}
		sum := sha256.Sum256([]byte(input))
		if e.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatal("full digest lost")
		}
		if _, err := Merge(State{}, Delta{Failures: []Failure{{Fingerprint: "command", Source: "result", Tool: "bash", Output: e}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Observe(string([]byte{0xff}), 32); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestCheckpointRejectsInvalidOrOversizedState(t *testing.T) {
	task := Text{Source: "task", Content: "original"}
	s, err := Merge(State{}, Delta{ActiveTask: &task})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*State){
		"version":          func(s *State) { s.Version++ },
		"active source":    func(s *State) { s.ActiveTask.Source = "missing" },
		"duplicate source": func(s *State) { s.Decisions = append(s.Decisions, *s.ActiveTask) },
		"oversized text":   func(s *State) { s.Decisions = []Text{{Source: "large", Content: strings.Repeat("x", MaxTextBytes+1)}} },
		"invalid text":     func(s *State) { s.Decisions = []Text{{Source: "bad", Content: string([]byte{0xff})}} },
		"job claims result": func(s *State) {
			code := 0
			s.Jobs = []Job{{ID: "job", Started: time.Unix(1, 0), Command: "cmd", Status: Running, ExitCode: &code}}
		},
		"verification lacks result": func(s *State) {
			e := testEvidence(0, "")
			e.ExitCode = nil
			s.Verification = []verification.Evidence{e}
		},
		"corrupt excerpt": func(s *State) {
			e := observe(t, "error", 32)
			e.Head = "other"
			s.Failures = []Failure{{Fingerprint: "f", Source: "s", Tool: "bash", Output: e}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			copy := roundTrip(t, s)
			mutate(&copy)
			if _, err := Encode(copy); err == nil {
				t.Fatal("invalid state accepted")
			}
		})
	}
	// JSON escaping counts against the actual wire bound, too.
	var escaped []Text
	for i := 0; i < 5; i++ {
		escaped = append(escaped, Text{Source: string(rune('a' + i)), Content: strings.Repeat("<", 20000)})
	}
	if _, err := Merge(s, Delta{Decisions: escaped}); err == nil {
		t.Fatal("encoded-size overflow silently accepted")
	}
	many := make([]PendingCall, MaxEntries+1)
	for i := range many {
		many[i] = PendingCall{ID: strings.Repeat("x", i+1), Tool: "read", ArgumentsSHA256: strings.Repeat("a", 64)}
	}
	if _, err := Merge(s, Delta{Pending: many}); err == nil {
		t.Fatal("entry overflow accepted")
	}
	if s.ActiveTask.Content != "original" {
		t.Fatal("overflow changed original state")
	}
}

func TestDecodeRejectsUnsupportedAndMalformedRecords(t *testing.T) {
	for _, input := range []string{`null`, `{}`, `{"version":2}`, `{"version":1,"new_field":1}`, `{"version":1} {"version":1}`, strings.Repeat(" ", MaxBytes+1)} {
		if _, err := Decode([]byte(input)); err == nil {
			t.Fatalf("accepted malformed/unknown checkpoint: %.100s", input)
		}
	}
	if _, err := Decode([]byte("{\"version\":1,\"constraints\":[{\"source\":\"s\",\"content\":\"\xff\"}]}")); err == nil {
		t.Fatal("invalid source bytes silently replaced on decode")
	}
	s, err := Merge(State{}, Delta{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(b); err != nil {
		t.Fatal(err)
	}
}
