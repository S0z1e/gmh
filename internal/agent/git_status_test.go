package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/tools"
	"minimalharness/internal/verification"
)

func TestGitStatusIsReadOnlyAndHonorsHostApproval(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for _, tc := range []struct {
		name, mode      string
		sandbox, permit bool
		ask             int
	}{
		{name: "suggest approval", mode: "suggest", permit: true, ask: 1},
		{name: "host approval in sandbox", mode: "auto", sandbox: true, permit: true, ask: 1},
		{name: "denied host approval in sandbox", mode: "auto", sandbox: true, ask: 1},
		{name: "trusted auto", mode: "auto", permit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := guidanceFixture(t)
			root := cfg.Exec.Root
			git := exec.Command("git", "init", "-b", "fixture-status-branch")
			git.Dir = root
			if out, err := git.CombinedOutput(); err != nil {
				t.Fatalf("git init: %v (%s)", err, out)
			}
			if err := os.Symlink(".git/HEAD", filepath.Join(root, "unsupported-snapshot")); err != nil {
				t.Skipf("fixture needs a symlink in the workspace: %v", err)
			}
			cfg.Verification = &verification.Policy{Required: []verification.Check{{Name: "test", Command: "true"}}}
			cfg.Mode, cfg.Exec.Sandbox = tc.mode, tc.sandbox
			asks := 0
			cfg.Ask = func(req ApprovalRequest) (bool, error) {
				asks++
				if req.ToolName != tools.GitStatusTool {
					t.Errorf("unexpected approval: %+v", req)
				}
				return tc.permit, nil
			}
			_, requests, err := runVerification(t, cfg, toolCallSSE("status", tools.GitStatusTool, `{}`))
			if err != nil || len(requests) != 2 || asks != tc.ask {
				t.Fatalf("read-only query affected completion: calls=%d approvals=%d err=%v", len(requests), asks, err)
			}
			if tc.permit != strings.Contains(requests[1], "fixture-status-branch") {
				t.Fatalf("query dispatch did not honor approval: allowed=%t", tc.permit)
			}
		})
	}
}
