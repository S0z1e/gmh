package tools

import (
	"context"
	"errors"
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
