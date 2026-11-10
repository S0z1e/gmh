// Package config loads the harness JSON configuration file, Pi-style:
// a global file plus a project file that overrides it field by field.
// The API key is never read from a file: it stays in OPENAI_API_KEY.
//
// Files:
//   - global:  ~/.minimal-harness/config.json
//   - project: <root>/.harness.json (wins where it sets a value)
//
// Example:
//
//	{
//	  "defaultProvider": "discovery",
//	  "providers": {
//	    "openconsole": {"baseUrl": "https://www.openconsole.cn/v1", "api": "chat", "model": "deepseek-v4.1-flash"},
//	    "discovery":   {"baseUrl": "https://discovery-api.intern-ai.org.cn/v1", "api": "anthropic", "model": "Agents-A1"}
//	  }
//	}
//
// The top-level baseUrl/api/model fields are a shorthand for a single
// provider without the providers map. Precedence everywhere is:
// flag > environment variable > project file > global file > built-in
// default.
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

// GlobalFileName is the config file name under ~/.minimal-harness/.
// ProjectFileName is the config file name under the workspace root.
const (
	GlobalFileName  = "config.json"
	ProjectFileName = ".harness.json"
)

// Provider is one model provider: where to reach it, which API shape
// to speak, and which model to use by default.
type Provider struct {
	BaseURL string `json:"baseUrl"`
	API     string `json:"api"`
	Model   string `json:"model"`
}

// File is the on-disk shape. Pointer fields distinguish "not set"
// (inherit from the other file, the environment, or the default)
// from an explicitly set value, including 0 and false.
type File struct {
	DefaultProvider string              `json:"defaultProvider"`
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
	if err := rejectAPIKey(data); err != nil {
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
func Merge(dst, src *File) {
	if src.DefaultProvider != "" {
		dst.DefaultProvider = src.DefaultProvider
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
// the defaultProvider, or the single configured provider, or an
// empty Provider when the file only uses the shorthand fields.
// ok is false when a named provider does not exist.
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
	return Provider{}, true
}

// Validate checks the values a file may carry. Unknown fields are
// already rejected at decode time.
func Validate(f *File) error {
	if f.DefaultProvider != "" {
		if _, ok := f.Providers[f.DefaultProvider]; !ok {
			return fmt.Errorf("defaultProvider %q is not defined in providers (%s)", f.DefaultProvider, strings.Join(f.ProviderNames(), ", "))
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
	if p.API != "" && p.API != "chat" && p.API != "responses" && p.API != "anthropic" {
		return fmt.Errorf("provider %q: api %q: use chat, responses or anthropic", name, p.API)
	}
	return nil
}

// rejectAPIKey refuses any apiKey/api_key field anywhere in the raw
// document. The key lives in OPENAI_API_KEY and must never land in
// a config file.
func rejectAPIKey(data []byte) error {
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
				return fmt.Errorf("apiKey must stay in the OPENAI_API_KEY environment variable and cannot be set in the config file")
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
