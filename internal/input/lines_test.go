package input

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestCancelledPromptLeavesInputForNextTurn(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	lines := New(reader)
	defer lines.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := lines.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() { _, err := io.WriteString(writer, "next task\ny\n"); written <- err }()
	for _, want := range []string{"next task", "y"} {
		got, err := lines.Read(context.Background())
		if err != nil || got != want {
			t.Fatalf("got=%q want=%q err=%v", got, want, err)
		}
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}
