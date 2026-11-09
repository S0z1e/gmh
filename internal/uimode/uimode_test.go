package uimode

import (
	"testing"
)

func TestDecide(t *testing.T) {
	cases := []struct {
		name  string
		flag  string
		isTTY bool
		env   map[string]string
		want  string
	}{
		{"explicit plain wins over tty", "plain", true, nil, "plain"},
		{"explicit tui on tty", "tui", true, nil, "tui"},
		{"explicit tui without tty falls back", "tui", false, nil, "plain"},
		{"explicit tui ignores NO_COLOR", "tui", true, map[string]string{"NO_COLOR": "1"}, "tui"},
		{"auto on tty", "auto", true, nil, "tui"},
		{"auto without tty", "auto", false, nil, "plain"},
		{"auto with NO_COLOR", "auto", true, map[string]string{"NO_COLOR": "1"}, "plain"},
		{"auto with empty NO_COLOR does not count", "auto", true, map[string]string{"NO_COLOR": ""}, "tui"},
		{"auto with TERM=dumb", "auto", true, map[string]string{"TERM": "dumb"}, "plain"},
		{"NO_COLOR beats TERM", "auto", true, map[string]string{"NO_COLOR": "1", "TERM": "xterm"}, "plain"},
		{"default flag is auto", "", true, nil, "tui"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", "")
			t.Setenv("TERM", "")
			// Unset by setting empty, then set the case's values.
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			// Empty NO_COLOR must behave as unset for the
			// "empty NO_COLOR" case; t.Setenv above already did that.
			got, err := Decide(tc.flag, tc.isTTY)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("Decide(%q, tty=%v) = %q, want %q", tc.flag, tc.isTTY, got, tc.want)
			}
		})
	}
}

func TestDecideInvalidFlag(t *testing.T) {
	if _, err := Decide("fancy", true); err == nil {
		t.Fatal("expected an error for an invalid --ui value")
	}
}
