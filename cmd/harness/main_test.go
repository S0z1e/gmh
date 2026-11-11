package main

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// The approval answer must come from the same scanner the REPL
// loop uses: the scanner buffers piped stdin ahead, so a fresh
// reader on the same stream would see EOF after the first task
// line. One scanner reads the task, the approval, then the next
// task, exactly like a piped REPL session.
func TestAskFromScannerSharesREPLInput(t *testing.T) {
	scanner := bufio.NewScanner(strings.NewReader("Write approved.txt\ny\n/quit\n"))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if !scanner.Scan() || scanner.Text() != "Write approved.txt" {
		t.Fatalf("first task line = %q", scanner.Text())
	}
	ask := askFromScanner(scanner)
	ok, err := ask("Run write ...?")
	if err != nil {
		t.Fatalf("ask returned error: %v", err)
	}
	if !ok {
		t.Fatal("ask(\"y\") = false, want true")
	}
	if !scanner.Scan() || scanner.Text() != "/quit" {
		t.Fatalf("next task line = %q, want /quit", scanner.Text())
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
		scanner := bufio.NewScanner(strings.NewReader(tc.in))
		ok, err := askFromScanner(scanner)("Run write ...?")
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
	scanner := bufio.NewScanner(strings.NewReader(""))
	if _, err := askFromScanner(scanner)("Run write ...?"); !errors.Is(err, io.EOF) {
		t.Fatalf("ask on EOF = %v, want io.EOF", err)
	}
	// A read error surfaces instead of being swallowed.
	boom := errors.New("boom")
	scanner = bufio.NewScanner(errReader{err: boom})
	if _, err := askFromScanner(scanner)("Run write ...?"); !errors.Is(err, boom) {
		t.Fatalf("ask on read error = %v, want boom", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
