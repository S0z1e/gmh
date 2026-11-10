package tools

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// SSRF guard for webfetch (P0 from the webfetch deep-dive).
//
// webfetch is approval-free, so the model can ask it to fetch any
// URL with zero user interaction. Without a guard, a fetched URL —
// or one it redirects to — can reach the cloud metadata endpoint
// (169.254.169.254, which hands out temporary credentials) or a
// service on the local network. The guard has four parts:
//
//  1. the host is validated before any network use: a literal
//     non-public IP is refused, and a hostname is resolved and
//     every returned address must be public;
//  2. a redirect to another host is not followed at all (Claude
//     Code's rule): the target is reported so the model can decide
//     whether the new host is trustworthy;
//  3. localhost is deliberately kept fetchable: the user's own dev
//     servers live there. That is a documented choice, not an
//     oversight — and it does not extend to the metadata address,
//     which has no legitimate use from a dev server.
//  4. transport-layer IP pinning (Gemini CLI's rule): the
//     connection is dialled to one of the addresses the check
//     already validated — a custom DialContext on a cloned
//     transport — so a DNS record cannot change between the check
//     and the dial (DNS rebinding). TLS still verifies against the
//     hostname, not the IP.
//
// Residual: pinning only applies to a direct connection. When a
// proxy is in use the proxy dials the target itself, so there is
// nothing to pin — the preflight check still runs, but the TOCTOU
// window is the proxy's, not the harness's.

// isLocalhostName reports whether host is a loopback name the user
// deliberately kept fetchable: their own dev servers live here.
// "localhost", any "*.localhost" name, and the loopback literals.
func isLocalhostName(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	switch h {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return strings.HasSuffix(h, ".localhost")
}

// isPublicIP reports whether ip may be fetched: loopback, private,
// link-local, unspecified and multicast addresses are not public.
// The cloud metadata address 169.254.169.254 is link-local, so it
// is refused here too.
func isPublicIP(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() && !ip.IsUnspecified() && !ip.IsMulticast()
}

// resolveFetchHost resolves host and returns the validated public
// addresses the connection may be pinned to. A localhost name
// returns nil addresses (no pinning: the loopback address is not
// an attacker-controlled DNS answer, and the user's dev servers
// live there). It runs before webfetch makes any network use of
// the URL, and the returned addresses are the ones the transport
// dials — the hostname is never resolved a second time.
func (e *Executor) resolveFetchHost(ctx context.Context, host string) ([]net.IP, error) {
	if isLocalhostName(host) {
		return nil, nil // deliberate: the user's own dev servers
	}
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return nil, fmt.Errorf("webfetch refuses non-public address %s", host)
		}
		return []net.IP{ip}, nil
	}
	lookup := e.LookupIP
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("webfetch cannot resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("webfetch cannot resolve %s: no addresses", host)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return nil, fmt.Errorf("webfetch refuses %s: resolves to non-public address %s", host, ip)
		}
	}
	return ips, nil
}

// dialPinned dials addr's port at one of ips, in order, instead of
// resolving addr's hostname. The transport calls this with the
// target's host:port; the host part is ignored on purpose — this
// dialer is installed on a transport built for one fetch of one
// host, so every dial it sees is for that host, and resolving the
// hostname again would reopen the TOCTOU window the pinning
// closes. base is the transport's own dialer, so timeouts and
// keep-alives behave as before; only the address changes.
func dialPinned(ctx context.Context, network, addr string, ips []net.IP, base func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		conn, err := base(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if lastErr == nil {
		return nil, fmt.Errorf("no addresses to dial")
	}
	return nil, lastErr
}

// pinFetchTransport returns a RoundTripper for one fetch that dials
// only ips, or nil when pinning does not apply. The transport is a
// clone of base, so the caller's client is never mutated. Pinning
// is skipped when there are no validated addresses (a localhost
// name), when base is a custom RoundTripper rather than an
// *http.Transport, and when the request will go through a proxy —
// the proxy dials the target itself, so the transport never dials
// the pinned host, and pinning the proxy's address to the target's
// IP would break the fetch outright.
func pinFetchTransport(base http.RoundTripper, ips []net.IP, req *http.Request) http.RoundTripper {
	if len(ips) == 0 {
		return nil
	}
	var tr *http.Transport
	switch t := base.(type) {
	case nil:
		if dt, ok := http.DefaultTransport.(*http.Transport); ok {
			tr = dt
		} else {
			tr = &http.Transport{}
		}
	case *http.Transport:
		tr = t
	default:
		return nil // a custom RoundTripper cannot be pinned
	}
	clone := tr.Clone()
	if clone.Proxy != nil {
		if proxyURL, _ := clone.Proxy(req); proxyURL != nil {
			return nil // proxied: the proxy dials the target
		}
	}
	baseDial := clone.DialContext
	if baseDial == nil {
		baseDial = (&net.Dialer{}).DialContext
	}
	clone.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialPinned(ctx, network, addr, ips, baseDial)
	}
	return clone
}

// checkFetchRedirect is the http.Client policy for webfetch: a
// redirect to another host is NOT followed — following it silently
// would let a fetched URL reach a host the model never asked for.
// Returning ErrUseLastResponse makes the client hand back the 3xx
// response so Webfetch can report the target instead of fetching
// it. Same-host redirects (including an http→https upgrade) are
// followed, up to 10.
func checkFetchRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if !strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname()) {
		return http.ErrUseLastResponse
	}
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	return nil
}
