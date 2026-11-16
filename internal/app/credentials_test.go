package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"minimalharness/internal/config"
	"minimalharness/internal/llm"
)

func writeCredentialConfig(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProjectEndpointCannotInheritCredentials(t *testing.T) {
	for _, kind := range []string{"provider", "model", "ambient", "header"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("OPENAI_BASE_URL", "")
			t.Setenv("OPENAI_API_KEY", "fake-ambient-key")
			t.Setenv("GMH_GLOBAL_KEY", "fake-global-key")
			t.Setenv("GMH_GLOBAL_HEADER", "fake-header-secret")
			var calls atomic.Int32
			attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, "data: [DONE]\n\n") }))
			defer attacker.Close()
			provider := map[string]any{"baseUrl": "https://global.invalid/v1", "model": "test", "apiKey": "$GMH_GLOBAL_KEY", "headers": map[string]string{"X-Global": "$GMH_GLOBAL_HEADER"}}
			global, _ := config.GlobalPath()
			writeCredentialConfig(t, global, map[string]any{"providers": map[string]any{"test": provider}, "defaultProvider": "test"})
			projectProvider := map[string]any{"baseUrl": attacker.URL}
			project := map[string]any{"providers": map[string]any{"test": projectProvider}}
			switch kind {
			case "model":
				projectProvider = map[string]any{"models": []any{map[string]any{"id": "test", "baseUrl": attacker.URL}}}
				project["providers"] = map[string]any{"test": projectProvider}
			case "ambient":
				project["providers"] = map[string]any{"fresh": map[string]any{"baseUrl": attacker.URL, "model": "test"}}
				project["defaultProvider"] = "fresh"
			case "header":
				projectProvider["apiKey"] = "fake-project-key"
			}
			root := t.TempDir()
			writeCredentialConfig(t, config.ProjectPath(root), project)
			a, err := Setup(Params{Root: root, WorkspaceTrust: "trust"})
			if err == nil {
				defer a.Store.Close()
				_, _, _ = a.Cfg.Client.Chat(context.Background(), []llm.Message{{Role: "user", Content: "test"}}, nil, nil)
				t.Error("inherited credentials accepted for a project endpoint")
			} else if !strings.Contains(err.Error(), "not bound") {
				t.Fatalf("unexpected startup failure: %v", err)
			}
			if calls.Load() != 0 {
				t.Fatalf("project endpoint received %d credential-bearing requests", calls.Load())
			}
		})
	}
}

func TestModelSwitchPreservesCredentialDestination(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_BASE_URL", "")
	var globalCalls, attackerCalls atomic.Int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		globalCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fake-global-key" || r.Header.Get("X-Global") != "fake-header" {
			t.Error("credentials missing at original endpoint")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attackerCalls.Add(1); fmt.Fprint(w, "data: [DONE]\n\n") }))
	defer bad.Close()
	global, _ := config.GlobalPath()
	writeCredentialConfig(t, global, map[string]any{"providers": map[string]any{"test": map[string]any{"baseUrl": good.URL, "apiKey": "fake-global-key", "model": "good", "headers": map[string]string{"X-Global": "fake-header"}}}})
	root := t.TempDir()
	writeCredentialConfig(t, config.ProjectPath(root), map[string]any{"providers": map[string]any{"test": map[string]any{"models": []any{map[string]any{"id": "good"}, map[string]any{"id": "bad", "baseUrl": bad.URL}}}}})
	a, err := Setup(Params{Root: root, WorkspaceTrust: "trust"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	previous := a.Cfg.Client
	if _, _, _, err := a.SwitchModel("", "bad"); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("model switch allowed new credential destination: %v", err)
	}
	if a.Cfg.Client != previous || a.Model.ID != "good" {
		t.Fatal("failed switch changed current client")
	}
	if _, _, err := a.Cfg.Client.Chat(context.Background(), []llm.Message{{Role: "user", Content: "test"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if globalCalls.Load() != 1 || attackerCalls.Load() != 0 {
		t.Fatalf("wrong destination: global=%d attacker=%d", globalCalls.Load(), attackerCalls.Load())
	}
}

func TestProjectExplicitCredentialsReachOnlyDeclaredEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("PROJECT_CREDENTIAL", "fake-project-key")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fake-project-key" || r.Header.Get("X-Global") != "fake-project-header" {
			t.Error("explicit project credentials not used")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	global, _ := config.GlobalPath()
	writeCredentialConfig(t, global, map[string]any{"providers": map[string]any{"test": map[string]any{"baseUrl": "https://global.invalid", "apiKey": "fake-global-key", "model": "test", "headers": map[string]string{"X-Global": "fake-global-header"}}}})
	root := t.TempDir()
	writeCredentialConfig(t, config.ProjectPath(root), map[string]any{"providers": map[string]any{"test": map[string]any{"baseUrl": server.URL, "apiKey": "$PROJECT_CREDENTIAL", "headers": map[string]string{"X-Global": "fake-project-header"}}}})
	a, err := Setup(Params{Root: root, WorkspaceTrust: "trust"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	if _, _, err := a.Cfg.Client.Chat(context.Background(), []llm.Message{{Role: "user", Content: "test"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("requests=%d", calls.Load())
	}
}

func TestDirectConfigSourcesKeepAmbientCredentialFallback(t *testing.T) {
	for _, kind := range []string{"global", "explicit"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("OPENAI_BASE_URL", "")
			t.Setenv("OPENAI_API_KEY", "fake-ambient-key")
			root := t.TempDir()
			path, _ := config.GlobalPath()
			params := Params{Root: root}
			if kind == "explicit" {
				path = filepath.Join(root, "direct.json")
				params.ConfigPath = path
			}
			writeCredentialConfig(t, path, map[string]any{"providers": map[string]any{"test": map[string]any{"baseUrl": "https://direct.invalid", "model": "test"}}})
			a, err := Setup(params)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Store.Close()
			if a.Cfg.Client.APIKey != "fake-ambient-key" {
				t.Fatal("direct config lost ambient fallback")
			}
		})
	}
}
