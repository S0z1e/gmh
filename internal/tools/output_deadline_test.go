package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

func TestBashPreservesShellExitBeforeDrainDeadline(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, code := range []int{0, 2} {
			t.Run(fmt.Sprintf("legacy-%t-exit-%d", legacy, code), func(t *testing.T) {
				e := &Executor{Root: t.TempDir()}
				if !legacy {
					e.OutputDir = t.TempDir()
				}
				_, err := e.Bash(context.Background(), BashArgs{Command: fmt.Sprintf("sleep .85; (sleep .5) & printf early; exit %d", code), Timeout: 1})
				if code == 0 {
					if err != nil {
						t.Fatalf("completed shell reported failure: %v", err)
					}
					return
				}
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != code {
					t.Fatalf("shell exited before timeout, want exit %d, got %v", code, err)
				}
			})
		}
	}
}
