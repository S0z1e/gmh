package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if f.DefaultProvider != "" || len(f.Providers) != 0 {
		t.Fatalf("missing file should load empty, got %+v", f)
	}
}

func TestLoadProvidersAndShorthand(t *testing.T) {
	p := write(t, t.TempDir(), "c.json", `{
		"defaultProvider": "discovery",
		"providers": {
			"openconsole": {"baseUrl": "https://www.openconsole.cn/v1", "api": "chat", "model": "deepseek-v4.1-flash"},
			"discovery": {"baseUrl": "https://discovery-api.intern-ai.org.cn/v1", "api": "anthropic", "model": "Agents-A1"}
		},
		"mode": "auto"
	}`)
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.DefaultProvider != "discovery" {
		t.Fatalf("defaultProvider = %q", f.DefaultProvider)
	}
	prov, ok := f.Select("")
	if !ok {
		t.Fatal("Select(\"\") should find the default provider")
	}
	if prov.Model != "Agents-A1" || prov.API != "anthropic" {
		t.Fatalf("default provider = %+v", prov)
	}
	prov, ok = f.Select("openconsole")
	if !ok || prov.Model != "deepseek-v4.1-flash" || prov.BaseURL != "https://www.openconsole.cn/v1" {
		t.Fatalf("openconsole = %+v ok=%v", prov, ok)
	}
	if _, ok := f.Select("nope"); ok {
		t.Fatal("Select(\"nope\") should fail")
	}
	if f.Mode != "auto" {
		t.Fatalf("mode = %q", f.Mode)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	// encoding/json matches tags case-insensitively, so "baseurl"
	// is accepted as "baseUrl"; a genuinely unknown field is not.
	p := write(t, t.TempDir(), "c.json", `{"modelName": "gpt-5"}`)
	if _, err := Load(p); err == nil {
		t.Fatal("unknown field should be rejected")
	}
}

func TestLoadRejectsAPIKey(t *testing.T) {
	p := write(t, t.TempDir(), "c.json", `{"apiKey": "sk-secret"}`)
	if _, err := Load(p); err == nil {
		t.Fatal("apiKey in the config file should be rejected")
	}
	p2 := write(t, t.TempDir(), "c2.json", `{"providers": {"x": {"baseUrl": "https://example.com", "apiKey": "sk-secret"}}}`)
	if _, err := Load(p2); err == nil {
		t.Fatal("apiKey inside a provider should be rejected")
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	cases := []string{
		`{"defaultProvider": "ghost"}`,
		`{"providers": {"x": {"baseUrl": "not a url"}}}`,
		`{"providers": {"x": {"api": "soap"}}}`,
		`{"mode": "yolo"}`,
		`{"bashTimeoutSeconds": 0}`,
		`{"bgThresholdSeconds": -1}`,
		`{"tuiTheme": "neon"}`,
		`{not json`,
	}
	for _, c := range cases {
		p := write(t, t.TempDir(), "c.json", c)
		if _, err := LoadExplicit(p); err == nil {
			t.Fatalf("config %s should be rejected", c)
		}
	}
}

func TestLoadForProjectOverridesGlobal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".minimal-harness")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, globalDir, "config.json", `{
		"defaultProvider": "discovery",
		"providers": {
			"openconsole": {"baseUrl": "https://www.openconsole.cn/v1", "api": "chat", "model": "deepseek-v4.1-flash"},
			"discovery": {"baseUrl": "https://discovery-api.intern-ai.org.cn/v1", "api": "anthropic", "model": "Agents-A1"}
		}
	}`)
	// A project file may name a provider only the global file defines.
	root := t.TempDir()
	write(t, root, ".harness.json", `{"defaultProvider": "openconsole"}`)
	f, err := LoadFor(root)
	if err != nil {
		t.Fatal(err)
	}
	prov, ok := f.Select("")
	if !ok || prov.Model != "deepseek-v4.1-flash" {
		t.Fatalf("merged default = %+v ok=%v", prov, ok)
	}
	if len(f.Providers) != 2 {
		t.Fatalf("merged providers = %d, want 2", len(f.Providers))
	}
}

func TestMergeProjectOverGlobal(t *testing.T) {
	global := &File{
		DefaultProvider: "openconsole",
		Providers: map[string]Provider{
			"openconsole": {BaseURL: "https://www.openconsole.cn/v1", API: "chat", Model: "deepseek-v4.1-flash"},
			"discovery":   {BaseURL: "https://discovery-api.intern-ai.org.cn/v1", API: "anthropic", Model: "Agents-A1"},
		},
		Mode: "suggest",
	}
	ten := 10
	project := &File{
		DefaultProvider:    "discovery",
		BgThresholdSeconds: &ten,
		Providers: map[string]Provider{
			"discovery": {Model: "Agents-A1-turbo"},
		},
	}
	Merge(global, project)
	if global.DefaultProvider != "discovery" {
		t.Fatalf("defaultProvider = %q", global.DefaultProvider)
	}
	if global.Mode != "suggest" {
		t.Fatalf("unset mode should stay, got %q", global.Mode)
	}
	if global.BgThresholdSeconds == nil || *global.BgThresholdSeconds != 10 {
		t.Fatalf("bgThresholdSeconds = %v", global.BgThresholdSeconds)
	}
	// Per-field provider merge: model replaced, baseUrl/api kept.
	d := global.Providers["discovery"]
	if d.Model != "Agents-A1-turbo" || d.BaseURL != "https://discovery-api.intern-ai.org.cn/v1" || d.API != "anthropic" {
		t.Fatalf("merged discovery = %+v", d)
	}
	if global.Providers["openconsole"].Model != "deepseek-v4.1-flash" {
		t.Fatal("untouched provider should survive the merge")
	}
}

func TestSelectSingleProvider(t *testing.T) {
	f := &File{Providers: map[string]Provider{"only": {Model: "m1"}}}
	p, ok := f.Select("")
	if !ok || p.Model != "m1" {
		t.Fatalf("single provider should be selected, got %+v ok=%v", p, ok)
	}
}

func TestLoadExplicitMissingIsError(t *testing.T) {
	if _, err := LoadExplicit(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("explicit missing file should be an error")
	}
}
