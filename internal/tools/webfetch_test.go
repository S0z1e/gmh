package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// noProxyClient reaches httptest servers directly, ignoring the VM's
// proxy env vars (the test servers are loopback).
func noProxyClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil}}
}

func TestWebfetchHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Example Domain</title><style>.x{color:red}</style><script>var secret = 1;</script><!-- a comment --></head><body><h1>Example Domain</h1><p>This domain is for use in examples &amp; docs.</p></body></html>`)
	}))
	defer srv.Close()
	e := &Executor{Root: t.TempDir(), HTTPClient: noProxyClient()}
	out, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Example Domain", "This domain is for use in examples & docs."} {
		if !strings.Contains(out, want) {
			t.Fatalf("html fetch = %q, want %q", out, want)
		}
	}
	for _, nope := range []string{"secret", ".x{", "a comment", "<h1>", "<p>"} {
		if strings.Contains(out, nope) {
			t.Fatalf("html fetch = %q, must not contain %q", out, nope)
		}
	}
}

func TestWebfetchTextAndJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plain":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "just text\nsecond line\n")
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"hello":"world"}`)
		}
	}))
	defer srv.Close()
	e := &Executor{Root: t.TempDir(), HTTPClient: noProxyClient()}
	out, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL + "/plain"})
	if err != nil || out != "just text\nsecond line" {
		t.Fatalf("plain fetch = %q, err %v", out, err)
	}
	out, err = e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL + "/json"})
	if err != nil || out != `{"hello":"world"}` {
		t.Fatalf("json fetch = %q, err %v", out, err)
	}
}

func TestWebfetchRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "arrived")
	}))
	defer srv.Close()
	e := &Executor{Root: t.TempDir(), HTTPClient: noProxyClient()}
	out, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL + "/old"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "Redirected to "+srv.URL+"/new") || !strings.Contains(out, "arrived") {
		t.Fatalf("redirect fetch = %q", out)
	}
}

func TestWebfetchErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/404":
			http.NotFound(w, r)
		case "/png":
			w.Header().Set("Content-Type", "image/png")
			fmt.Fprint(w, "\x89PNG....")
		}
	}))
	defer srv.Close()
	e := &Executor{Root: t.TempDir(), HTTPClient: noProxyClient()}
	if _, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL + "/404"}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404 err = %v", err)
	}
	if _, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL + "/png"}); err == nil || !strings.Contains(err.Error(), "unsupported content type") {
		t.Fatalf("png err = %v", err)
	}
	if _, err := e.Webfetch(context.Background(), WebfetchArgs{URL: "ftp://example.com/x"}); err == nil || !strings.Contains(err.Error(), "non-http") {
		t.Fatalf("ftp err = %v", err)
	}
	if _, err := e.Webfetch(context.Background(), WebfetchArgs{URL: "not a url"}); err == nil {
		t.Fatal("garbage URL should error")
	}
}

func TestWebfetchCapsOversize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("a", 100<<10))
	}))
	defer srv.Close()
	e := &Executor{Root: t.TempDir(), HTTPClient: noProxyClient()}
	out, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > MaxWebfetchBytes+100 || !strings.Contains(out, "[truncated") {
		t.Fatalf("oversize fetch len = %d, want the cap + marker", len(out))
	}
}

func TestDispatchWebfetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "via dispatch")
	}))
	defer srv.Close()
	e := &Executor{Root: t.TempDir(), HTTPClient: noProxyClient()}
	res, err := e.Dispatch(context.Background(), "webfetch", `{"url":`+fmt.Sprintf("%q", srv.URL)+`}`)
	if err != nil || res.Text != "via dispatch" {
		t.Fatalf("dispatch webfetch = %+v, err %v", res, err)
	}
}

func TestHTMLToText(t *testing.T) {
	cases := []struct{ in, want string }{
		{`<p>one</p><p>two</p>`, "one\n\ntwo"},
		{`<div>a <b>bold</b> c</div>`, "a bold c"},
		{`x<!-- c > d -->y`, "x y"},
		{`a &lt;b&gt; &amp; &quot;q&quot;`, `a <b> & "q"`},
		{`<ul><li>one</li><li>two</li></ul>`, "one\n\ntwo"},
	}
	for _, c := range cases {
		if got := htmlToText(c.in); got != c.want {
			t.Fatalf("htmlToText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
