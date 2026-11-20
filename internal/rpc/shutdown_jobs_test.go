package rpc

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"minimalharness/internal/app"
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

func TestRPCQuitCanRetryFailedResultSave(t *testing.T) {
	for _, typ := range []string{"quit", "prompt"} {
		t.Run(typ, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			root := t.TempDir()
			config := `{"providers":{"mock":{"apiKey":"test","baseUrl":"https://example.invalid","api":"chat","model":"mock"}}}`
			if err := os.WriteFile(filepath.Join(root, ".harness.json"), []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			a, err := app.Setup(app.Params{WorkspaceTrust: "trust", Root: root, Mode: "auto"})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			job, err := a.Cfg.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "printf failed; exit 7"})
			if err != nil {
				t.Fatal(err)
			}
			<-job.Done()
			parked := a.Store.Path + ".parked"
			if err := os.Rename(a.Store.Path, parked); err != nil {
				t.Fatal(err)
			}
			defer os.Rename(parked, a.Store.Path)
			d := startDriver(t, a, &app.Runner{App: a, Print: func(string, ...any) {}})
			d.waitFor("ready", time.Second)
			d.send(map[string]any{"id": "failed-quit", "type": typ, "message": "/quit"})
			reply := d.waitFor("error", time.Second)
			if reply["id"] != "failed-quit" || job.Delivered() {
				t.Fatalf("failed save was acknowledged: reply=%v delivered=%t", reply, job.Delivered())
			}
			select {
			case err := <-d.done:
				t.Fatalf("server exited before quit could be retried: %v", err)
			default:
			}
			if err := os.Rename(parked, a.Store.Path); err != nil {
				t.Fatal(err)
			}
			d.quit(t)
			saved, err := session.Load(a.Store.ID)
			if err != nil || len(saved) != 1 || !job.Delivered() {
				t.Fatalf("retry lost or duplicated result: %v %+v", err, saved)
			}
		})
	}
}
