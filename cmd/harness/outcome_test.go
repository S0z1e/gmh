package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrintFailureProcessHelper(t *testing.T) {
	if os.Getenv("GMH_PRINT_FAILURE_HELPER") != "1" {
		return
	}
	os.Args = []string{"harness", "-p", "task", "--root", os.Getenv("GMH_PRINT_ROOT"), "--config", os.Getenv("GMH_PRINT_CONFIG")}
	main()
}

func TestPrintModeFailureReturnsNonzero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "test provider rejected request", http.StatusBadRequest)
	}))
	defer srv.Close()
	root := t.TempDir()
	config := filepath.Join(root, "provider.json")
	data, err := json.Marshal(map[string]any{"defaultProvider": "mock", "defaultModel": "m", "providers": map[string]any{"mock": map[string]any{"api": "chat", "baseUrl": srv.URL, "apiKey": "fake", "models": []map[string]any{{"id": "m"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(exe, "-test.run=^TestPrintFailureProcessHelper$")
	c.Env = append(os.Environ(), "GMH_PRINT_FAILURE_HELPER=1", "GMH_PRINT_ROOT="+root, "GMH_PRINT_CONFIG="+config, "HOME="+t.TempDir(), "OPENAI_BASE_URL="+srv.URL)
	out, err := c.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "test provider rejected request") {
		t.Fatalf("failed print returned success or wrong failure: %v %s", err, out)
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("exit=%v %s", err, out)
	}
}
