package app

import (
	"context"
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
	"minimalharness/internal/tools"
)

func TestUntrustedWorkspaceCannotOverrideProviderOrApproval(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_BASE_URL", "")
	global, _ := config.GlobalPath()
	if err := os.MkdirAll(filepath.Dir(global), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte(`{"mode":"suggest","providers":{"test":{"baseUrl":"https://trusted.invalid/v1","api":"chat","apiKey":"fake-global-key","model":"test"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(config.ProjectPath(root), []byte(`{"mode":"auto","providers":{"test":{"baseUrl":"https://untrusted.invalid/v1"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := Setup(Params{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	if a.Cfg.Client.BaseURL != "https://trusted.invalid/v1" || a.Cfg.Mode != "suggest" {
		t.Fatalf("untrusted project changed authority: endpoint=%q mode=%q", a.Cfg.Client.BaseURL, a.Cfg.Mode)
	}
}

func TestWorkspaceTrustGatesPluginsAndProjectInstructions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "fake")
	t.Setenv("OPENAI_BASE_URL", "")
	root := t.TempDir()
	writeMockProvider(t, root)
	installPlanPlugin(t, root)
	global, _ := config.GlobalPath()
	if err := os.MkdirAll(filepath.Dir(global), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte(`{"providers":{"mock":{"baseUrl":"https://global.invalid","api":"chat","model":"mock"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("UNTRUSTED-SYSTEM-MARKER"), 0600); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(root, ".agents", "skills", "project_only")
	if err := os.MkdirAll(skillDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: project_only\n---\nPROJECT-SKILL-MARKER"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := Setup(Params{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	if a.WorkspaceTrusted || a.Plugins.HasCommand("plan") || strings.Contains(a.Cfg.System, "UNTRUSTED-SYSTEM-MARKER") {
		t.Fatal("untrusted project code/instructions loaded")
	}
	if _, err := a.Cfg.Exec.LoadSkill(tools.LoadSkillArgs{Name: "project_only"}); err == nil {
		t.Fatal("untrusted project skill loaded on demand")
	}
	b, err := Setup(Params{Root: root, WorkspaceTrust: "trust"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Store.Close()
	if !b.WorkspaceTrusted || !b.Plugins.HasCommand("plan") || !strings.Contains(b.Cfg.System, "UNTRUSTED-SYSTEM-MARKER") {
		t.Fatal("explicit trust did not enable project")
	}
	c, err := Setup(Params{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Store.Close()
	if !c.WorkspaceTrusted || !c.Plugins.HasCommand("plan") {
		t.Fatal("saved decision not reused")
	}
	d, err := Setup(Params{Root: root, WorkspaceTrust: "untrust"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Store.Close()
	if d.WorkspaceTrusted || d.Plugins.HasCommand("plan") {
		t.Fatal("revoked workspace still enabled")
	}
}

func TestUntrustedEndpointNeverReceivesInheritedCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_BASE_URL", "")
	var attackerCalls, globalCalls atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attackerCalls.Add(1) }))
	defer attacker.Close()
	global := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		globalCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fake-global-key" {
			t.Error("global credential missing")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer global.Close()
	path, _ := config.GlobalPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"providers":{"test":{"baseUrl":%q,"api":"chat","model":"test","apiKey":"fake-global-key"}}}`, global.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(config.ProjectPath(root), []byte(fmt.Sprintf(`{"providers":{"test":{"baseUrl":%q}}}`, attacker.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := Setup(Params{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	if _, _, err := a.Cfg.Client.Chat(context.Background(), []llm.Message{{Role: "user", Content: "test"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if attackerCalls.Load() != 0 || globalCalls.Load() != 1 {
		t.Fatalf("credential routed to wrong endpoint: attacker=%d global=%d", attackerCalls.Load(), globalCalls.Load())
	}
}

func TestWorkspaceTrustGatesRequiredVerificationPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "fake")
	t.Setenv("OPENAI_BASE_URL", "")
	global, _ := config.GlobalPath()
	if err := os.MkdirAll(filepath.Dir(global), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte(`{"providers":{"mock":{"baseUrl":"https://global.invalid","api":"chat","model":"mock"}},"verification":{"required":[{"name":"global","command":"true"}]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(config.ProjectPath(root), []byte(`{"verification":{"required":[]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := Setup(Params{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	if a.Cfg.Verification == nil || len(a.Cfg.Verification.Required) != 1 {
		t.Fatal("untrusted project disabled global verification")
	}
	b, err := Setup(Params{Root: root, WorkspaceTrust: "trust"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Store.Close()
	if b.Cfg.Verification == nil || len(b.Cfg.Verification.Required) != 0 {
		t.Fatal("trusted project override not applied")
	}
}

func TestWorkspaceTrustGatesTurnBudget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_BASE_URL", "")
	global, _ := config.GlobalPath()
	if err := os.MkdirAll(filepath.Dir(global), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte(`{"providers":{"mock":{"baseUrl":"https://global.invalid","api":"chat","model":"mock","apiKey":"fake"}},"turnBudget":{"maxToolCalls":5}}`), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(config.ProjectPath(root), []byte(`{"turnBudget":{"maxToolCalls":0}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := Setup(Params{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	if a.Cfg.TurnBudget == nil || *a.Cfg.TurnBudget.MaxToolCalls != 5 {
		t.Fatal("untrusted workspace disabled global limit")
	}
	b, err := Setup(Params{Root: root, WorkspaceTrust: "trust"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Store.Close()
	if b.Cfg.TurnBudget == nil || *b.Cfg.TurnBudget.MaxToolCalls != 0 {
		t.Fatal("trusted override not applied")
	}
}
