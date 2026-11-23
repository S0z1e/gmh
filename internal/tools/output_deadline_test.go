package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestBashPreservesShellExitBeforeDrainDeadline(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, code := range []int{0, 2} {
			t.Run(fmt.Sprintf("legacy-%t-exit-%d", legacy, code), func(t *testing.T) {
				t.Parallel()
				e := &Executor{Root: t.TempDir()}
				if !legacy {
					e.OutputDir = t.TempDir()
				}
				ctx := context.Background()
				output, err := e.newBashOutput(ctx)
				if err != nil {
					t.Fatal(err)
				}
				// Keep the child's pipe open past the command deadline even
				// when the leader exits immediately.
				command := fmt.Sprintf("sleep 10 & printf early; exit %d", code)
				out, _, err := e.runBashCmd(ctx, command, 3*time.Second, 4*time.Second, output, e.Root, nil)
				if !strings.Contains(out, "Output capture closed") {
					t.Fatalf("inherited pipe did not reach the drain deadline: %q", out)
				}
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
