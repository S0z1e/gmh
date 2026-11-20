package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInvalidArgumentsHaveNoFileSideEffects(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		{"write", `{"path":"file","overwrite":true}`},
		{"write", `{"path":"file","content":null,"overwrite":true}`},
		{"write", `{"path":"file","content":42}`},
		{"write", `{"path":"file","content":"","overwrite":null}`},
		{"write", `{"path":"file","content":"","overwrite":true,"contnet":"typo"}`},
		{"edit", `{"path":"file","oldText":"KEEP"}`},
		{"edit", `{"path":"file","oldText":"KEEP","newText":null}`},
		{"edit", `{"path":"file","startLine":-1,"newText":""}`},
		{"edit", `{"path":"file","newText":""}`},
		{"bash", `{"command":"touch marker","killJobId":"job"}`},
		{"bash", `null`},
	} {
		t.Run(tc.name+tc.args, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "file"), []byte("KEEP"), 0600); err != nil {
				t.Fatal(err)
			}
			e := &Executor{Root: root}
			if _, err := e.Dispatch(context.Background(), tc.name, tc.args); err == nil {
				t.Fatal("invalid args accepted")
			}
			data, err := os.ReadFile(filepath.Join(root, "file"))
			if err != nil || string(data) != "KEEP" {
				t.Fatalf("file changed: %q %v", data, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 {
				t.Fatalf("unexpected side effects: %v %v", entries, err)
			}
		})
	}
}

func TestExplicitEmptyContentRemainsValid(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	if _, err := e.Dispatch(context.Background(), "write", `{"path":"empty","content":""}`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Dispatch(context.Background(), "write", `{"path":"file","content":"KEEP"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Dispatch(context.Background(), "edit", `{"path":"file","oldText":"KEEP","newText":""}`); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(e.Root, "file"))
	if err != nil || len(data) != 0 {
		t.Fatalf("explicit deletion failed: %q %v", data, err)
	}
}
