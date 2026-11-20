//go:build !windows

package rpc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestRPCInheritedStdoutDisconnectCleansUp(t *testing.T) {
	if os.Getenv("GMH_RPC_PIPE_CHILD") == "1" {
		s, _ := usageRegressionServer(t, false)
		canceled := make(chan struct{})
		s.app.Cfg.Client.HTTP.Transport = usageRegressionTransport(func(r *http.Request) (*http.Response, error) {
			fmt.Fprintln(os.Stderr, "provider started")
			<-r.Context().Done()
			close(canceled)
			return nil, r.Context().Err()
		})
		err := Run(s.app, s.runner, os.Stdin, os.Stdout)
		if err == nil || !strings.Contains(err.Error(), "write rpc output") {
			os.Exit(3)
		}
		select {
		case <-canceled:
		case <-time.After(time.Second):
			os.Exit(4)
		}
		if _, err := s.app.Store.UsageTotals(); !errors.Is(err, os.ErrClosed) {
			os.Exit(5)
		}
		os.Exit(0) // The Go test driver cannot print to this intentionally broken pipe.
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRPCInheritedStdoutDisconnectCleansUp$")
	cmd.Env = append(os.Environ(), "GMH_RPC_PIPE_CHILD=1", "TMPDIR="+t.TempDir())
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	reader := bufio.NewReader(out)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(in, "{\"type\":\"prompt\",\"message\":\"start\"}\n"); err != nil {
		t.Fatal(err)
	}
	diagnostics := bufio.NewScanner(stderr)
	for diagnostics.Scan() && diagnostics.Text() != "provider started" {
	}
	if diagnostics.Text() != "provider started" {
		t.Fatal("provider did not start")
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(in, "{\"type\":\"get_state\"}\n"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child failed to clean up after inherited stdout disconnected: %v", err)
	}
}
