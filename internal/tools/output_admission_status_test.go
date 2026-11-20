package tools

import (
	"context"
	"errors"
	"testing"
)

func TestDispatchPreservesBashAdmissionStatus(t *testing.T) {
	e := &Executor{Root: t.TempDir(), OutputDir: t.TempDir(), OutputMaxBytes: 1}
	if _, err := e.Dispatch(context.Background(), "bash", `{"command":"printf x"}`); err != nil {
		t.Fatal(err)
	}
	res, err := e.Dispatch(context.Background(), "bash", `{"command":"printf x"}`)
	var admission *BashAdmissionError
	if !errors.As(err, &admission) {
		t.Fatalf("want admission error, got %v", err)
	}
	if res.Status != ResultNotExecuted {
		t.Fatalf("admission status=%q, want %q", res.Status, ResultNotExecuted)
	}
}
