package tools

import (
	"context"
	"net"
	"net/http"
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

func TestValidateFetchHost(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	// Localhost names are the deliberate dev-server exception.
	for _, h := range []string{"localhost", "LOCALHOST", "127.0.0.1", "::1", "api.localhost"} {
		if err := e.validateFetchHost(context.Background(), h); err != nil {
			t.Fatalf("validateFetchHost(%q) = %v, want nil", h, err)
		}
	}
	// Literal non-public IPs are refused without a lookup.
	for _, h := range []string{"169.254.169.254", "10.0.0.5", "192.168.1.1", "0.0.0.0"} {
		if err := e.validateFetchHost(context.Background(), h); err == nil {
			t.Fatalf("validateFetchHost(%q) = nil, want an error", h)
		}
	}
	// Hostnames are resolved and every address must be public.
	e.LookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		switch host {
		case "good.example":
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		case "evil.example":
			return []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("10.0.0.5")}, nil
		case "meta.example":
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		}
		return nil, &net.DNSError{IsNotFound: true, Name: host}
	}
	if err := e.validateFetchHost(context.Background(), "good.example"); err != nil {
		t.Fatalf("validateFetchHost(good.example) = %v", err)
	}
	for _, h := range []string{"evil.example", "meta.example", "missing.example"} {
		if err := e.validateFetchHost(context.Background(), h); err == nil {
			t.Fatalf("validateFetchHost(%q) = nil, want an error", h)
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
