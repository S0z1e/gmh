package app

import (
	"context"
	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionJobsStayWithOwner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test")
	root := t.TempDir()
	writeMockProvider(t, root)
	a, err := Setup(Params{Root: root, Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.Store.Close() }()
	old := a.Store.ID
	job, err := a.Cfg.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "printf old"})
	if err != nil {
		t.Fatal(err)
	}
	<-job.Done()
	if _, err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if len(a.Cfg.Exec.Jobs()) != 0 {
		t.Fatal("new session inherited old jobs")
	}
	if _, err := a.LoadSession(old); err != nil {
		t.Fatal(err)
	}
	if len(a.Cfg.Exec.Jobs()) != 1 || a.Cfg.Exec.Jobs()[0] != job {
		t.Fatal("owner lost its job")
	}
}

func TestSessionPlanAndReadSnapshotsStayIsolated(t *testing.T) {
	a, _, _ := setupWithPlan(t)
	defer func() { a.Store.Close() }()
	ctx := context.Background()
	old := a.Store.ID
	if _, err := a.Plugins.Call(ctx, "plan_create", `{"title":"old plan","steps":["old step"]}`); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.Root, "edit.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Cfg.Exec.Read(tools.ReadArgs{Path: "edit.txt"}); err != nil {
		t.Fatal(err)
	}
	a.PendingImages = []llm.Image{{Path: "pending.png"}}
	a.Cfg.LastPromptTokens = 9000
	if _, err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if len(a.PendingImages) != 0 || a.Cfg.LastPromptTokens != 0 {
		t.Fatal("new session inherited turn state")
	}
	if out, err := a.Plugins.Call(ctx, "plan_show", `{}`); err != nil || out != "No plan yet." {
		t.Fatalf("new plan = %q, %v", out, err)
	}
	if _, err := a.LoadSession(old); err != nil {
		t.Fatal(err)
	}
	if out, err := a.Plugins.Call(ctx, "plan_show", `{}`); err != nil || !strings.Contains(out, "old plan") {
		t.Fatalf("restored plan = %q, %v", out, err)
	}
}

func TestRestoredToolImageStillUsesWorkspaceJail(t *testing.T) {
	a, _, _ := setupWithPlan(t)
	defer a.Store.Close()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "image.png"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(a.Root, "escape")); err != nil {
		t.Fatal(err)
	}
	a.Cfg.Exec.ImageInput = true
	msgs := []llm.Message{{Role: "user", Kind: "image", Images: []llm.Image{{Path: "escape/image.png", MediaType: "image/png", Workspace: true}}}}
	a.hydrateImages(msgs)
	if msgs[0].Images[0].Data != "" {
		t.Fatal("restored tool image escaped workspace")
	}
}

func TestSessionRenameKeepsPlanAndJobOwnership(t *testing.T) {
	a, _, _ := setupWithPlan(t)
	defer func() { a.Store.Close() }()
	old := a.Store.ID
	ctx := context.Background()
	if _, err := a.Plugins.Call(ctx, "plan_create", `{"title":"retained","steps":["one"]}`); err != nil {
		t.Fatal(err)
	}
	job, err := a.Cfg.Exec.BashBackground(ctx, tools.BashArgs{Command: "printf done"})
	if err != nil {
		t.Fatal(err)
	}
	<-job.Done()
	if err := a.RenameSession(old, "renamed"); err != nil {
		t.Fatal(err)
	}
	if out, err := a.Plugins.Call(ctx, "plan_show", `{}`); err != nil || !strings.Contains(out, "retained") {
		t.Fatalf("plan=%q,%v", out, err)
	}
	if _, err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.LoadSession("renamed"); err != nil {
		t.Fatal(err)
	}
	if jobs := a.Cfg.Exec.Jobs(); len(jobs) != 1 || jobs[0] != job {
		t.Fatal("renamed session lost its jobs")
	}
}

func TestBackgroundAuditStaysWithOriginalSession(t *testing.T) {
	a, _, _ := setupWithPlan(t)
	defer func() { a.Store.Close() }()
	oldAudit := a.Store.AuditPath()
	job, err := a.Cfg.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "sleep 0.1; printf owner-marker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	<-job.Done()
	if raw, err := os.ReadFile(oldAudit); err != nil || !strings.Contains(string(raw), "owner-marker") {
		t.Fatalf("owner audit=%q,%v", raw, err)
	}
	if raw, err := os.ReadFile(a.Store.AuditPath()); err == nil && strings.Contains(string(raw), "owner-marker") {
		t.Fatal("background result audited in new session")
	}
}

func TestSessionRenameDoesNotGrantReusedOutputDirectory(t *testing.T) {
	a, _, _ := setupWithPlan(t)
	defer func() { a.Store.Close() }()
	ctx := context.Background()
	old := a.Store.ID
	owned, err := a.Cfg.Exec.BashBackground(ctx, tools.BashArgs{Command: "printf owned"})
	if err != nil {
		t.Fatal(err)
	}
	<-owned.Done()
	if err := a.RenameSession(old, "renamed"); err != nil {
		t.Fatal(err)
	}
	if a.Cfg.Exec.OutputDir != sessionOutputDir("renamed") {
		t.Fatal("renamed session still writes into the old output directory")
	}
	fresh, err := a.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID != old {
		if err := a.RenameSession(fresh.ID, old); err != nil {
			t.Fatal(err)
		}
	}
	foreign, err := a.Cfg.Exec.BashBackground(ctx, tools.BashArgs{Command: "printf foreign"})
	if err != nil {
		t.Fatal(err)
	}
	<-foreign.Done()
	if _, err := a.LoadSession("renamed"); err != nil {
		t.Fatal(err)
	}
	if out, err := a.Cfg.Exec.Read(tools.ReadArgs{Path: owned.OutputFile}); err != nil || !strings.Contains(out, "owned") {
		t.Fatalf("owned output = %q, %v", out, err)
	}
	for _, path := range []string{foreign.OutputFile, filepath.Dir(foreign.OutputFile)} {
		if _, err := a.Cfg.Exec.Read(tools.ReadArgs{Path: path}); err == nil {
			t.Errorf("renamed session can read another session's output: %s", path)
		}
	}
}
