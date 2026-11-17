package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMalformedSearchPatternsAreInputErrors(t *testing.T) {
	e := setupGrepRoot(t)
	var input *InputError
	_, _, err := e.globWalk(e.Root, "[", 10)
	if !errors.As(err, &input) {
		t.Fatalf("fallback glob lost argument failure: %v", err)
	}
	if _, lookupErr := exec.LookPath("grep"); lookupErr == nil {
		_, err := e.grepFallback(context.Background(), e.Root, GrepArgs{Pattern: "["}, 10, 0)
		if !errors.As(err, &input) {
			t.Fatalf("fallback grep lost argument failure: %v", err)
		}
	}
	if _, err := exec.LookPath("rg"); err != nil {
		return
	}
	for _, run := range []func() error{
		func() error { _, _, err := e.Grep(context.Background(), GrepArgs{Pattern: "["}); return err },
		func() error { _, _, err := e.Glob(context.Background(), GlobArgs{Pattern: "["}); return err },
		func() error {
			_, _, err := e.Grep(context.Background(), GrepArgs{Pattern: "TODO", Include: "["})
			return err
		},
	} {
		err := run()
		var exit *exec.ExitError
		if !errors.As(err, &input) || !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Fatalf("parser error lost original cause: %v", err)
		}
	}
	// The same exit status for an operational I/O error is not an input proof.
	_, _, err = e.Grep(context.Background(), GrepArgs{Pattern: "TODO", Path: filepath.Join(e.Root, "missing")})
	if err == nil || errors.As(err, &input) {
		t.Fatalf("missing directory misclassified: %v", err)
	}
}

func TestMalformedNativeGrepSyntaxIsInputError(t *testing.T) {
	if _, err := exec.LookPath("grep"); err != nil {
		t.Skip(err)
	}
	t.Setenv("LC_ALL", "C")
	e := setupGrepRoot(t)
	for _, pattern := range []string{`[z-a]`, `[[:bogus:]]`, `\1`, `\(`, `a\{`, `a\{2,1\}`, `[[.bogus.]]`} {
		t.Run(pattern, func(t *testing.T) {
			_, err := e.grepFallback(context.Background(), e.Root, GrepArgs{Pattern: pattern}, 10, 0)
			var input *InputError
			if !errors.As(err, &input) {
				t.Fatalf("native parser error not invalid input: %v", err)
			}
		})
	}
	for _, missing := range []string{"missing", "invalid character range"} {
		_, err := e.grepFallback(context.Background(), filepath.Join(e.Root, missing), GrepArgs{Pattern: "valid"}, 10, 0)
		var input *InputError
		if err == nil || errors.As(err, &input) {
			t.Fatalf("native operational error misclassified: %v", err)
		}
	}
}

func TestReversedEditRangePrecedesVersionChecks(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint(stale), func(t *testing.T) {
			e := &Executor{Root: t.TempDir()}
			path := filepath.Join(e.Root, "source")
			if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if stale {
				if _, err := e.Read(ReadArgs{Path: "source"}); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("changed\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := e.Dispatch(context.Background(), "edit", `{"path":"source","startLine":3,"endLine":1,"newText":"replacement"}`)
			var input *InputError
			if !errors.As(err, &input) {
				t.Fatalf("static range error hidden by version state: %v", err)
			}
		})
	}
}
