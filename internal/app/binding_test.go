package app

import (
	"minimalharness/internal/llm"
	"os"
	"testing"
)

func TestSessionRejectsOtherWorkspace(t *testing.T) {
	for _, via := range []string{"resume", "session", "switch"} {
		t.Run(via, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("OPENAI_API_KEY", "test-key")
			first, second := t.TempDir(), t.TempDir()
			writeMockProvider(t, first)
			writeMockProvider(t, second)
			a, err := Setup(Params{Root: first, WorkspaceTrust: "trust"})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Store.Append(llm.Message{Role: "user", Content: "edit relative-path.txt"}); err != nil {
				t.Fatal(err)
			}
			id, path := a.Store.ID, a.Store.Path
			a.Store.Close()
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			params := Params{Root: second, WorkspaceTrust: "trust"}
			switch via {
			case "resume":
				params.Resume = id
			case "session":
				params.SessionID = id
			}
			b, err := Setup(params)
			if via == "switch" && err == nil {
				defer b.Store.Close()
				active := b.Store
				_, err = b.LoadSession(id)
				if err == nil {
					t.Fatal("cross-workspace session switch accepted")
				}
				if b.Store != active {
					t.Fatal("failed switch changed active store")
				}
			} else {
				if err == nil {
					b.Store.Close()
					t.Fatal("cross-workspace session accepted")
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("rejected resume changed transcript")
			}
		})
	}
}

func TestSessionExplicitRebindAndSameWorkspaceReload(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	first, second := t.TempDir(), t.TempDir()
	writeMockProvider(t, first)
	writeMockProvider(t, second)
	a, err := Setup(Params{Root: first, WorkspaceTrust: "trust"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Append(llm.Message{Role: "user", Content: "kept"}); err != nil {
		t.Fatal(err)
	}
	id := a.Store.ID
	a.Store.Close()
	b, err := Setup(Params{Root: second, WorkspaceTrust: "trust", Resume: id, RebindSession: true})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Store.Close()
	if len(b.History) != 1 || b.History[0].Content != "kept" {
		t.Fatalf("history=%+v", b.History)
	}
	if _, err := b.LoadSession(id); err != nil {
		t.Fatal(err)
	}
	b.Root = first
	if _, err := b.LoadSession(id); err == nil {
		t.Fatal("active session bypassed workspace check")
	}
	if other, err := Setup(Params{Root: first, RebindSession: true}); err == nil {
		other.Store.Close()
		t.Fatal("rebind without resume accepted")
	}
}

func TestSetupFailureReleasesSessionWriter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := t.TempDir()
	writeMockProvider(t, root)
	t.Setenv("PATH", t.TempDir()) // no bwrap: fail after opening the session
	if a, err := Setup(Params{Root: root, WorkspaceTrust: "trust", SessionID: "failed", Sandbox: true}); err == nil {
		a.Store.Close()
		t.Fatal("sandbox setup unexpectedly succeeded")
	}
	a, err := Setup(Params{Root: root, SessionID: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	a.Store.Close()
}

func TestResumeMissingSessionDoesNotCreateOne(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := t.TempDir()
	writeMockProvider(t, root)
	for _, rebind := range []bool{false, true} {
		a, err := Setup(Params{Root: root, WorkspaceTrust: "trust", Resume: "missing", RebindSession: rebind})
		if err == nil {
			a.Store.Close()
			t.Fatal("resumed missing session")
		}
	}
}
