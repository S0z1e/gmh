// Package endpoint keeps provider credential bindings and HTTP destinations on
// the same base-URL normalization contract.
package endpoint

import (
	"net/url"
	"strings"
)

func Base(raw string) string {
	base := strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	for _, suffix := range []string{"/chat/completions", "/responses", "/messages"} {
		base = strings.TrimSuffix(base, suffix)
	}
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return base
}

func Key(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	base := Base(raw)
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	u.Scheme = strings.ToLower(u.Scheme)
	// Unicode lowercasing can collapse distinct IDNA destinations (e.g. İ
	// versus i). Fold only ASCII DNS casing and preserve IPv6 zone spelling.
	host := []byte(u.Host)
	for i, b := range host {
		if b == '%' {
			break
		}
		if b >= 'A' && b <= 'Z' {
			host[i] = b + ('a' - 'A')
		}
	}
	u.Host = string(host)
	return u.String()
}
