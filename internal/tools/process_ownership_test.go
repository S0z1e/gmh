//go:build linux || darwin

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBashShellCompletionReapsOwnedChildren(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, redirect := range []string{"", "exec >/dev/null 2>&1; "} {
			t.Run(fmt.Sprintf("legacy-%t-redirect-%t", legacy, redirect != ""), func(t *testing.T) {
				e := &Executor{Root: t.TempDir()}
				if !legacy {
					e.OutputDir = t.TempDir()
				}
				command := "(" + redirect + `for ((i=0;i<500;i++)); do if test -f release-child; then : > child-survived; exit; fi; sleep .01; done) & printf finished`
				out, err := e.Bash(context.Background(), BashArgs{Command: command})
				if err != nil || !strings.Contains(out, "finished") {
					t.Fatalf("out=%q err=%v", out, err)
				}
				if err := os.WriteFile(filepath.Join(e.Root, "release-child"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				time.Sleep(300 * time.Millisecond)
				if _, err := os.Stat(filepath.Join(e.Root, "child-survived")); !os.IsNotExist(err) {
					t.Fatal("shell returned but its owned child continued and wrote a marker")
				}
			})
		}
	}
}

func TestBashStoppedShellIsNotCompleted(t *testing.T) {
	e := &Executor{Root: t.TempDir(), OutputDir: t.TempDir()}
	out, err := e.Bash(context.Background(), BashArgs{Command: `(sleep .1; kill -CONT $$) & kill -STOP $$; printf resumed; exit 2`, Timeout: 3})
	var exit *exec.ExitError
	if !strings.Contains(out, "resumed") || !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("stopped shell was mistaken for an exited shell: out=%q err=%v", out, err)
	}
}
