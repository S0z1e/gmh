package main

import (
	"context"
	"errors"
	"io"
	"minimalharness/internal/input"
	"strings"
	"testing"

	"minimalharness/internal/agent"
)

// The approval answer must come from the same scanner the REPL
// loop uses: the scanner buffers piped stdin ahead, so a fresh
// reader on the same stream would see EOF after the first task
// line. One scanner reads the task, the approval, then the next
// task, exactly like a piped REPL session.
func TestAskFromScannerSharesREPLInput(t *testing.T) {
	scanner := input.New(strings.NewReader("Write approved.txt\ny\n/quit\n"))
	defer scanner.Close()
	line, err := scanner.Read(context.Background())
	if err != nil || line != "Write approved.txt" {
		t.Fatalf("first task line = %q, error=%v", line, err)
	}
	ask := askFromScanner(scanner)
	ok, err := ask(agent.ApprovalRequest{ToolName: "write", Arguments: `{}`})
	if err != nil {
		t.Fatalf("ask returned error: %v", err)
	}
	if !ok {
		t.Fatal("ask(\"y\") = false, want true")
	}
	line, err = scanner.Read(context.Background())
	if err != nil || line != "/quit" {
		t.Fatalf("next task line = %q, want /quit", line)
	}
}

func TestAskFromScannerAnswers(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"  y  \n", true},
		{"n\n", false},
		{"no\n", false},
		{"\n", false},
		{"maybe\n", false},
	} {
		scanner := input.New(strings.NewReader(tc.in))
		t.Cleanup(scanner.Close)
		ok, err := askFromScanner(scanner)(agent.ApprovalRequest{ToolName: "write", Arguments: `{}`})
		if err != nil {
			t.Errorf("ask(%q) error: %v", tc.in, err)
			continue
		}
		if ok != tc.want {
			t.Errorf("ask(%q) = %v, want %v", tc.in, ok, tc.want)
		}
	}
}

func TestAskFromScannerEOFAndError(t *testing.T) {
	// Clean EOF (a pipe that ends mid-approval) is an error, like
	// the loop's default ask returning the ReadString error.
	scanner := input.New(strings.NewReader(""))
	t.Cleanup(scanner.Close)
	if _, err := askFromScanner(scanner)(agent.ApprovalRequest{ToolName: "write", Arguments: `{}`}); !errors.Is(err, io.EOF) {
		t.Fatalf("ask on EOF = %v, want io.EOF", err)
	}
	// A read error surfaces instead of being swallowed.
	boom := errors.New("boom")
	scanner = input.New(errReader{err: boom})
	t.Cleanup(scanner.Close)
	if _, err := askFromScanner(scanner)(agent.ApprovalRequest{ToolName: "write", Arguments: `{}`}); !errors.Is(err, boom) {
		t.Fatalf("ask on read error = %v, want boom", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
