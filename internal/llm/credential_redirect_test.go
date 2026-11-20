package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestProviderRedirectDoesNotForwardCredentialHeaders(t *testing.T) {
	for _, api := range []string{"chat", "responses", "anthropic"} {
		t.Run(api, func(t *testing.T) {
			var calls atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
			defer other.Close()
			original := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
			}))
			defer original.Close()
			custom := &http.Client{}
			c := &Client{BaseURL: original.URL, API: api, Model: "test", APIKey: "fake-key", Headers: map[string]string{"X-Custom-Credential": "fake-header-secret"}, HTTP: custom}
			if _, _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "test"}}, nil, nil); err == nil {
				t.Fatal("redirect treated as success")
			}
			if calls.Load() != 0 {
				t.Fatal("credential-bearing request followed redirect")
			}
			if custom.CheckRedirect != nil {
				t.Fatal("shared HTTP client was mutated")
			}
		})
	}
}
