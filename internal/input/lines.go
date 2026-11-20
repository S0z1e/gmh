// Package input owns one scanner for a stream shared by prompts and the REPL.
package input

import (
	"bufio"
	"context"
	"io"
	"sync"
)

type line struct {
	text string
	err  error
}
type Lines struct {
	scanner      *bufio.Scanner
	lines        chan line
	stop         chan struct{}
	start, close sync.Once
}

func New(reader io.Reader) *Lines {
	s := bufio.NewScanner(reader)
	s.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return &Lines{scanner: s, lines: make(chan line), stop: make(chan struct{})}
}

// Read cancellation leaves the next line available to the next prompt. Only
// the reader goroutine touches the scanner, so a cancelled wait never competes
// with a subsequent REPL read. Close stops delivery; underlying blocking stdin
// reads live for the process lifetime (their owner controls the descriptor).
func (l *Lines) Read(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	l.start.Do(func() {
		go func() {
			defer close(l.lines)
			for l.scanner.Scan() {
				select {
				case l.lines <- line{text: l.scanner.Text()}:
				case <-l.stop:
					return
				}
			}
			err := l.scanner.Err()
			if err == nil {
				err = io.EOF
			}
			select {
			case l.lines <- line{err: err}:
			case <-l.stop:
			}
		}()
	})
	select {
	case next, ok := <-l.lines:
		if !ok {
			return "", io.EOF
		}
		return next.text, next.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-l.stop:
		return "", io.EOF
	}
}

func (l *Lines) Close() { l.close.Do(func() { close(l.stop) }) }
