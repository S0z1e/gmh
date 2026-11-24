package rpc

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const maxOutputFrames = 1024
const maxOutputBytes = 16 << 20
const maxOutputFrameBytes = 8 << 20
const outputWriteTimeout = 5 * time.Second

var errOutputBackpressure = errors.New("rpc client is not consuming output")

// protocolOutput owns the single writer. Producers never wait behind stdout;
// exceeding either bound disconnects the client and cancels active work. No
// protocol frame is silently dropped while the connection remains usable.
// A closable writer (stdio and pipes) is closed on error/timeout to interrupt
// an in-flight write. Custom io.Writers must make Close interrupt Write too.
type protocolOutput struct {
	mu        sync.Mutex
	writer    io.Writer
	queue     chan []byte
	pending   int
	closed    bool
	err       error
	failed    chan struct{}
	done      chan struct{}
	failOnce  sync.Once
	onFailure func()
	timeout   time.Duration
}

func newProtocolOutput(w io.Writer, onFailure func()) *protocolOutput {
	p := &protocolOutput{writer: w, queue: make(chan []byte, maxOutputFrames), failed: make(chan struct{}), done: make(chan struct{}), onFailure: onFailure, timeout: outputWriteTimeout}
	go p.run()
	return p
}
func (p *protocolOutput) fail(err error) {
	p.failOnce.Do(func() {
		p.mu.Lock()
		p.err = err
		close(p.failed)
		p.mu.Unlock()
		if p.onFailure != nil {
			p.onFailure()
		}
		if c, ok := p.writer.(io.Closer); ok {
			_ = c.Close()
		}
	})
}
func (p *protocolOutput) pendingFrames() int {
	return len(p.queue)
}

func (p *protocolOutput) enqueue(b []byte) {
	p.mu.Lock()
	if p.closed || p.err != nil {
		p.mu.Unlock()
		return
	}
	if len(b) > maxOutputFrameBytes || len(b) > maxOutputBytes-p.pending {
		p.mu.Unlock()
		p.fail(errOutputBackpressure)
		return
	}
	p.pending += len(b)
	select {
	case p.queue <- b:
		p.mu.Unlock()
	default:
		p.pending -= len(b)
		p.mu.Unlock()
		p.fail(errOutputBackpressure)
	}
}
func (p *protocolOutput) run() {
	defer close(p.done)
	for b := range p.queue {
		select {
		case <-p.failed:
			return
		default:
		}
		timer := time.AfterFunc(p.timeout, func() { p.fail(fmt.Errorf("rpc output write timed out: %w", errOutputBackpressure)) })
		n, err := p.writer.Write(b)
		timer.Stop()
		if err == nil && n != len(b) {
			err = io.ErrShortWrite
		}
		if err != nil {
			p.fail(fmt.Errorf("write rpc output: %w", err))
			return
		}
		p.mu.Lock()
		p.pending -= len(b)
		p.mu.Unlock()
	}
}
func (p *protocolOutput) finish() error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.queue)
	}
	p.mu.Unlock()
	select {
	case <-p.done:
	case <-p.failed:
	case <-time.After(p.timeout):
		p.fail(fmt.Errorf("rpc output flush timed out: %w", errOutputBackpressure))
	}
	p.mu.Lock()
	err := p.err
	p.mu.Unlock()
	if err != nil {
		for range p.queue {
		}
	}
	return err
}
