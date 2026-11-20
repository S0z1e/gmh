package main

import (
	"bytes"
	"os"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
)

func TestViewSessionWhileWriterOpen(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := session.New("view")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Append(llm.Message{Role: "user", Content: "view me"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := viewSession(&out, s.ID); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("view me")) {
		t.Fatal(out.String())
	}
	after, err := os.ReadFile(s.Path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("viewer changed transcript")
	}
	if err := s.Append(llm.Message{Role: "user", Content: "still owned"}); err != nil {
		t.Fatal(err)
	}
}
