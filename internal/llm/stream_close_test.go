package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type shutdownTransport func(*http.Request) (*http.Response, error)

func (f shutdownTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Like a response body whose EOF is pending in the HTTP transport, Close must
// wait for a concurrent Read. Only cancelling the request releases that Read.
type shutdownBody struct {
	*strings.Reader
	ctx     context.Context
	reading chan struct{}
	exited  chan struct{}
}

func (b *shutdownBody) Read(p []byte) (int, error) {
	if b.Len() > 0 {
		return b.Reader.Read(p)
	}
	close(b.reading)
	<-b.ctx.Done()
	close(b.exited)
	return 0, b.ctx.Err()
}

func (b *shutdownBody) Close() error {
	<-b.exited
	return nil
}

func TestStreamCancelsRequestBeforeClosingBody(t *testing.T) {
	for _, api := range []string{"chat", "responses", "anthropic"} {
		t.Run(api, func(t *testing.T) {
			payload := map[string]string{
				"chat":      "data: [DONE]\n\n",
				"responses": "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\ndata: [DONE]\n\n",
				"anthropic": "data: {\"type\":\"message_stop\"}\n\ndata: [DONE]\n\n",
			}[api]
			reading := make(chan struct{})
			exited := make(chan struct{})
			client := &Client{APIKey: "test-key", Model: "test", API: api, HTTP: &http.Client{Transport: shutdownTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &shutdownBody{strings.NewReader(payload), r.Context(), reading, exited}}, nil
			})}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				result <- client.Stream(ctx, nil, nil, nil, func(Message, Usage) { <-reading }, nil)
			}()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				cancel()
				<-result
				t.Fatal("completed stream waits for body Close before cancelling its blocked reader")
			}
		})
	}
}

// Completion markers end the SSE protocol before the line pump necessarily
// reaches EOF. Repeating requests on one transport exercises reader shutdown
// together with keep-alive reuse, rather than only testing a single response.
func TestStreamRepeatedCompletion(t *testing.T) {
	for _, api := range []string{"chat", "responses", "anthropic"} {
		t.Run(api, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				switch api {
				case "chat":
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n")
				case "responses":
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
				case "anthropic":
					fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
				}
				w.(http.Flusher).Flush()
			}))
			defer srv.Close()
			client := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test", API: api, HTTP: &http.Client{Timeout: 10 * time.Second}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			completed := make(chan error, 1)
			go func() {
				for i := 0; i < 200; i++ {
					msg, _, err := client.Chat(ctx, []Message{{Role: "user", Content: "hi"}}, nil, nil)
					if err != nil {
						completed <- fmt.Errorf("request %d: %w", i, err)
						return
					}
					if msg.Content != "OK" {
						completed <- fmt.Errorf("request %d: content=%q", i, msg.Content)
						return
					}
				}
				completed <- nil
			}()
			select {
			case err := <-completed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				cancel()
				srv.CloseClientConnections()
				t.Fatal("completed SSE response hung during reader shutdown")
			}
		})
	}
}
