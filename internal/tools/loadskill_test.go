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
	if !strings.Contains(out.Text, "Do the demo thing.") {
		t.Fatalf("load_skill output = %q, want the skill body", out.Text)
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

func TestLoadSkillReference(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "demo", "---\nname: demo\ndescription: A demo skill.\n---\n# Demo\nDo the demo thing.\n")
	refDir := filepath.Join(root, ".agents", "skills", "demo", "references")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refDir, "api.md"), []byte("REF-MARKER-456\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Root: root}
	// Loading the skill lists the reference but does not inline it.
	out, err := e.Dispatch(context.Background(), "load_skill", `{"name":"demo"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "api.md") {
		t.Fatalf("load_skill output should list references: %q", out.Text)
	}
	if strings.Contains(out.Text, "REF-MARKER-456") {
		t.Fatal("load_skill must not inline reference contents")
	}
	// Loading one reference returns its content.
	out, err = e.Dispatch(context.Background(), "load_skill", `{"name":"demo","reference":"api.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "REF-MARKER-456\n" {
		t.Fatalf("load_skill reference = %q, want the file content", out.Text)
	}
	// Unknown reference and traversal are refused.
	if _, err := e.Dispatch(context.Background(), "load_skill", `{"name":"demo","reference":"nope.md"}`); err == nil {
		t.Fatal("unknown reference should error")
	}
	if _, err := e.Dispatch(context.Background(), "load_skill", `{"name":"demo","reference":"../SKILL.md"}`); err == nil {
		t.Fatal("traversal reference should error")
	}
}
