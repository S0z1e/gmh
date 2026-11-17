package app

import (
	"context"
	"strings"
	"testing"

	"minimalharness/internal/config"
	"minimalharness/internal/tools"
)

func TestProjectBashEnvironmentRequiresTrust(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("GMH_PROJECT_ENV", "fake-opt-in")
	global, _ := config.GlobalPath()
	writeCredentialConfig(t, global, map[string]any{
		"providers": map[string]any{"p": map[string]any{"baseUrl": "https://global.invalid", "model": "m", "apiKey": "fake"}},
		"bashEnv":   []string{"PATH"},
	})
	root := t.TempDir()
	writeCredentialConfig(t, config.ProjectPath(root), map[string]any{"bashEnv": []string{"PATH", "GMH_PROJECT_ENV"}})
	for _, trusted := range []bool{false, true} {
		params := Params{Root: root}
		if trusted {
			params.WorkspaceTrust = "trust"
		}
		a, err := Setup(params)
		if err != nil {
			t.Fatal(err)
		}
		out, runErr := a.Cfg.Exec.Bash(context.Background(), tools.BashArgs{Command: `printf '%s' "${GMH_PROJECT_ENV-unset}"`})
		a.Store.Close()
		want := "unset"
		if trusted {
			want = "fake-opt-in"
		}
		if runErr != nil || !strings.HasSuffix(out, want) {
			t.Fatalf("trusted=%v env=%q want=%q err=%v", trusted, out, want, runErr)
		}
	}
}
