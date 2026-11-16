package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
)

func mutationFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{".git", ".agents", "real-plugins", "public-dir", "nested"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{".harness.json", ".git/config", "real-plugins/plugin.js", "public", "public-dir/config"} {
		if err := os.WriteFile(filepath.Join(root, path), []byte("KEEP"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for alias, target := range map[string]string{"settings": ".harness.json", "git-link": ".git", "plugins-link": ".agents/plugins", ".agents/plugins": "../real-plugins", "nested/settings": "../.harness.json"} {
		if err := os.Symlink(target, filepath.Join(root, alias)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	return root
}

func TestProtectedMutationAliasesRequireApprovalInAutoMode(t *testing.T) {
	for _, tc := range []struct{ path, target string }{
		{"settings", ".harness.json"}, {"nested/./settings", ".harness.json"}, {"git-link/config", ".git/config"}, {"plugins-link/plugin.js", "real-plugins/plugin.js"}, {"real-plugins/plugin.js", "real-plugins/plugin.js"},
	} {
		for _, name := range []string{"write", "edit"} {
			t.Run(name+tc.path, func(t *testing.T) {
				root := mutationFixture(t)
				asks := 0
				cfg := &Config{Client: &llm.Client{APIKey: "test", Model: "test", API: "chat"}, Exec: &tools.Executor{Root: root}, Tools: ToolDefs(), Mode: "auto", MaxSteps: 3,
					Ask: func(req ApprovalRequest) (bool, error) {
						asks++
						if req.TargetPath != filepath.FromSlash(tc.target) || !strings.Contains(req.Prompt(), filepath.FromSlash(tc.target)) {
							t.Errorf("approval omitted resolved target: %+v", req)
						}
						return false, nil
					},
				}
				args := `{"path":"` + tc.path + `","content":"CHANGED","overwrite":true}`
				if name == "edit" {
					args = `{"path":"` + tc.path + `","oldText":"KEEP","newText":"CHANGED"}`
				}
				_, body := runTwoTurns(t, cfg, toolCallSSE("call", name, args))
				data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(tc.target)))
				if asks != 1 || err != nil || string(data) != "KEEP" || !strings.Contains(body, "User denied") {
					t.Fatalf("protected alias bypassed: asks=%d data=%q err=%v body=%s", asks, data, err, body)
				}
			})
		}
	}
}

func TestMutationRetargetedDuringApprovalIsRefused(t *testing.T) {
	for _, tc := range []struct{ path, link, initial, replacement string }{
		{"alias", "alias", "public", ".harness.json"},
		{"alias-dir/config", "alias-dir", "public-dir", ".git"},
		{"alias", "alias", ".harness.json", "public"},
	} {
		for _, name := range []string{"write", "edit"} {
			t.Run(name+tc.initial, func(t *testing.T) {
				root := mutationFixture(t)
				if err := os.Symlink(tc.initial, filepath.Join(root, tc.link)); err != nil {
					t.Fatal(err)
				}
				cfg := &Config{Client: &llm.Client{APIKey: "test", Model: "test", API: "chat"}, Exec: &tools.Executor{Root: root}, Tools: ToolDefs(), Mode: "suggest", MaxSteps: 3,
					Ask: func(ApprovalRequest) (bool, error) {
						if err := os.Remove(filepath.Join(root, tc.link)); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(tc.replacement, filepath.Join(root, tc.link)); err != nil {
							t.Fatal(err)
						}
						return true, nil
					},
				}
				args := `{"path":"` + tc.path + `","content":"CHANGED","overwrite":true}`
				if name == "edit" {
					args = `{"path":"` + tc.path + `","oldText":"KEEP","newText":"CHANGED"}`
				}
				_, body := runTwoTurns(t, cfg, toolCallSSE("call", name, args))
				if !strings.Contains(body, "target changed") {
					t.Fatalf("retargeting not refused: %s", body)
				}
				for _, path := range []string{".harness.json", ".git/config", "public", "public-dir/config"} {
					data, err := os.ReadFile(filepath.Join(root, path))
					if err != nil || string(data) != "KEEP" {
						t.Fatalf("wrong mutation %s: %q %v", path, data, err)
					}
				}
			})
		}
	}
}
