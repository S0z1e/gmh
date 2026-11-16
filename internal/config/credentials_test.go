package config

import (
	"strings"
	"testing"
)

func TestCredentialBindingsFollowDeclarationsAcrossMerges(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	t.Setenv("HEADER_SECRET", "header-secret")
	for _, kind := range []string{"key", "ambient", "header", "model-header"} {
		t.Run(kind, func(t *testing.T) {
			p := Provider{BaseURL: "https://old.invalid", Model: "m", APIKey: "literal-secret"}
			if kind == "ambient" {
				p.APIKey = ""
			}
			if kind == "header" {
				p.Headers = map[string]string{"X-Secret": "$HEADER_SECRET"}
			}
			if kind == "model-header" {
				p.Models = []Model{{ID: "m", Headers: map[string]string{"X-Secret": "$HEADER_SECRET"}}}
			}
			f := &File{Providers: map[string]Provider{"p": p}}
			override := Provider{BaseURL: "https://new.invalid"}
			if kind == "header" || kind == "model-header" {
				override.APIKey = "new-explicit-secret"
			}
			Merge(f, &File{Providers: map[string]Provider{"p": override}})
			merged := f.Providers["p"]
			model, _ := merged.ResolveModel("")
			_, _, err := merged.ResolveCredentials(model, model.BaseURL, false)
			if err == nil || !strings.Contains(err.Error(), "not bound") {
				t.Fatalf("inherited material rebound: %v", err)
			}
			for _, secret := range []string{"literal-secret", "ambient-secret", "header-secret", "new-explicit-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("credential leaked in error")
				}
			}
		})
	}
}

func TestExplicitCredentialDeclarationsAuthorizeNewEndpoint(t *testing.T) {
	t.Setenv("PROJECT_KEY", "project-key")
	t.Setenv("PROJECT_HEADER", "project-header")
	f := &File{Providers: map[string]Provider{"p": {BaseURL: "https://old.invalid", Model: "m", APIKey: "old", Headers: map[string]string{"X-Auth": "old-header"}}}}
	Merge(f, &File{Providers: map[string]Provider{"p": {BaseURL: "https://new.invalid", APIKey: "$PROJECT_KEY", Headers: map[string]string{"x-auth": "$PROJECT_HEADER"}}}})
	p := f.Providers["p"]
	if len(p.Headers) != 1 {
		t.Fatal("case alias retained an inherited credential header")
	}
	m, _ := p.ResolveModel("")
	key, headers, err := p.ResolveCredentials(m, m.BaseURL, false)
	if err != nil || key != "project-key" || headers["X-Auth"] != "project-header" {
		t.Fatalf("explicit credentials rejected: %v", err)
	}
}

func TestEquivalentEndpointAndDirectOverrideRemainUsable(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient")
	f := &File{Providers: map[string]Provider{"p": {BaseURL: "https://EXAMPLE.invalid", Model: "m"}}}
	Merge(f, &File{Providers: map[string]Provider{"p": {BaseURL: "https://example.invalid/v1/chat/completions/"}}})
	p := f.Providers["p"]
	m, _ := p.ResolveModel("")
	if _, _, err := p.ResolveCredentials(m, m.BaseURL, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.ResolveCredentials(m, "https://direct-user.invalid", true); err != nil {
		t.Fatal(err)
	}
}
