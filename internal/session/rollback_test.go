package session

import (
	"testing"

	"minimalharness/internal/llm"
)

func TestIsTurnStart(t *testing.T) {
	cases := []struct {
		m    llm.Message
		want bool
	}{
		{llm.Message{Role: "user", Content: "do the thing"}, true},
		{llm.Message{Role: "user", Content: "do it", Images: []llm.Image{{Path: "x.png"}}}, false},
		{llm.Message{Role: "assistant", Content: "done"}, false},
		{llm.Message{Role: "tool", Content: "out"}, false},
		{llm.Message{Role: "user", Content: "Skill demo:\nbody"}, false},
		{llm.Message{Role: "user", Content: "Background job j1 finished"}, false},
		{llm.Message{Role: "user", Content: "Previous conversation summary:\n..."}, false},
		{llm.Message{Role: "user", Content: "  "}, false},
	}
	for _, tc := range cases {
		if got := IsTurnStart(tc.m); got != tc.want {
			t.Errorf("IsTurnStart(%+v) = %v, want %v", tc.m, got, tc.want)
		}
	}
}

func twoTurns() []llm.Message {
	return []llm.Message{
		{Role: "user", Content: "first task"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "Skill demo:\nbody"},
		{Role: "user", Content: "second task"},
		{Role: "assistant", Content: "second answer"},
		{Role: "tool", Content: "tool out", ToolCallID: "c1"},
		{Role: "assistant", Content: "second done"},
	}
}

func TestRollbackDropsLastTurn(t *testing.T) {
	kept, dropped := Rollback(twoTurns(), 1)
	if dropped != 4 {
		t.Fatalf("dropped = %d, want 4 (second task + 3 replies)", dropped)
	}
	if len(kept) != 3 {
		t.Fatalf("kept %d messages, want 3", len(kept))
	}
	if kept[2].Content != "Skill demo:\nbody" {
		t.Fatalf("kept tail = %q, want the skill message of the first turn", kept[2].Content)
	}
}

func TestRollbackAllTurnsKeepsPrefix(t *testing.T) {
	kept, dropped := Rollback(twoTurns(), 5)
	if dropped != 7 || len(kept) != 0 {
		t.Fatalf("dropped = %d, kept = %d, want 7 / 0", dropped, len(kept))
	}
}

func TestRollbackZeroAndNoTurns(t *testing.T) {
	msgs := twoTurns()
	if kept, dropped := Rollback(msgs, 0); dropped != 0 || len(kept) != len(msgs) {
		t.Fatalf("n=0: dropped=%d kept=%d", dropped, len(kept))
	}
	if _, dropped := Rollback([]llm.Message{{Role: "assistant", Content: "x"}}, 1); dropped != 0 {
		t.Fatalf("no turns: dropped=%d, want 0", dropped)
	}
}

func TestStoreRewriteRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := New("rollback-test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, m := range twoTurns() {
		if err := s.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	kept, _ := Rollback(twoTurns(), 1)
	if err := s.Rewrite(kept); err != nil {
		t.Fatal(err)
	}
	// Appends still work after a rewrite.
	if err := s.Append(llm.Message{Role: "user", Content: "third task"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load("rollback-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 4 || loaded[3].Content != "third task" {
		t.Fatalf("loaded = %+v, want 3 kept + third task", loaded)
	}
}
