package config

import (
	"minimalharness/internal/verification"
	"testing"
)

func TestVerificationConfigMergeAndValidation(t *testing.T) {
	global := &File{Verification: &verification.Policy{Required: []verification.Check{{Name: "global", Command: "true"}}, Exclude: []string{"output"}}}
	dst := &File{}
	Merge(dst, global)
	global.Verification.Required[0].Command = "false"
	global.Verification.Exclude[0] = "changed"
	if dst.Verification.Required[0].Command != "true" || dst.Verification.Exclude[0] != "output" {
		t.Fatal("merge aliased policy")
	}
	Merge(dst, &File{})
	if len(dst.Verification.Required) != 1 {
		t.Fatal("omitted policy erased inherited checks")
	}
	Merge(dst, &File{Verification: &verification.Policy{}})
	if len(dst.Verification.Required) != 0 {
		t.Fatal("explicit empty policy did not disable checks")
	}
	path := write(t, t.TempDir(), "config.json", `{"verification":{"required":[{"name":"tests","command":"go test ./...","cwd":"."}],"exclude":["output"]}}`)
	loaded, err := Load(path)
	if err != nil || loaded.Verification == nil || loaded.Verification.Required[0].Name != "tests" {
		t.Fatalf("config=%+v err=%v", loaded, err)
	}
	loaded.Verification.Required[0].Command = ""
	if err := Validate(loaded); err == nil {
		t.Fatal("invalid check accepted by config validation")
	}
}
