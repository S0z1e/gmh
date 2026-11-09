package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The full channel path: Approver.Ask sends the request into a running
// program and blocks until a y/n keypress answers it.
func TestApproverAskAnsweredByKey(t *testing.T) {
	run := func(key rune, want bool) {
		// Headless program: no TTY in tests, so feed input from a reader
		// and discard output instead of opening /dev/tty.
		prog := tea.NewProgram(NewModel(""), tea.WithoutRenderer(), tea.WithInput(strings.NewReader("")), tea.WithOutput(io.Discard))
		done := make(chan struct{})
		go func() {
			_, _ = prog.Run()
			close(done)
		}()
		approver := NewApprover(prog)
		result := make(chan bool, 1)
		go func() {
			ok, err := approver.Ask("Run write?")
			if err != nil {
				t.Errorf("Ask error: %v", err)
			}
			result <- ok
		}()
		// Let the request reach the model, then answer it.
		time.Sleep(100 * time.Millisecond)
		prog.Send(tea.KeyPressMsg{Text: string(key), Code: key})
		select {
		case got := <-result:
			if got != want {
				t.Fatalf("Ask = %v, want %v", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Ask did not return after the keypress")
		}
		prog.Quit()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("program did not quit")
		}
	}
	run('y', true)
	run('n', false)
}
