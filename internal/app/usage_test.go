package app

import (
	"context"
	"minimalharness/internal/llm"
	"testing"
)

func TestUsageLifetimeAndRunAcrossResumeAndRateChanges(t *testing.T) {
	srv := mockServer(t)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "fake")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	root := t.TempDir()
	writeMockProvider(t, root)
	a, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Mode: "auto", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	rates := llm.Rates{Configured: true, InputPerMTok: 1000, OutputPerMTok: 2000, Source: "first schedule"}
	a.Rates = rates
	a.Cfg.UsageRates = rates
	r := &Runner{App: a, Print: func(string, ...any) {}}
	r.Handle(context.Background(), "first")
	if r.LastErr != nil || r.SessionUsage.TotalTokens != 15 || r.RunUsage.TotalTokens != 15 || r.SessionCost != 0.02 {
		t.Fatalf("first: %+v err=%v", r.SessionMetering, r.LastErr)
	}
	id := a.Store.ID
	if _, err := r.ResetSession(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResumeSession(id); err != nil {
		t.Fatal(err)
	}
	if r.SessionUsage.TotalTokens != 15 || r.RunUsage.TotalTokens != 0 || r.SessionCost != 0.02 {
		t.Fatalf("resume: %+v run=%+v cost=%v", r.SessionMetering, r.RunUsage, r.SessionCost)
	}
	a.Rates = llm.Rates{Configured: true, InputPerMTok: 3000, OutputPerMTok: 4000, Source: "second schedule"}
	a.Cfg.UsageRates = a.Rates
	r.Handle(context.Background(), "second")
	if r.LastErr != nil || r.SessionUsage.TotalTokens != 30 || r.RunUsage.TotalTokens != 15 || r.SessionCost != 0.07 || r.RunCost != 0.05 {
		t.Fatalf("second: %+v run=%+v cost=%v/%v err=%v", r.SessionMetering, r.RunUsage, r.SessionCost, r.RunCost, r.LastErr)
	}
	if _, _, err := r.Rollback(1); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Setup(Params{WorkspaceTrust: "trust", Root: root, Resume: id, Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	r = &Runner{App: a, Print: func(string, ...any) {}}
	if err := r.RefreshUsage(); err != nil {
		t.Fatal(err)
	}
	if r.SessionUsage.TotalTokens != 30 || r.RunUsage.TotalTokens != 0 || r.SessionCost != 0.07 || r.SessionMetering.Attempts != 2 {
		t.Fatalf("cold resume: %+v run=%+v cost=%v", r.SessionMetering, r.RunUsage, r.SessionCost)
	}
}
