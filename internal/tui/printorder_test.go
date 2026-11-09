package tui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// A single PrintMsg with embedded newlines keeps its lines in order.
// (Two back-to-back PrintMsgs do NOT: consecutive tea.Println calls
// reverse their order in Bubble Tea v2 — verified with the old
// prog.Println path, where the session usage line printed above the
// turn line. Callers must combine lines into one PrintMsg.)
func TestPrintMsgKeepsEmbeddedLineOrder(t *testing.T) {
	var out bytes.Buffer
	prog := tea.NewProgram(NewModel(""), tea.WithInput(strings.NewReader("")), tea.WithOutput(&out))
	done := make(chan struct{})
	go func() {
		_, _ = prog.Run()
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	prog.Send(PrintMsg{Text: "AAA-first\nBBB-second"})
	time.Sleep(300 * time.Millisecond)
	prog.Quit()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("program did not quit")
	}
	s := out.String()
	iA := strings.Index(s, "AAA-first")
	iB := strings.Index(s, "BBB-second")
	if iA < 0 || iB < 0 || iA > iB {
		t.Fatalf("AAA at %d, BBB at %d in %q", iA, iB, s)
	}
}
