package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDispatchWriteCarriesContentDetail(t *testing.T) {
	root := t.TempDir()
	ex := &Executor{Root: root}
	res, err := ex.Dispatch(context.Background(), "write", `{"path":"note.txt","content":"hello\nworld\n"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "wrote note.txt" {
		t.Fatalf("text = %q", res.Text)
	}
	if res.Detail != "hello\nworld\n" {
		t.Fatalf("detail = %q", res.Detail)
	}
	// A failed write carries no detail: nothing landed.
	res2, err2 := ex.Dispatch(context.Background(), "write", `{"path":"note.txt","content":"again"}`)
	if err2 == nil || res2.Detail != "" {
		t.Fatalf("failed write: err=%v detail=%q", err2, res2.Detail)
	}
}

func TestDispatchEditCarriesDiffDetail(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := &Executor{Root: root}
	res, err := ex.Dispatch(context.Background(), "edit", `{"path":"x.txt","oldText":"alpha","newText":"gamma"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "edited x.txt" {
		t.Fatalf("text = %q", res.Text)
	}
	if res.Detail != "− alpha\n+ gamma" {
		t.Fatalf("detail = %q", res.Detail)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "x.txt")); string(got) != "gamma\nbeta\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestCapDetailTruncates(t *testing.T) {
	big := strings.Repeat("x", maxDetailChars+100)
	got := capDetail(big)
	if len([]rune(got)) > maxDetailChars+100 || !strings.Contains(got, "truncated") {
		t.Fatalf("cap failed: %d runes, marker missing", len([]rune(got)))
	}
	if got := capDetail("short"); got != "short" {
		t.Fatalf("short detail changed: %q", got)
	}
}
