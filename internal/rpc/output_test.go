package rpc

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockedProtocolWriter struct {
	started, release chan struct{}
	start, close     sync.Once
}

func newBlockedProtocolWriter() *blockedProtocolWriter {
	return &blockedProtocolWriter{started: make(chan struct{}), release: make(chan struct{})}
}
func (w *blockedProtocolWriter) Write([]byte) (int, error) {
	w.start.Do(func() { close(w.started) })
	<-w.release
	return 0, io.ErrClosedPipe
}
func (w *blockedProtocolWriter) Close() error { w.close.Do(func() { close(w.release) }); return nil }

func TestProtocolOutputBackpressureDisconnectsAndBoundsQueue(t *testing.T) {
	w := newBlockedProtocolWriter()
	var calls atomic.Int32
	p := newProtocolOutput(w, func() { calls.Add(1) })
	p.enqueue([]byte("first\n"))
	<-w.started
	for i := 0; i < maxOutputFrames+10; i++ {
		p.enqueue([]byte("frame\n"))
	}
	if err := p.finish(); !errors.Is(err, errOutputBackpressure) {
		t.Fatalf("backpressure was ignored: %v", err)
	}
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("blocked writer did not stop")
	}
	p.mu.Lock()
	pending := p.pending
	p.mu.Unlock()
	if pending > maxOutputBytes || calls.Load() != 1 {
		t.Fatalf("pending=%d disconnects=%d", pending, calls.Load())
	}
}
func TestProtocolOutputDeadlineInterruptsWrite(t *testing.T) {
	w := newBlockedProtocolWriter()
	p := newProtocolOutput(w, nil)
	p.timeout = 30 * time.Millisecond
	p.enqueue([]byte("blocked\n"))
	<-w.started
	start := time.Now()
	if err := p.finish(); !errors.Is(err, errOutputBackpressure) {
		t.Fatalf("missing deadline: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("deadline did not bound shutdown")
	}
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("writer remained blocked")
	}
}

type shortProtocolWriter struct{}

func (shortProtocolWriter) Write(b []byte) (int, error) { return len(b) - 1, nil }
func TestProtocolOutputRejectsShortWrites(t *testing.T) {
	p := newProtocolOutput(shortProtocolWriter{}, nil)
	p.enqueue([]byte("frame\n"))
	if err := p.finish(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write accepted: %v", err)
	}
}
