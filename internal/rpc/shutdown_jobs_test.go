package rpc

import (
	"bytes"
	"context"
	"io"
	"testing"

	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestRPCShutdownStopsJobsAndSavesResults(t *testing.T) {
	for _, input := range []string{"", "{\"id\":\"quit\",\"type\":\"quit\"}\n"} {
		t.Run(input, func(t *testing.T) {
			a, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "unused"}})
			job, err := a.Cfg.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "printf closing; exec sleep 30", Background: true})
			if err != nil {
				t.Fatal(err)
			}
			defer job.Cancel()
			if err := Run(a, runner, bytes.NewBufferString(input), io.Discard); err != nil {
				t.Fatal(err)
			}
			select {
			case <-job.Done():
			default:
				t.Fatal("RPC returned while owning a running job")
			}
			saved, err := session.Load(a.Store.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !job.Delivered() || len(saved) != 1 || saved[0].Kind != "job" {
				t.Fatalf("RPC shutdown lost its result: %+v", saved)
			}
		})
	}
}
