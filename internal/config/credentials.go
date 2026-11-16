package config

import (
	"fmt"
	"net/http"
	"os"

	"minimalharness/internal/endpoint"
)

const ambientAPIKey = "\x00OPENAI_API_KEY"

// Bind raw credential declarations before merging configuration sources. An
// inherited declaration keeps its original destinations; a project must
// explicitly redeclare credentials for a new endpoint. Never serialize this
// in-memory provenance or include credential values in errors.
type credentialBindings map[string]map[string]bool

func (b credentialBindings) allow(value string, urls []string) {
	if b[value] == nil {
		b[value] = map[string]bool{}
	}
	for _, raw := range urls {
		if key := endpoint.Key(raw); key != "" {
			b[value][key] = true
		}
	}
}

func (p Provider) endpoints() []string {
	urls := []string{p.BaseURL}
	for _, m := range p.Models {
		if m.BaseURL != "" {
			urls = append(urls, m.BaseURL)
		}
	}
	return urls
}

func (p *Provider) bindExplicit(src Provider) {
	urls := p.endpoints()
	if src.APIKey != "" {
		p.credentials.allow(src.APIKey, urls)
	}
	for _, value := range src.Headers {
		p.credentials.allow(value, urls)
	}
	for _, m := range src.Models {
		base := m.BaseURL
		if base == "" {
			base = p.BaseURL
		}
		for _, value := range m.Headers {
			p.credentials.allow(value, []string{base})
		}
	}
}

func (p *Provider) bindInitial() {
	if p.credentials != nil {
		return
	}
	p.credentials = credentialBindings{}
	p.credentials.allow(ambientAPIKey, p.endpoints())
	p.bindExplicit(*p)
}

func (f *File) bindInitialCredentials() {
	for name, p := range f.Providers {
		p.bindInitial()
		f.Providers[name] = p
	}
	if f.shorthandCredentials == nil {
		f.shorthandCredentials = credentialBindings{}
		f.shorthandCredentials.allow(ambientAPIKey, []string{f.BaseURL})
	}
}

// ResolveCredentials is shared by startup and model switching. explicitEndpoint
// is true only for the user's OPENAI_BASE_URL override, never project config.
func (p Provider) ResolveCredentials(model Model, baseURL string, explicitEndpoint bool) (string, map[string]string, error) {
	p.bindInitial() // Programmatically constructed providers are a direct source.
	keyRef := p.APIKey
	if keyRef == "" {
		keyRef = ambientAPIKey
	}
	if !explicitEndpoint && !p.credentials[keyRef][endpoint.Key(baseURL)] {
		return "", nil, fmt.Errorf("API key is not bound to this endpoint: explicitly set apiKey in the configuration source that declares the new endpoint")
	}
	var key string
	var err error
	if keyRef == ambientAPIKey {
		key = os.Getenv("OPENAI_API_KEY")
	} else {
		key, err = ResolveAPIKey(keyRef)
	}
	if err != nil {
		return "", nil, err
	}
	if key == "" {
		return "", nil, fmt.Errorf("missing API key: set OPENAI_API_KEY or a provider apiKey")
	}
	merged := map[string]string{}
	for name, value := range p.Headers {
		merged[http.CanonicalHeaderKey(name)] = value
	}
	for name, value := range model.Headers {
		merged[http.CanonicalHeaderKey(name)] = value
	}
	headers := make(map[string]string, len(merged))
	for name, value := range merged {
		if !explicitEndpoint && !p.credentials[value][endpoint.Key(baseURL)] {
			return "", nil, fmt.Errorf("header %q is not bound to this endpoint: explicitly redeclare it alongside the new endpoint", name)
		}
		resolved, err := ResolveValue(value)
		if err != nil {
			return "", nil, err
		}
		headers[name] = resolved
	}
	return key, headers, nil
}
