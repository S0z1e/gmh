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
// service on the local network. The guard has three parts:
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
//
// What this does NOT do: pin the validated IP to the connection
// (Gemini CLI's transport-layer pinning). A DNS record could in
// principle change between the check and the dial — a TOCTOU window
// that only pinning closes. That is a known residual, not an
// accident: pinning needs a custom DialContext and is a follow-up
// if the user wants it.

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

// validateFetchHost resolves host (unless it is a localhost name or
// an IP literal) and refuses it when any resolved address is not
// public. It runs before webfetch makes any network use of the URL.
func (e *Executor) validateFetchHost(ctx context.Context, host string) error {
	if isLocalhostName(host) {
		return nil // deliberate: the user's own dev servers
	}
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return fmt.Errorf("webfetch refuses non-public address %s", host)
		}
		return nil
	}
	lookup := e.LookupIP
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return fmt.Errorf("webfetch cannot resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("webfetch cannot resolve %s: no addresses", host)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("webfetch refuses %s: resolves to non-public address %s", host, ip)
		}
	}
	return nil
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
