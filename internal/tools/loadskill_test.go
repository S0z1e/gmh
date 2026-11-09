package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, ".agents", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSkillDispatch(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "demo", "---\nname: demo\ndescription: A demo skill.\n---\n# Demo\nDo the demo thing.\n")
	e := &Executor{Root: root}
	out, err := e.Dispatch(context.Background(), "load_skill", `{"name":"demo"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Do the demo thing.") {
		t.Fatalf("load_skill output = %q, want the skill body", out)
	}
	// Case-insensitive name.
	if _, err := e.Dispatch(context.Background(), "load_skill", `{"name":"DEMO"}`); err != nil {
		t.Fatalf("case-insensitive load failed: %v", err)
	}
	// Unknown skill names the available ones.
	_, err = e.Dispatch(context.Background(), "load_skill", `{"name":"nope"}`)
	if err == nil || !strings.Contains(err.Error(), "demo") {
		t.Fatalf("unknown skill error = %v, want it to list available skills", err)
	}
	// Empty name.
	if _, err := e.Dispatch(context.Background(), "load_skill", `{"name":""}`); err == nil {
		t.Fatal("empty skill name should error")
	}
}
