package skills

import (
	"testing"
)

func TestMatchKeywords(t *testing.T) {
	skills := []Skill{{Name: "demo"}, {Name: "code-review"}}
	cases := []struct {
		text string
		want []string
	}{
		{"use the demo skill", []string{"demo"}},
		{"DEMO it", []string{"demo"}},
		{"demonstrate this", nil},
		{"the demo.", []string{"demo"}},
		{"code-review this change", []string{"code-review"}},
		{"code-reviews are good", nil},
		{"my code-review, please", []string{"code-review"}},
		{"demo and code-review both", []string{"demo", "code-review"}},
		{"nothing relevant", nil},
	}
	for _, tc := range cases {
		got := MatchKeywords(tc.text, skills)
		var names []string
		for _, s := range got {
			names = append(names, s.Name)
		}
		if len(names) != len(tc.want) {
			t.Fatalf("MatchKeywords(%q) = %v, want %v", tc.text, names, tc.want)
		}
		for i := range names {
			if names[i] != tc.want[i] {
				t.Fatalf("MatchKeywords(%q) = %v, want %v", tc.text, names, tc.want)
			}
		}
	}
}
