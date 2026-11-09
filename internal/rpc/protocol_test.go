package rpc

import (
	"strings"
	"testing"
)

func TestEncodeDecodeRequest(t *testing.T) {
	req := Request{ID: "r-1", Type: "prompt", Message: "hello\nworld"}
	b, err := encodeLine(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("encoded line has no LF: %q", b)
	}
	got, err := decodeRequest(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if got != req {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, req)
	}
}

func TestDecodeRequestErrors(t *testing.T) {
	for _, line := range []string{`not json`, `{"id":"x"}`, `{"type":""}`} {
		if _, err := decodeRequest(line); err == nil {
			t.Errorf("decodeRequest(%q) should fail", line)
		}
	}
}

func TestParseApprovalPrompt(t *testing.T) {
	tool, args := parseApprovalPrompt(`Run write {"path":"u.txt","content":"hi"}?`)
	if tool != "write" {
		t.Fatalf("tool = %q, want write", tool)
	}
	m, ok := args.(map[string]any)
	if !ok || m["path"] != "u.txt" {
		t.Fatalf("args = %#v, want path u.txt", args)
	}
	// A prompt without the expected shape falls back to the raw text.
	tool, args = parseApprovalPrompt("Run bash?")
	if tool != "" {
		t.Fatalf("tool = %q, want empty", tool)
	}
	if args != "Run bash?" {
		t.Fatalf("args = %#v, want the raw prompt", args)
	}
}

func TestParseToolArgs(t *testing.T) {
	v := parseToolArgs(`{"path":"t.txt"}`)
	m, ok := v.(map[string]any)
	if !ok || m["path"] != "t.txt" {
		t.Fatalf("parsed = %#v", v)
	}
	if got := parseToolArgs(`{broken`); got != `{broken` {
		t.Fatalf("unparseable args should come back raw, got %#v", got)
	}
}

func TestCapOutput(t *testing.T) {
	short := "hello"
	if got, trunc := capOutput(short, 100); got != short || trunc {
		t.Fatalf("short text should pass through: %q %v", got, trunc)
	}
	long := strings.Repeat("x", 1000)
	got, trunc := capOutput(long, 100)
	if !trunc {
		t.Fatalf("long text should be truncated")
	}
	if !strings.Contains(got, "[truncated: 100 chars shown of 1000]") {
		t.Fatalf("truncation marker missing: %q", got)
	}
	if len(got) > 200 {
		t.Fatalf("capped output too long: %d chars", len(got))
	}
}

func TestSlashRequest(t *testing.T) {
	cases := map[string]string{
		"/quit": "/quit", "/exit": "/exit", "/usage": "/usage", "/jobs": "/jobs",
		"/clear": "/clear", "/compact": "/compact",
	}
	for line, want := range cases {
		typ, _ := slashRequest(line)
		if typ == "" {
			t.Errorf("slashRequest(%q) = empty, want a control type", line)
		}
		_ = want
	}
	typ, arg := slashRequest("/skill:foo")
	if typ != "skill" || arg != "foo" {
		t.Errorf("slashRequest(/skill:foo) = (%q, %q), want (skill, foo)", typ, arg)
	}
	for _, line := range []string{"hello", "/foo", "/skill", " /usage"} {
		if typ, _ := slashRequest(line); typ != "" {
			t.Errorf("slashRequest(%q) = %q, want empty (a task)", line, typ)
		}
	}
}
