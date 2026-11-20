package verification

import (
	"strings"
	"testing"
)

func TestPolicyValidation(t *testing.T) {
	for _, p := range []*Policy{nil, {}, {Required: []Check{{Name: "test", Command: "go test ./..."}}, Exclude: []string{"output", "node_modules"}}} {
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []*Policy{
		{Required: []Check{{Name: " ", Command: "true"}}},
		{Required: []Check{{Name: "test", Command: " "}}},
		{Required: []Check{{Name: "test", Command: "true", Cwd: "bad\x00dir"}}},
		{Required: []Check{{Name: "test", Command: "true"}, {Name: "test", Command: "false"}}},
		{Required: make([]Check, 33)},
		{Required: []Check{{Name: "test", Command: strings.Repeat("x", 16385)}}},
		{Exclude: []string{".."}}, {Exclude: []string{"."}}, {Exclude: []string{"/outside"}},
		{Exclude: make([]string, MaxExclusions+1)},
	} {
		if err := p.Validate(); err == nil {
			t.Fatalf("accepted invalid policy: %+v", p)
		}
	}
}
