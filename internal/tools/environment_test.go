package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBashDefaultEnvironmentExcludesCredentialsAndStartupScript(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENAI_API_KEY", "fake-provider-secret")
	t.Setenv("GMH_VENDOR_API_KEY", "fake-vendor-secret")
	marker := filepath.Join(root, "unexpected-startup")
	script := filepath.Join(root, "bootstrap.sh")
	if err := os.WriteFile(script, []byte("printf executed > unexpected-startup\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_ENV", script)
	command := `printf '%s|%s|%s' "${OPENAI_API_KEY-unset}" "${GMH_VENDOR_API_KEY-unset}" "${PATH-unset}"`
	for _, background := range []bool{false, true} {
		e := &Executor{Root: root}
		var out string
		if background {
			job, err := e.BashBackground(context.Background(), BashArgs{Command: command, Timeout: 5})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-job.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("background job did not complete")
			}
			result, ok := job.Result()
			if !ok || result.Err != nil {
				t.Fatalf("background result: %+v", result)
			}
			out = result.Output
		} else {
			var err error
			out, err = e.Bash(context.Background(), BashArgs{Command: command, Timeout: 5})
			if err != nil {
				t.Fatal(err)
			}
		}
		if !strings.HasPrefix(out, "unset|unset|") || strings.HasSuffix(out, "|unset") {
			t.Fatalf("unexpected child environment: %q", out)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("ambient BASH_ENV startup script ran")
		}
	}
}

func TestBashEnvironmentExplicitOptInAndEmptyList(t *testing.T) {
	t.Setenv("GMH_VENDOR_API_KEY", "fake-opt-in")
	e := &Executor{Root: t.TempDir(), BashEnv: []string{"PATH", "GMH_VENDOR_API_KEY"}}
	out, err := e.Bash(context.Background(), BashArgs{Command: `printf '%s' "${GMH_VENDOR_API_KEY-unset}"`})
	if err != nil || out != "fake-opt-in" {
		t.Fatalf("explicit variable not inherited: %q %v", out, err)
	}
	e.BashEnv = []string{}
	out, err = e.Bash(context.Background(), BashArgs{Command: `printf '%s' "${GMH_VENDOR_API_KEY-unset}"`})
	if err != nil || out != "unset" {
		t.Fatalf("empty list inherited parent: %q %v", out, err)
	}
	if env := bashEnvironment([]string{}); env == nil || len(env) != 0 {
		t.Fatal("empty list must be non-nil to prevent full inheritance")
	}
}

func TestSandboxProbeUsesFilteredEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "fake-provider-secret")
	t.Setenv("BASH_ENV", "/not/a/startup/script")
	path := filepath.Join(t.TempDir(), "probe")
	script := "#!/bin/sh\n[ -z \"${OPENAI_API_KEY-}\" ] && [ -z \"${BASH_ENV-}\" ]\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if err := ProbeSandbox(path); err != nil {
		t.Fatalf("probe inherited provider/bootstrap environment: %v", err)
	}
}
