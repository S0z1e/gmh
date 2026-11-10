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
	// A literal key is refused wherever it appears.
	p := write(t, t.TempDir(), "c.json", `{"apiKey": "sk-secret"}`)
	if _, err := Load(p); err == nil {
		t.Fatal("apiKey in the config file should be rejected")
	}
	p2 := write(t, t.TempDir(), "c2.json", `{"providers": {"x": {"baseUrl": "https://example.com", "apiKey": "sk-secret"}}}`)
	if _, err := Load(p2); err == nil {
		t.Fatal("apiKey inside a provider should be rejected")
	}
	// "!command" is not supported either.
	p3 := write(t, t.TempDir(), "c3.json", `{"providers": {"x": {"baseUrl": "https://example.com", "apiKey": "!pass show x"}}}`)
	if _, err := Load(p3); err == nil {
		t.Fatal("apiKey as !command should be rejected")
	}
	// A "$NAME" reference inside a provider is the allowed form.
	p4 := write(t, t.TempDir(), "c4.json", `{"providers": {"x": {"baseUrl": "https://example.com", "apiKey": "$MY_API_KEY"}}}`)
	f, err := Load(p4)
	if err != nil {
		t.Fatalf("apiKey $NAME should be accepted: %v", err)
	}
	if f.Providers["x"].APIKey != "$MY_API_KEY" {
		t.Fatalf("apiKey = %q", f.Providers["x"].APIKey)
	}
	// A "$NAME" apiKey at the top level is still an unknown field.
	p5 := write(t, t.TempDir(), "c5.json", `{"apiKey": "$MY_API_KEY"}`)
	if _, err := Load(p5); err == nil {
		t.Fatal("top-level apiKey should be rejected as an unknown field")
	}
}

func TestLoadAcceptsSchema(t *testing.T) {
	p := write(t, t.TempDir(), "c.json", `{"$schema": "https://pi.dev/schemas/models.schema.json", "providers": {"x": {"baseUrl": "https://example.com", "api": "chat"}}}`)
	if _, err := Load(p); err != nil {
		t.Fatalf("$schema should be accepted and ignored: %v", err)
	}
}

func TestNormalizeAPI(t *testing.T) {
	cases := map[string]string{
		"": "chat", "chat": "chat", "openai-completions": "chat",
		"responses": "responses", "openai-responses": "responses",
		"anthropic": "anthropic", "anthropic-messages": "anthropic",
		"OPENAI-RESPONSES": "responses",
	}
	for in, want := range cases {
		got, err := NormalizeAPI(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeAPI(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"google-generative-ai", "soap", "openai"} {
		if _, err := NormalizeAPI(bad); err == nil {
			t.Fatalf("NormalizeAPI(%q) should fail", bad)
		}
	}
}

func TestResolveAPIKey(t *testing.T) {
	t.Setenv("MY_TEST_KEY", "sekrit")
	if got, err := ResolveAPIKey("$MY_TEST_KEY"); err != nil || got != "sekrit" {
		t.Fatalf("ResolveAPIKey $MY_TEST_KEY = %q, %v", got, err)
	}
	if got, err := ResolveAPIKey("${MY_TEST_KEY}"); err != nil || got != "sekrit" {
		t.Fatalf("ResolveAPIKey ${MY_TEST_KEY} = %q, %v", got, err)
	}
	if _, err := ResolveAPIKey("$MY_MISSING_KEY"); err == nil {
		t.Fatal("unset env var should fail")
	}
	if _, err := ResolveAPIKey("sk-literal"); err == nil {
		t.Fatal("literal key should fail")
	}
	if _, err := ResolveAPIKey("!cmd"); err == nil {
		t.Fatal("!command should fail")
	}
	if got, err := ResolveAPIKey(""); err != nil || got != "" {
		t.Fatalf("empty ref = %q, %v", got, err)
	}
}

func TestModelsArrayResolve(t *testing.T) {
	p := write(t, t.TempDir(), "c.json", `{
		"defaultProvider": "discovery",
		"defaultModel": "Agents-A1",
		"providers": {
			"discovery": {
				"baseUrl": "https://discovery-api.intern-ai.org.cn/v1",
				"api": "anthropic-messages",
				"apiKey": "$DISC_KEY",
				"models": [
					{"id": "Agents-A1", "reasoning": true, "contextWindow": 1000000, "maxTokens": 16384, "input": ["text", "image"]},
					{"id": "deepseek-v4-flash-0731", "api": "openai-completions", "contextWindow": 256000}
				]
			}
		}
	}`)
	f, err := LoadExplicit(p)
	if err != nil {
		t.Fatal(err)
	}
	prov, ok := f.Select("")
	if !ok {
		t.Fatal("Select should find the default provider")
	}
	// Default model: the entry, merged over the provider.
	m, ok := prov.ResolveModel("")
	if !ok || m.ID != "Agents-A1" || m.API != "anthropic" || m.BaseURL != "https://discovery-api.intern-ai.org.cn/v1" {
		t.Fatalf("default model = %+v ok=%v", m, ok)
	}
	if m.ContextWindow == nil || *m.ContextWindow != 1000000 {
		t.Fatalf("contextWindow = %v", m.ContextWindow)
	}
	if !m.Reasoning || len(m.Input) != 2 || m.Input[1] != "image" {
		t.Fatalf("reasoning/input = %v %+v", m.Reasoning, m.Input)
	}
	// Second model overrides the api.
	m, ok = prov.ResolveModel("deepseek-v4-flash-0731")
	if !ok || m.API != "chat" || m.ContextWindow == nil || *m.ContextWindow != 256000 {
		t.Fatalf("second model = %+v ok=%v", m, ok)
	}
	// Unknown id fails when a list is declared.
	if _, ok := prov.ResolveModel("ghost"); ok {
		t.Fatal("unknown model id should fail")
	}
	// The flat model stays valid even when it is not in the list.
	prov.Model = "flat-model"
	if _, ok := prov.ResolveModel("flat-model"); !ok {
		t.Fatal("flat model should resolve")
	}
}

func TestResolveModelNoList(t *testing.T) {
	p := Provider{BaseURL: "https://example.com/v1", API: "chat", Model: "m1"}
	m, ok := p.ResolveModel("")
	if !ok || m.ID != "m1" || m.API != "chat" {
		t.Fatalf("no-list default = %+v ok=%v", m, ok)
	}
	m, ok = p.ResolveModel("anything")
	if !ok || m.ID != "anything" {
		t.Fatalf("no-list ad-hoc id = %+v ok=%v", m, ok)
	}
	if len(m.Input) != 1 || m.Input[0] != "text" {
		t.Fatalf("input default = %v", m.Input)
	}
}

func TestMergeModelsReplacesList(t *testing.T) {
	global := &File{
		DefaultProvider: "discovery",
		Providers: map[string]Provider{
			"discovery": {
				BaseURL: "https://discovery-api.intern-ai.org.cn/v1", API: "anthropic",
				Models: []Model{{ID: "Agents-A1"}, {ID: "old-model"}},
			},
		},
	}
	// A project list replaces the whole list (Pi-style).
	project := &File{Providers: map[string]Provider{
		"discovery": {Models: []Model{{ID: "new-model"}}},
	}}
	Merge(global, project)
	got := global.Providers["discovery"].Models
	if len(got) != 1 || got[0].ID != "new-model" {
		t.Fatalf("models after replace = %+v", got)
	}
	// A baseUrl-only override keeps the list.
	global2 := &File{Providers: map[string]Provider{
		"discovery": {BaseURL: "https://a.example", API: "chat", Models: []Model{{ID: "m1"}}},
	}}
	Merge(global2, &File{Providers: map[string]Provider{
		"discovery": {BaseURL: "https://b.example"},
	}})
	d := global2.Providers["discovery"]
	if d.BaseURL != "https://b.example" || len(d.Models) != 1 || d.Models[0].ID != "m1" {
		t.Fatalf("baseUrl-only override = %+v", d)
	}
}

func TestValidateModelsAndCost(t *testing.T) {
	bad := []string{
		// Model without an id.
		`{"providers": {"x": {"baseUrl": "https://example.com", "models": [{"contextWindow": 100}]}}}`,
		// Duplicate model ids.
		`{"providers": {"x": {"baseUrl": "https://example.com", "models": [{"id": "m"}, {"id": "m"}]}}}`,
		// Bad contextWindow / maxTokens.
		`{"providers": {"x": {"baseUrl": "https://example.com", "models": [{"id": "m", "contextWindow": 0}]}}}`,
		`{"providers": {"x": {"baseUrl": "https://example.com", "models": [{"id": "m", "maxTokens": -5}]}}}`,
		// Bad input modality.
		`{"providers": {"x": {"baseUrl": "https://example.com", "models": [{"id": "m", "input": ["audio"]}]}}}`,
		// Cost with a missing rate.
		`{"providers": {"x": {"baseUrl": "https://example.com", "models": [{"id": "m", "cost": {"input": 0.2, "output": 0.8, "cacheRead": 0.05}}]}}}`,
		// Cost with a negative rate.
		`{"providers": {"x": {"baseUrl": "https://example.com", "models": [{"id": "m", "cost": {"input": -1, "output": 0.8, "cacheRead": 0.05, "cacheWrite": 0.2}}]}}}`,
		// defaultModel naming a model the provider does not have.
		`{"defaultProvider": "x", "defaultModel": "ghost", "providers": {"x": {"baseUrl": "https://example.com", "models": [{"id": "m"}]}}}`,
		// google-generative-ai has no client.
		`{"providers": {"x": {"baseUrl": "https://example.com", "api": "google-generative-ai"}}}`,
	}
	for _, c := range bad {
		p := write(t, t.TempDir(), "c.json", c)
		if _, err := LoadExplicit(p); err == nil {
			t.Fatalf("config %s should be rejected", c)
		}
	}
	// Zero rates are a free tier, not an absent one.
	good := write(t, t.TempDir(), "good.json", `{"providers": {"x": {"baseUrl": "https://example.com", "models": [{"id": "m", "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "tiers": [{"inputTokensAbove": 100000, "input": 0.1, "output": 0.2, "cacheRead": 0.01, "cacheWrite": 0.02}]}}]}}}`)
	if _, err := LoadExplicit(good); err != nil {
		t.Fatalf("zero-cost model should be accepted: %v", err)
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
