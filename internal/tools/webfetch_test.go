package tools

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	// Default format is Markdown: the heading survives as "# ...".
	for _, want := range []string{"# Example Domain", "This domain is for use in examples & docs."} {
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

func TestWebfetchFormatText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<h1>Example Domain</h1><p>plain words</p>`)
	}))
	defer srv.Close()
	e := &Executor{Root: t.TempDir(), HTTPClient: noProxyClient()}
	out, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL, Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "Example Domain\n\nplain words" {
		t.Fatalf("text fetch = %q", out)
	}
	if _, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL, Format: "yaml"}); err == nil {
		t.Fatal("unknown format should error")
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
	// 100 KiB of plain text is no longer cut at the 64 KiB fetch
	// cap: the cap now applies after conversion, as 30k-character
	// pages, and offset reads on through the rest.
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
	if !strings.Contains(out, "[showing characters 0-30000 of 102400; use offset=30000 to read on]") {
		t.Fatalf("oversize fetch = %q..., want the paging footer", out[:120])
	}
	if got := len([]rune(out)); got > MaxWebfetchPageChars+200 {
		t.Fatalf("oversize fetch len = %d, want one page + footer", got)
	}
}

func TestWebfetchPaging(t *testing.T) {
	body := strings.Repeat("0123456789", 7000) // 70,000 chars
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	e := &Executor{Root: t.TempDir(), HTTPClient: noProxyClient()}
	strip := func(s string) string {
		if i := strings.Index(s, "\n[showing"); i >= 0 {
			return s[:i]
		}
		return s
	}
	var got strings.Builder
	for _, off := range []int{0, 30000, 60000} {
		out, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL, Offset: off})
		if err != nil {
			t.Fatal(err)
		}
		got.WriteString(strip(out))
	}
	if got.String() != body {
		t.Fatalf("paged fetch reassembled %d chars, want %d", got.Len(), len(body))
	}
	if _, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL, Offset: 70000}); err == nil || !strings.Contains(err.Error(), "past the end") {
		t.Fatalf("offset past end err = %v", err)
	}
	if _, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL, Offset: -1}); err == nil {
		t.Fatal("negative offset should error")
	}
}

func TestWebfetchFetchCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("a", MaxWebfetchBytes+1024))
	}))
	defer srv.Close()
	e := &Executor{Root: t.TempDir(), HTTPClient: noProxyClient()}
	out, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[truncated: fetched page exceeded 5 MiB") {
		t.Fatalf("fetch-cap fetch = %q..., want the fetch truncation marker", out[len(out)-200:])
	}
}

func TestWebfetchCache(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "cached body")
	}))
	defer srv.Close()
	e := &Executor{Root: t.TempDir(), HTTPClient: noProxyClient()}
	for i := 0; i < 2; i++ {
		out, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL})
		if err != nil || out != "cached body" {
			t.Fatalf("fetch %d = %q, err %v", i, out, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1 (second fetch is cached)", hits.Load())
	}
	// A different format is a different cache entry.
	if _, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL, Format: "text"}); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("server hits = %d, want 2 after a format=text fetch", hits.Load())
	}
	// The cache is per-executor: a new executor re-downloads.
	e2 := &Executor{Root: t.TempDir(), HTTPClient: noProxyClient()}
	if _, err := e2.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 3 {
		t.Fatalf("server hits = %d, want 3 after a new executor fetches", hits.Load())
	}
}

func TestWebfetchCrossHostRedirect(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		fmt.Fprint(w, "secret target")
	}))
	defer target.Close()
	// Redirect to the same port on a different hostname: localhost
	// vs the 127.0.0.1 the test server listens on.
	u, _ := url.Parse(target.URL)
	redirectTo := "http://localhost:" + u.Port() + "/target"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTo, http.StatusFound)
	}))
	defer srv.Close()
	e := &Executor{Root: t.TempDir(), HTTPClient: noProxyClient()}
	out, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Redirect to "+redirectTo+" not followed") {
		t.Fatalf("cross-host redirect = %q, want the not-followed report", out)
	}
	if targetHits.Load() != 0 {
		t.Fatalf("redirect target was hit %d times, want 0", targetHits.Load())
	}
}

func TestWebfetchRefusesMetadataIP(t *testing.T) {
	e := &Executor{Root: t.TempDir(), HTTPClient: noProxyClient()}
	// The metadata endpoint must be refused before any network use:
	// no server is listening there, so any other error would mean
	// the check did not run.
	_, err := e.Webfetch(context.Background(), WebfetchArgs{URL: "http://169.254.169.254/latest/meta-data/"})
	if err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("metadata fetch err = %v, want a non-public-address refusal", err)
	}
}

func TestWebfetchPinnedDialUsesValidatedIP(t *testing.T) {
	// Whole-chain test: a fake hostname whose only address (via the
	// injected resolver) is a public IP. The transport's own dialer
	// records the address it is asked to dial and redirects the
	// connection to the local test server, so the test proves the
	// dial used the validated IP, not the hostname — without any
	// real DNS or any real connection to that IP.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "pinned ok")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	_, port, _ := net.SplitHostPort(u.Host)
	var mu sync.Mutex
	var dialed []string
	baseDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		d := &net.Dialer{}
		return d.DialContext(ctx, network, "127.0.0.1:"+port)
	}
	tr := &http.Transport{Proxy: nil, DialContext: baseDial}
	e := &Executor{Root: t.TempDir(), HTTPClient: &http.Client{Transport: tr}}
	e.LookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		if host == "pinned.example" {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}
		return nil, &net.DNSError{IsNotFound: true, Name: host}
	}
	out, err := e.Webfetch(context.Background(), WebfetchArgs{URL: "http://pinned.example/"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "pinned ok" {
		t.Fatalf("pinned fetch = %q, want %q", out, "pinned ok")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 1 || dialed[0] != "93.184.216.34:80" {
		t.Fatalf("dialed = %v, want [93.184.216.34:80]", dialed)
	}
}

func TestWebfetchPinnedTLSKeepsHostname(t *testing.T) {
	// The load-bearing property of pinning: TLS must still verify
	// against the hostname, not the IP. The httptest TLS server's
	// certificate is valid for "example.com" and nothing else, so
	// if the handshake used the pinned IP as the ServerName it
	// would fail with a certificate error. A successful fetch
	// proves the ServerName stayed "example.com" while the socket
	// went to the validated IP.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "tls pinned ok")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	_, port, _ := net.SplitHostPort(u.Host)
	var mu sync.Mutex
	var dialed []string
	baseDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		d := &net.Dialer{}
		return d.DialContext(ctx, network, "127.0.0.1:"+port)
	}
	client := srv.Client()
	tr := client.Transport.(*http.Transport)
	tr.Proxy = nil // no proxy: the pinned path must engage
	tr.DialContext = baseDial
	e := &Executor{Root: t.TempDir(), HTTPClient: client}
	e.LookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		if host == "example.com" {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}
		return nil, &net.DNSError{IsNotFound: true, Name: host}
	}
	out, err := e.Webfetch(context.Background(), WebfetchArgs{URL: "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "tls pinned ok" {
		t.Fatalf("pinned TLS fetch = %q, want %q", out, "tls pinned ok")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 1 || dialed[0] != "93.184.216.34:443" {
		t.Fatalf("dialed = %v, want [93.184.216.34:443]", dialed)
	}
}

func TestWebfetchTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		fmt.Fprint(w, "too slow")
	}))
	defer srv.Close()
	e := &Executor{Root: t.TempDir(), HTTPClient: noProxyClient()}
	_, err := e.Webfetch(context.Background(), WebfetchArgs{URL: srv.URL, Timeout: 1})
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("slow fetch err = %v, want a deadline error", err)
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
