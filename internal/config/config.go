// Package config loads the harness JSON configuration file, Pi-style:
// a global file plus a project file that overrides it. The model setup
// follows Pi's models.json shape (see pi.dev/docs/latest/models-configuration):
// each provider carries a models array; a model entry's only required
// field is id, the rest have Pi defaults or are unset.
//
// Files:
//   - global:  ~/.minimal-harness/config.json
//   - project: <root>/.harness.json (wins where it sets a value)
//
// A provider must be configured by the user: there is no built-in
// default provider. A config with no providers is a setup error.
// A project file's models array replaces the provider's whole list
// (Pi-style); a baseUrl/headers-only override keeps the models.
//
// Example:
//
//	{
//	  "defaultProvider": "discovery",
//	  "defaultModel": "Agents-A1",
//	  "providers": {
//	    "openconsole": {
//	      "baseUrl": "https://www.openconsole.cn/v1",
//	      "api": "openai-completions",
//	      "apiKey": "$OPENAI_API_KEY",
//	      "models": [
//	        {"id": "deepseek-v4.1-flash", "contextWindow": 1000000,
//	         "maxTokens": 16384, "input": ["text", "image"],
//	         "cost": {"input": 0.2, "output": 0.8, "cacheRead": 0.05, "cacheWrite": 0.2},
//	         "samplingParams": {"temperature": 0.7}}
//	      ]
//	    },
//	    "discovery": {
//	      "baseUrl": "https://discovery-api.intern-ai.org.cn/v1",
//	      "api": "anthropic-messages",
//	      "apiKey": "$DISCOVERY_API_KEY",
//	      "models": [
//	        {"id": "Agents-A1", "reasoning": true, "contextWindow": 1000000, "maxTokens": 16384}
//	      ]
//	    }
//	  }
//	}
//
// The top-level baseUrl/api/model fields are a shorthand for a single
// provider without the providers map. Precedence everywhere is:
// flag > environment variable > project file > global file > Pi default
// (contextWindow 128000, maxTokens 16384). The API key is never a
// literal in a file: provider apiKey is a "$NAME" environment reference.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Pi defaults for optional model fields, from the models-configuration
// reference. A model that omits contextWindow/maxTokens gets these.
const (
	DefaultContextWindow = 128000
	DefaultMaxTokens     = 16384
)

// GlobalFileName is the config file name under ~/.minimal-harness/.
// ProjectFileName is the config file name under the workspace root.
const (
	GlobalFileName  = "config.json"
	ProjectFileName = ".harness.json"
)

// ModelCost is Pi's per-model pricing in USD per million tokens.
// The four rates are required when a cost object is present (Pi marks
// them required); a rate of 0 is a free tier, not an absent one. The
// harness parses and stores cost; it is not yet wired into the cost
// estimate (env rates still drive that).
type ModelCost struct {
	Input      *float64   `json:"input"`
	Output     *float64   `json:"output"`
	CacheRead  *float64   `json:"cacheRead"`
	CacheWrite *float64   `json:"cacheWrite"`
	Tiers      []CostTier `json:"tiers"`
}

// CostTier is one request-wide pricing tier: when total request input
// exceeds InputTokensAbove, this tier's rates apply to the full request.
type CostTier struct {
	InputTokensAbove *float64 `json:"inputTokensAbove"`
	Input            *float64 `json:"input"`
	Output           *float64 `json:"output"`
	CacheRead        *float64 `json:"cacheRead"`
	CacheWrite       *float64 `json:"cacheWrite"`
}

// Model is one model on a provider, Pi's models-entry shape. Only ID
// is required. ContextWindow/MaxTokens are pointers so "unset" (Pi
// default) is distinguishable from an explicit value.
type Model struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	API     string `json:"api"`
	BaseURL string `json:"baseUrl"`
	// Reasoning reports whether the model supports thinking
	// (Pi default false). The harness already handles reasoning
	// content generically; this is metadata for now.
	Reasoning      bool              `json:"reasoning"`
	ContextWindow  *int              `json:"contextWindow"`
	MaxTokens      *int              `json:"maxTokens"`
	SamplingParams map[string]any    `json:"samplingParams"`
	Headers        map[string]string `json:"headers"`
	// Input lists the modalities the model accepts ("text", "image").
	// Pi default ["text"]. The harness is text-only for now; the
	// field is parsed and stored so a vision-capable model can be
	// declared.
	Input []string   `json:"input"`
	Cost  *ModelCost `json:"cost"`
}

// Provider is one model provider: where to reach it, which API shape
// to speak, how to authenticate, and which models it offers. The flat
// Model/API/BaseURL fields are the provider default; a Models entry
// overrides them where it sets a value. A provider needs a baseUrl
// and an api at provider or model level.
type Provider struct {
	BaseURL string `json:"baseUrl"`
	API     string `json:"api"`
	Model   string `json:"model"`
	// APIKey is a "$NAME" / "${NAME}" environment reference only.
	// A literal key is refused at load time; "!command" is not supported.
	APIKey  string            `json:"apiKey"`
	Headers map[string]string `json:"headers"`
	Models  []Model           `json:"models"`
}

// File is the on-disk shape. Pointer fields distinguish "not set"
// (inherit from the other file, the environment, or the default)
// from an explicitly set value, including 0 and false.
type File struct {
	// Schema is accepted and ignored: Pi files carry
	// "$schema": "https://pi.dev/schemas/models.schema.json"
	// for editor validation.
	Schema          string              `json:"$schema"`
	DefaultProvider string              `json:"defaultProvider"`
	DefaultModel    string              `json:"defaultModel"`
	Providers       map[string]Provider `json:"providers"`
	// Shorthand for a single provider without the providers map.
	// A selected provider entry wins over these.
	BaseURL string `json:"baseUrl"`
	API     string `json:"api"`
	Model   string `json:"model"`

	Mode                string `json:"mode"`
	BashTimeoutSeconds  *int   `json:"bashTimeoutSeconds"`
	BgThresholdSeconds  *int   `json:"bgThresholdSeconds"`
	DenyInsteadOfPrompt *bool  `json:"denyInsteadOfPrompt"`
	Sandbox             *bool  `json:"sandbox"`
	ContextLimit        *int   `json:"contextLimit"`
	AnthropicMaxTokens  *int   `json:"anthropicMaxTokens"`
	TUITheme            string `json:"tuiTheme"`
}

// GlobalPath returns ~/.minimal-harness/config.json.
func GlobalPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".minimal-harness", GlobalFileName), nil
}

// ProjectPath returns <root>/.harness.json.
func ProjectPath(root string) string {
	return filepath.Join(root, ProjectFileName)
}

// Load reads one config file. A missing file is not an error: it
// yields an empty File, so a fresh install needs no config at all.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{}, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return &File{}, nil
	}
	if err := checkAPIKeyForms(data); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	// Values are validated by the caller on the merged result
	// (LoadFor) or the single file (LoadExplicit): a project file
	// may legitimately name a provider only the global file defines.
	return &f, nil
}

// LoadExplicit reads exactly the file named by path. Unlike Load, a
// missing file is an error: the user asked for this file by name.
// The file's values are validated.
func LoadExplicit(path string) (*File, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	f, err := Load(path)
	if err != nil {
		return nil, err
	}
	if err := Validate(f); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return f, nil
}

// LoadFor loads the global file, then the project file for root on
// top of it, field by field (Pi's nested-merge behaviour).
func LoadFor(root string) (*File, error) {
	global, err := GlobalPath()
	if err != nil {
		return nil, err
	}
	f, err := Load(global)
	if err != nil {
		return nil, err
	}
	project, err := Load(ProjectPath(root))
	if err != nil {
		return nil, err
	}
	Merge(f, project)
	if err := Validate(f); err != nil {
		return nil, err
	}
	return f, nil
}

// Merge applies src on top of dst, field by field. A source field
// only wins when it is set; provider entries merge per field too.
// A source models list replaces the provider's whole list (Pi-style):
// a baseUrl/headers-only override keeps the destination's models.
func Merge(dst, src *File) {
	if src.Schema != "" {
		dst.Schema = src.Schema
	}
	if src.DefaultProvider != "" {
		dst.DefaultProvider = src.DefaultProvider
	}
	if src.DefaultModel != "" {
		dst.DefaultModel = src.DefaultModel
	}
	if src.BaseURL != "" {
		dst.BaseURL = src.BaseURL
	}
	if src.API != "" {
		dst.API = src.API
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.Mode != "" {
		dst.Mode = src.Mode
	}
	if src.BashTimeoutSeconds != nil {
		dst.BashTimeoutSeconds = src.BashTimeoutSeconds
	}
	if src.BgThresholdSeconds != nil {
		dst.BgThresholdSeconds = src.BgThresholdSeconds
	}
	if src.DenyInsteadOfPrompt != nil {
		dst.DenyInsteadOfPrompt = src.DenyInsteadOfPrompt
	}
	if src.Sandbox != nil {
		dst.Sandbox = src.Sandbox
	}
	if src.ContextLimit != nil {
		dst.ContextLimit = src.ContextLimit
	}
	if src.AnthropicMaxTokens != nil {
		dst.AnthropicMaxTokens = src.AnthropicMaxTokens
	}
	if src.TUITheme != "" {
		dst.TUITheme = src.TUITheme
	}
	for name, sp := range src.Providers {
		if dst.Providers == nil {
			dst.Providers = map[string]Provider{}
		}
		dp := dst.Providers[name]
		if sp.BaseURL != "" {
			dp.BaseURL = sp.BaseURL
		}
		if sp.API != "" {
			dp.API = sp.API
		}
		if sp.Model != "" {
			dp.Model = sp.Model
		}
		if sp.APIKey != "" {
			dp.APIKey = sp.APIKey
		}
		if len(sp.Headers) > 0 {
			if dp.Headers == nil {
				dp.Headers = map[string]string{}
			}
			for k, v := range sp.Headers {
				dp.Headers[k] = v
			}
		}
		if len(sp.Models) > 0 {
			dp.Models = sp.Models
		}
		dst.Providers[name] = dp
	}
}

// ProviderNames returns the configured provider names, sorted.
func (f *File) ProviderNames() []string {
	names := make([]string, 0, len(f.Providers))
	for name := range f.Providers {
		names = append(names, name)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}

// Select returns the provider entry to use: the named provider, or
// the defaultProvider, or the single configured provider, or the
// top-level shorthand as a synthetic provider, or an empty Provider
// when nothing is configured at all. ok is false when a named
// provider does not exist.
func (f *File) Select(name string) (Provider, bool) {
	if name == "" {
		name = f.DefaultProvider
	}
	if name != "" {
		p, ok := f.Providers[name]
		return p, ok
	}
	if len(f.Providers) == 1 {
		for _, p := range f.Providers {
			return p, true
		}
	}
	if f.BaseURL != "" || f.API != "" || f.Model != "" {
		return Provider{BaseURL: f.BaseURL, API: f.API, Model: f.Model}, true
	}
	return Provider{}, true
}

// SelectedName returns the name of the provider Select would pick
// for the requested name: the request itself, the defaultProvider,
// the single configured provider, or "" for the shorthand.
func (f *File) SelectedName(requested string) string {
	if requested != "" {
		return requested
	}
	if f.DefaultProvider != "" {
		return f.DefaultProvider
	}
	if len(f.Providers) == 1 {
		return f.ProviderNames()[0]
	}
	return ""
}

// HasProviders reports whether any provider is configured, either
// in the providers map or via the top-level shorthand.
func (f *File) HasProviders() bool {
	return len(f.Providers) > 0 || f.BaseURL != "" || f.API != "" || f.Model != ""
}

// NormalizeAPI maps a Pi api spelling ("openai-completions",
// "openai-responses", "anthropic-messages") or a harness short name
// ("chat", "responses", "anthropic") to the canonical short name.
// An empty value normalises to "chat". Anything else — including
// Pi's "google-generative-ai", which the harness has no client for —
// is an error.
func NormalizeAPI(api string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(api)) {
	case "", "chat", "openai-completions":
		return "chat", nil
	case "responses", "openai-responses":
		return "responses", nil
	case "anthropic", "anthropic-messages":
		return "anthropic", nil
	}
	return "", fmt.Errorf("api %q is not supported: use chat/openai-completions, responses/openai-responses or anthropic/anthropic-messages", api)
}

// ResolveModel picks the model for a provider and merges the entry
// over the provider's flat fields. id "" means "the default": the
// provider's flat model, else the first models entry. When the
// provider declares a models list, id must match an entry or the
// flat model; ok is false otherwise. With no list, any id is fine.
// Input defaults to ["text"] (Pi); ContextWindow/MaxTokens stay nil
// when unset so the caller can fall back to the config global and
// then the Pi defaults.
func (p Provider) ResolveModel(id string) (Model, bool) {
	// The api is stored canonically (Pi spellings normalised):
	// validation already rejected anything unrecognised.
	api, _ := NormalizeAPI(p.API)
	m := Model{API: api, BaseURL: p.BaseURL}
	if id == "" {
		id = p.Model
		if id == "" && len(p.Models) > 0 {
			id = p.Models[0].ID
		}
	}
	m.ID = id
	if len(p.Models) == 0 {
		m.Input = []string{"text"}
		return m, true
	}
	for _, e := range p.Models {
		if e.ID != id {
			continue
		}
		if e.Name != "" {
			m.Name = e.Name
		}
		if e.API != "" {
			if norm, err := NormalizeAPI(e.API); err == nil {
				m.API = norm
			} else {
				m.API = e.API
			}
		}
		if e.BaseURL != "" {
			m.BaseURL = e.BaseURL
		}
		m.Reasoning = e.Reasoning
		m.ContextWindow = e.ContextWindow
		m.MaxTokens = e.MaxTokens
		m.SamplingParams = e.SamplingParams
		m.Headers = e.Headers
		m.Input = e.Input
		m.Cost = e.Cost
		if len(m.Input) == 0 {
			m.Input = []string{"text"}
		}
		return m, true
	}
	if id != "" && id == p.Model {
		// The flat model is valid even when it is not in the list.
		m.Input = []string{"text"}
		return m, true
	}
	return m, false
}

// hasModel reports whether id is the provider's flat model or one
// of its models entries.
func (p Provider) hasModel(id string) bool {
	if id == p.Model {
		return true
	}
	for _, m := range p.Models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// Validate checks the values a file may carry. Unknown fields are
// already rejected at decode time. Presence (a provider must define
// a baseUrl and an api, a model must be resolvable) is checked by
// the caller after flag/env overrides, not here.
func Validate(f *File) error {
	if f.DefaultProvider != "" {
		if _, ok := f.Providers[f.DefaultProvider]; !ok {
			return fmt.Errorf("defaultProvider %q is not defined in providers (%s)", f.DefaultProvider, strings.Join(f.ProviderNames(), ", "))
		}
	}
	if f.DefaultModel != "" {
		prov, ok := f.Select(f.DefaultProvider)
		if !ok {
			return fmt.Errorf("defaultModel %q: default provider %q is not defined", f.DefaultModel, f.DefaultProvider)
		}
		if len(prov.Models) > 0 && !prov.hasModel(f.DefaultModel) {
			return fmt.Errorf("defaultModel %q is not a model of provider %q", f.DefaultModel, f.SelectedName(""))
		}
	}
	for name, p := range f.Providers {
		if err := validateProvider(name, p); err != nil {
			return err
		}
	}
	if err := validateProvider("default", Provider{BaseURL: f.BaseURL, API: f.API, Model: f.Model}); err != nil {
		return err
	}
	if f.Mode != "" && f.Mode != "suggest" && f.Mode != "auto" {
		return fmt.Errorf("mode %q: use suggest or auto", f.Mode)
	}
	if f.BashTimeoutSeconds != nil && *f.BashTimeoutSeconds <= 0 {
		return fmt.Errorf("bashTimeoutSeconds must be > 0")
	}
	if f.BgThresholdSeconds != nil && *f.BgThresholdSeconds < 0 {
		return fmt.Errorf("bgThresholdSeconds must be >= 0 (0 disables backgrounding)")
	}
	if f.ContextLimit != nil && *f.ContextLimit <= 0 {
		return fmt.Errorf("contextLimit must be > 0")
	}
	if f.AnthropicMaxTokens != nil && *f.AnthropicMaxTokens <= 0 {
		return fmt.Errorf("anthropicMaxTokens must be > 0")
	}
	if f.TUITheme != "" && f.TUITheme != "default" && f.TUITheme != "terminal" && f.TUITheme != "catppuccin-mocha" {
		return fmt.Errorf("tuiTheme %q: use default, terminal or catppuccin-mocha", f.TUITheme)
	}
	return nil
}

func validateProvider(name string, p Provider) error {
	if p.BaseURL != "" {
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("provider %q: baseUrl %q is not an http(s) URL", name, p.BaseURL)
		}
	}
	if p.API != "" {
		if _, err := NormalizeAPI(p.API); err != nil {
			return fmt.Errorf("provider %q: %w", name, err)
		}
	}
	if err := validateAPIKeyForm(p.APIKey); err != nil {
		return fmt.Errorf("provider %q: %w", name, err)
	}
	seen := map[string]bool{}
	for _, m := range p.Models {
		if err := validateModel(name, m); err != nil {
			return err
		}
		if seen[m.ID] {
			return fmt.Errorf("provider %q: duplicate model id %q", name, m.ID)
		}
		seen[m.ID] = true
	}
	return nil
}

func validateModel(provider string, m Model) error {
	if m.ID == "" {
		return fmt.Errorf("provider %q: a models entry needs an id", provider)
	}
	if m.API != "" {
		if _, err := NormalizeAPI(m.API); err != nil {
			return fmt.Errorf("provider %q model %q: %w", provider, m.ID, err)
		}
	}
	if m.BaseURL != "" {
		u, err := url.Parse(m.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("provider %q model %q: baseUrl %q is not an http(s) URL", provider, m.ID, m.BaseURL)
		}
	}
	if m.ContextWindow != nil && *m.ContextWindow <= 0 {
		return fmt.Errorf("provider %q model %q: contextWindow must be > 0", provider, m.ID)
	}
	if m.MaxTokens != nil && *m.MaxTokens <= 0 {
		return fmt.Errorf("provider %q model %q: maxTokens must be > 0", provider, m.ID)
	}
	for _, in := range m.Input {
		if in != "text" && in != "image" {
			return fmt.Errorf("provider %q model %q: input %q: use text or image", provider, m.ID, in)
		}
	}
	if m.Cost != nil {
		if err := validateCost(provider, m.ID, m.Cost); err != nil {
			return err
		}
	}
	return nil
}

func validateCost(provider, model string, c *ModelCost) error {
	for _, field := range []struct {
		name string
		v    *float64
	}{
		{"input", c.Input}, {"output", c.Output},
		{"cacheRead", c.CacheRead}, {"cacheWrite", c.CacheWrite},
	} {
		if field.v == nil {
			return fmt.Errorf("provider %q model %q: cost.%s is required when cost is set", provider, model, field.name)
		}
		if *field.v < 0 {
			return fmt.Errorf("provider %q model %q: cost.%s must be >= 0", provider, model, field.name)
		}
	}
	for i, t := range c.Tiers {
		if t.InputTokensAbove == nil || *t.InputTokensAbove < 0 {
			return fmt.Errorf("provider %q model %q: cost.tiers[%d].inputTokensAbove is required and must be >= 0", provider, model, i)
		}
		for _, field := range []struct {
			name string
			v    *float64
		}{
			{"input", t.Input}, {"output", t.Output},
			{"cacheRead", t.CacheRead}, {"cacheWrite", t.CacheWrite},
		} {
			if field.v == nil {
				return fmt.Errorf("provider %q model %q: cost.tiers[%d].%s is required", provider, model, i, field.name)
			}
			if *field.v < 0 {
				return fmt.Errorf("provider %q model %q: cost.tiers[%d].%s must be >= 0", provider, model, i, field.name)
			}
		}
	}
	return nil
}

// validateAPIKeyForm checks a provider apiKey value: only a "$NAME"
// or "${NAME}" environment reference is allowed. A literal key must
// never be written in a config file; "!command" is not supported.
func validateAPIKeyForm(ref string) error {
	if ref == "" {
		return nil
	}
	if _, ok := envRefName(ref); ok {
		return nil
	}
	return fmt.Errorf("apiKey %q: use \"$NAME\" to read the key from an environment variable (a literal key must never be written in the config file; \"!command\" is not supported)", ref)
}

// ResolveAPIKey resolves a provider apiKey reference to the key
// itself, reading the named environment variable. An unset variable
// is an error: fail fast instead of calling the API with an empty key.
func ResolveAPIKey(ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	name, ok := envRefName(ref)
	if !ok {
		return "", fmt.Errorf("apiKey %q: use \"$NAME\" to read the key from an environment variable", ref)
	}
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("apiKey %q: environment variable %s is not set", ref, name)
	}
	return v, nil
}

// ResolveValue resolves one header value: a "$NAME" reference reads
// the environment (unset is an error); any other value is used as-is.
func ResolveValue(v string) (string, error) {
	name, ok := envRefName(v)
	if !ok {
		return v, nil
	}
	if got := os.Getenv(name); got != "" {
		return got, nil
	}
	return "", fmt.Errorf("header value %q: environment variable %s is not set", v, name)
}

// envRefName parses "$NAME" / "${NAME}" and reports the name.
// Anything else is not an environment reference.
func envRefName(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "${") && strings.HasSuffix(ref, "}") && len(ref) > 3 {
		name := ref[2 : len(ref)-1]
		if validEnvName(name) {
			return name, true
		}
		return "", false
	}
	if strings.HasPrefix(ref, "$") && len(ref) > 1 {
		name := ref[1:]
		if validEnvName(name) {
			return name, true
		}
	}
	return "", false
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// checkAPIKeyForms refuses a non-reference apiKey value anywhere in
// the raw document. A "$NAME" reference is fine; a literal key or
// "!command" is not. Misplaced apiKey fields (top level, inside a
// model) are rejected later as unknown fields by the strict decoder.
func checkAPIKeyForms(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return err // the strict decode below reports the real error
	}
	return walkKeys(v)
}

func walkKeys(v any) error {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if strings.EqualFold(k, "apiKey") || strings.EqualFold(k, "api_key") {
				s, isStr := child.(string)
				if !isStr || validateAPIKeyForm(s) != nil {
					return fmt.Errorf("apiKey must be a \"$NAME\" environment reference and cannot be a literal key in the config file")
				}
			}
			if err := walkKeys(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range t {
			if err := walkKeys(child); err != nil {
				return err
			}
		}
	}
	return nil
}
