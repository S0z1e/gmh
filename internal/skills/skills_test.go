package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadListsReferences(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".agents", "skills", "demo")
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: demo\ndescription: A demo skill.\n---\n# Demo\nBody here.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "references", "api.md"), []byte("API details.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "references", "examples.md"), []byte("Examples.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	found := Discover(root)
	if len(found) != 1 {
		t.Fatalf("discovered %d skills, want 1", len(found))
	}
	sk := found[0]
	loaded, err := sk.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded, "Body here.") {
		t.Fatalf("Load() missing body: %q", loaded)
	}
	if !strings.Contains(loaded, "api.md") || !strings.Contains(loaded, "examples.md") {
		t.Fatalf("Load() missing references footer: %q", loaded)
	}
	if strings.Contains(loaded, "API details.") {
		t.Fatal("Load() must not inline reference contents")
	}
	refs, err := sk.References()
	if err != nil || len(refs) != 2 || refs[0].Name != "api.md" || refs[1].Name != "examples.md" {
		t.Fatalf("References() = %v, err = %v", refs, err)
	}
	// Load one reference file.
	content, err := sk.LoadReference("api.md")
	if err != nil || content != "API details.\n" {
		t.Fatalf("LoadReference(api.md) = %q, err = %v", content, err)
	}
	// Traversal and absolute paths are refused.
	for _, bad := range []string{"../SKILL.md", "../../etc/passwd", "/etc/passwd", "sub/../../SKILL.md"} {
		if _, err := sk.LoadReference(bad); err == nil {
			t.Fatalf("LoadReference(%q) should refuse", bad)
		}
	}
	// Unknown reference.
	if _, err := sk.LoadReference("missing.md"); err == nil {
		t.Fatal("LoadReference(missing.md) should error")
	}
}

func TestLoadWithoutReferences(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".agents", "skills", "plain")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# Plain\nJust body.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sk := Discover(root)[0]
	loaded, err := sk.Load()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(loaded, "References") {
		t.Fatalf("Load() should not add a footer without references/: %q", loaded)
	}
}

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

func TestReferenceRejectsSymlinkEscape(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "references")); err != nil {
		t.Fatal(err)
	}
	sk := Skill{Path: filepath.Join(dir, "SKILL.md")}
	if _, err := sk.LoadReference("secret.md"); err == nil {
		t.Fatal("escaped reference directory")
	}
	if _, err := sk.References(); err == nil {
		t.Fatal("listed outside reference directory")
	}
}
