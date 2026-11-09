package tui

import (
	"bytes"
	"sync"
)

// syncBuffer is a bytes.Buffer safe for concurrent use. Bubble Tea
// writes to the program output from two goroutines — the renderer's
// ticker flush and the event loop's tea.Println path — without
// synchronising them, so a plain bytes.Buffer races and can corrupt
// the captured output (found by go test -race: the ordering test
// failed with the usage line before the reply). The race is inside
// Bubble Tea v2.1.1 (Program.flush vs cursedRenderer.insertAbove);
// on a real terminal the same overlap can only interleave one frame,
// which the next repaint fixes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
