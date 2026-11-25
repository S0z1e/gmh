package rpc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
)

func TestResumeSessionBusyAndStealRefusesUnprovenOwner(t *testing.T) {
	a, runner, root := setupTestApp(t, "auto", nil)
	owner, err := session.Open("held", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := owner.Append(llm.Message{Role: "user", Content: "kept"}); err != nil {
		t.Fatal(err)
	}

	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "busy", "type": "resume_session", "session": "held"})
	errResp := d.waitFor("error", 5*time.Second)
	if errResp["id"] != "busy" || !strings.Contains(fmt.Sprint(errResp["error"]), "already open for writing") {
		t.Fatalf("resume of a busy session = %v", errResp)
	}
	if a.Store.ID == "held" {
		t.Fatal("busy resume switched the RPC session")
	}

	d.send(map[string]any{"id": "slash", "type": "prompt", "message": "/steal held"})
	errResp = d.waitFor("error", 5*time.Second)
	msg := fmt.Sprint(errResp["error"])
	if errResp["id"] != "slash" || !(strings.Contains(msg, "cannot prove process identity") || strings.Contains(msg, "still active") || strings.Contains(msg, "current process")) {
		t.Fatalf("slash steal = %v", errResp)
	}
	if a.Store.ID == "held" {
		t.Fatal("refused steal switched the RPC session")
	}
	d.quit(t)
}
