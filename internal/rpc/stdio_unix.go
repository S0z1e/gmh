//go:build !windows

package rpc

import (
	"io"
	"os"
	"os/signal"
	"syscall"
)

// On Unix, Go normally exits on EPIPE from stdout. Ask for the signal so the
// writer can report the error and Run can cancel and save active work first.
func handleStdoutBrokenPipe(out io.Writer) func() {
	if out != os.Stdout {
		return func() {}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGPIPE)
	return func() { signal.Stop(signals) }
}
