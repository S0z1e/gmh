package tools

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip     string
		public bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"93.184.216.34", true},
		{"2001:4860:4860::8888", true},
		// The cloud metadata endpoint: link-local, never fetchable.
		{"169.254.169.254", false},
		{"169.254.1.2", false},
		{"fe80::1", false},
		// Private ranges.
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"172.31.255.255", false},
		{"192.168.1.1", false},
		{"fd00::1", false},
		// Loopback and unspecified.
		{"127.0.0.1", false},
		{"::1", false},
		{"0.0.0.0", false},
		{"::", false},
		// Multicast.
		{"224.0.0.1", false},
		{"ff02::1", false},
	}
	for _, c := range cases {
		if got := isPublicIP(net.ParseIP(c.ip)); got != c.public {
			t.Fatalf("isPublicIP(%s) = %v, want %v", c.ip, got, c.public)
		}
	}
}

func TestResolveFetchHost(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	// Localhost names are the deliberate dev-server exception: no
	// error, and no addresses to pin (nothing to pin a loopback
	// dial to).
	for _, h := range []string{"localhost", "LOCALHOST", "127.0.0.1", "::1", "api.localhost"} {
		ips, err := e.resolveFetchHost(context.Background(), h)
		if err != nil {
			t.Fatalf("resolveFetchHost(%q) = %v, want nil", h, err)
		}
		if len(ips) != 0 {
			t.Fatalf("resolveFetchHost(%q) = %v, want no addresses", h, ips)
		}
	}
	// Literal non-public IPs are refused without a lookup.
	for _, h := range []string{"169.254.169.254", "10.0.0.5", "192.168.1.1", "0.0.0.0"} {
		if _, err := e.resolveFetchHost(context.Background(), h); err == nil {
			t.Fatalf("resolveFetchHost(%q) = nil, want an error", h)
		}
	}
	// A literal public IP is returned as the one address to pin.
	ips, err := e.resolveFetchHost(context.Background(), "93.184.216.34")
	if err != nil || len(ips) != 1 || ips[0].String() != "93.184.216.34" {
		t.Fatalf("resolveFetchHost(93.184.216.34) = %v, %v", ips, err)
	}
	// Hostnames are resolved and every address must be public; the
	// resolved addresses are the ones the transport pins to.
	e.LookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		switch host {
		case "good.example":
			return []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("2606:2800:220:1:248:1893:25c8:1946")}, nil
		case "evil.example":
			return []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("10.0.0.5")}, nil
		case "meta.example":
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		}
		return nil, &net.DNSError{IsNotFound: true, Name: host}
	}
	ips, err = e.resolveFetchHost(context.Background(), "good.example")
	if err != nil || len(ips) != 2 {
		t.Fatalf("resolveFetchHost(good.example) = %v, %v", ips, err)
	}
	for _, h := range []string{"evil.example", "meta.example", "missing.example"} {
		if _, err := e.resolveFetchHost(context.Background(), h); err == nil {
			t.Fatalf("resolveFetchHost(%q) = nil, want an error", h)
		}
	}
}

func TestCheckFetchRedirect(t *testing.T) {
	// Same-host redirect (including a scheme upgrade) is followed.
	orig, _ := url.Parse("http://example.com/old")
	same, _ := url.Parse("https://example.com/new")
	if err := checkFetchRedirect(&http.Request{URL: same}, []*http.Request{{URL: orig}}); err != nil {
		t.Fatalf("same-host redirect = %v, want nil", err)
	}
	// Cross-host redirect is refused.
	other, _ := url.Parse("http://other.com/new")
	if err := checkFetchRedirect(&http.Request{URL: other}, []*http.Request{{URL: orig}}); err != http.ErrUseLastResponse {
		t.Fatalf("cross-host redirect = %v, want ErrUseLastResponse", err)
	}
	// Too many same-host redirects still stop.
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = &http.Request{URL: orig}
	}
	if err := checkFetchRedirect(&http.Request{URL: same}, via); err == nil {
		t.Fatal("11th redirect = nil, want an error")
	}
}

// TestDialPinnedDialsTheIPNotTheHostname is the honest core of the
// pinning tests: a real DNS-rebinding race cannot be reproduced
// deterministically, so instead this proves the property pinning
// needs — the dialer never resolves the hostname. The address it
// is asked to dial names a host that does not exist
// (nonexistent.invalid), yet the connection still lands on the
// local test server, because the dial goes to the pinned IP.
func TestDialPinnedDialsTheIPNotTheHostname(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "pinned ok")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	_, port, _ := net.SplitHostPort(u.Host)
	base := (&net.Dialer{}).DialContext
	conn, err := dialPinned(context.Background(), "tcp", "nonexistent.invalid:"+port,
		[]net.IP{net.ParseIP("127.0.0.1")}, base)
	if err != nil {
		t.Fatalf("dialPinned = %v", err)
	}
	defer conn.Close()
	if got := conn.RemoteAddr().String(); got != "127.0.0.1:"+port {
		t.Fatalf("dialPinned landed on %s, want 127.0.0.1:%s", got, port)
	}
	// A full HTTP round trip over the pinned connection, to prove
	// the transport can actually use it.
	req, _ := http.NewRequest("GET", "http://nonexistent.invalid:"+port+"/", nil)
	tr := &http.Transport{Proxy: nil}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialPinned(ctx, network, addr, []net.IP{net.ParseIP("127.0.0.1")}, base)
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("pinned round trip = %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "pinned ok" {
		t.Fatalf("pinned round trip body = %q", body)
	}
}

func TestDialPinnedTriesAddressesInOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	_, port, _ := net.SplitHostPort(u.Host)
	base := (&net.Dialer{}).DialContext
	// 127.0.0.2 refuses fast (the whole 127/8 is loopback); the
	// dial must fall through to 127.0.0.1.
	conn, err := dialPinned(context.Background(), "tcp", "whatever.invalid:"+port,
		[]net.IP{net.ParseIP("127.0.0.2"), net.ParseIP("127.0.0.1")}, base)
	if err != nil {
		t.Fatalf("dialPinned = %v", err)
	}
	defer conn.Close()
	if got := conn.RemoteAddr().String(); got != "127.0.0.1:"+port {
		t.Fatalf("dialPinned landed on %s, want 127.0.0.1:%s", got, port)
	}
	// Every address refused: an error, not a hang.
	if _, err := dialPinned(context.Background(), "tcp", "whatever.invalid:"+port,
		[]net.IP{net.ParseIP("127.0.0.2")}, base); err == nil {
		t.Fatal("dialPinned with only a refused address = nil, want an error")
	}
	if _, err := dialPinned(context.Background(), "tcp", "bad-addr",
		[]net.IP{net.ParseIP("127.0.0.1")}, base); err == nil {
		t.Fatal("dialPinned with no port = nil, want an error")
	}
}
