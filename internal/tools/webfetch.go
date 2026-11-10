package tools

import (
	"context"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// MaxWebfetchBytes caps one fetched page at the transport layer:
	// nobody should download the whole internet into one tool call
	// (opencode and Pi both use 5 MB here). The old 64 KiB cap cut
	// the raw HTML before conversion, so on real doc pages the
	// model got 64 KiB of navigation junk and never saw the article.
	MaxWebfetchBytes = 5 << 20
	// MaxWebfetchPageChars is one page of converted text handed to
	// the model per call (Pi's default is 30k). The cap applies
	// AFTER conversion, and offset pages through the rest — so a
	// long article is readable in slices instead of losing its tail
	// to the navigation junk up front.
	MaxWebfetchPageChars = 30000
	// WebfetchCacheTTL is how long a fetched page is reused (Claude
	// Code and Pi-web both cache 15 minutes): repeated fetches and
	// offset paging do not re-download the page.
	WebfetchCacheTTL = 15 * time.Minute
	// MaxWebfetchCacheEntries bounds the in-memory cache; the oldest
	// entry is evicted first.
	MaxWebfetchCacheEntries = 16
	// MaxWebfetchTimeoutSeconds caps a per-call timeout (opencode:
	// 30s default, 120s max).
	MaxWebfetchTimeoutSeconds = 120
	// DefaultWebfetchTimeout bounds one fetch, redirects included.
	DefaultWebfetchTimeout = 30 * time.Second
)

// WebfetchArgs names the URL to fetch, plus the P1 knobs: format
// (markdown by default, or plain text), offset (characters into the
// converted text, for paging through a long page) and timeout
// (seconds, 1..120; 0 = the executor default, 30s).
type WebfetchArgs struct {
	URL     string `json:"url"`
	Format  string `json:"format,omitempty"`
	Offset  int    `json:"offset,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
}

type webfetchCacheEntry struct {
	text      string // full converted text, before paging
	truncated bool   // the raw fetch hit MaxWebfetchBytes
	finalURL  string // URL after same-host redirects
	fetchedAt time.Time
}

type webfetchCache struct {
	mu      sync.Mutex
	entries map[string]*webfetchCacheEntry
	order   []string // insertion order, oldest first
}

func (c *webfetchCache) get(key string) (*webfetchCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Since(e.fetchedAt) > WebfetchCacheTTL {
		delete(c.entries, key)
		for i, k := range c.order {
			if k == key {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
		return nil, false
	}
	return e, true
}

func (c *webfetchCache) put(key string, e *webfetchCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; !ok {
		c.order = append(c.order, key)
	}
	c.entries[key] = e
	for len(c.order) > MaxWebfetchCacheEntries {
		old := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, old)
	}
}

// Webfetch fetches a URL and returns its content as Markdown (HTML
// is converted, keeping headings, lists, links, code and tables;
// scripts, styles and tags are dropped). format=text returns plain
// text instead. Other text bodies come back as-is. Only http(s) is
// allowed, and non-text content (images, PDFs, binaries) is refused
// — the model can still fetch those with curl in bash when it
// really must.
//
// Safety (P0): the host is validated before any network use —
// private, loopback, link-local and cloud-metadata addresses are
// refused (localhost is deliberately kept, for the user's own dev
// servers), and a redirect to another host is not followed: the
// target is reported instead. The connection is then pinned to one
// of the validated addresses (transport-layer IP pinning), so a DNS
// record cannot change between the check and the dial; TLS still
// verifies against the hostname. Pinning is skipped when a proxy
// is in use — the proxy dials the target itself. Fetched pages are
// cached for 15 minutes, so repeated fetches and offset paging do
// not re-download. Read-only, like read and grep: it needs no
// approval in suggest mode.
func (e *Executor) Webfetch(ctx context.Context, a WebfetchArgs) (string, error) {
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("webfetch needs an http(s) URL, got %q", a.URL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("webfetch refuses non-http(s) URL: %s", u.Scheme)
	}
	format := strings.ToLower(strings.TrimSpace(a.Format))
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" && format != "text" {
		return "", fmt.Errorf("webfetch format must be markdown or text, got %q", a.Format)
	}
	if a.Offset < 0 {
		return "", fmt.Errorf("webfetch offset must be >= 0, got %d", a.Offset)
	}
	timeout := DefaultWebfetchTimeout
	if e.WebfetchTimeout > 0 {
		timeout = e.WebfetchTimeout
	}
	if a.Timeout > 0 {
		timeout = time.Duration(a.Timeout) * time.Second
	}
	if timeout > MaxWebfetchTimeoutSeconds*time.Second {
		timeout = MaxWebfetchTimeoutSeconds * time.Second
	}
	if timeout <= 0 {
		timeout = DefaultWebfetchTimeout
	}
	// SSRF preflight, before any network use of the URL. It returns
	// the validated addresses the connection is pinned to, so the
	// hostname is resolved exactly once.
	ips, err := e.resolveFetchHost(ctx, u.Hostname())
	if err != nil {
		return "", err
	}
	cacheKey := u.String() + "\n" + format
	cache := e.webfetchCache()
	entry, hit := cache.get(cacheKey)
	if !hit {
		entry, err = e.fetchPage(ctx, u, format, timeout, ips)
		if err != nil {
			return "", err
		}
		cache.put(cacheKey, entry)
	}
	// Page the converted text: the cap applies after conversion, so
	// a long article is readable in slices.
	runes := []rune(entry.text)
	total := len(runes)
	if a.Offset >= total {
		return "", fmt.Errorf("webfetch offset %d is past the end of the page (%d characters)", a.Offset, total)
	}
	end := a.Offset + MaxWebfetchPageChars
	if end > total {
		end = total
	}
	out := string(runes[a.Offset:end])
	if entry.finalURL != "" && entry.finalURL != u.String() {
		out = "Redirected to " + entry.finalURL + "\n\n" + out
	}
	if entry.truncated {
		out += "\n[truncated: fetched page exceeded 5 MiB, content after that point was not downloaded]"
	}
	if end < total {
		out += fmt.Sprintf("\n[showing characters %d-%d of %d; use offset=%d to read on]", a.Offset, end, total, end)
	}
	return out, nil
}

// fetchPage does one HTTP fetch and converts the body. It is the
// only place that touches the network; Webfetch wraps it with the
// cache and the paging. ips are the addresses resolveFetchHost
// already validated: the transport dials one of them, never the
// hostname again.
func (e *Executor) fetchPage(ctx context.Context, u *url.URL, format string, timeout time.Duration, ips []net.IP) (*webfetchCacheEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := e.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	// Copy so the cross-host redirect policy always applies, even
	// when the caller injected a client of its own.
	c := *client
	c.CheckRedirect = checkFetchRedirect
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "minimalharness/1.0 (+https://github.com/S0z1e/gmh)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	// Pin the connection to the validated addresses (no-op when the
	// fetch is proxied or the host is a localhost name: then there
	// is nothing to pin).
	if rt := pinFetchTransport(c.Transport, ips, req); rt != nil {
		c.Transport = rt
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webfetch %s: %w", u.String(), err)
	}
	defer resp.Body.Close()
	finalURL := resp.Request.URL.String()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// A cross-host redirect the policy refused: report the
		// target instead of fetching it, so the model can decide
		// whether the new host is trustworthy.
		loc := resp.Header.Get("Location")
		target := loc
		if t, err := resp.Request.URL.Parse(loc); err == nil {
			target = t.String()
		}
		return &webfetchCacheEntry{
			text:      fmt.Sprintf("Redirect to %s not followed (cross-host redirect; fetch that URL directly if you trust the host).", target),
			finalURL:  finalURL,
			fetchedAt: time.Now(),
		}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("webfetch %s: HTTP %s", u.String(), resp.Status)
	}
	ct := resp.Header.Get("Content-Type")
	if isBinaryContentType(ct) {
		return nil, fmt.Errorf("webfetch %s: unsupported content type %q (text and HTML only)", u.String(), ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxWebfetchBytes+1))
	if err != nil {
		return nil, fmt.Errorf("webfetch %s: %w", u.String(), err)
	}
	truncated := len(body) > MaxWebfetchBytes
	if truncated {
		body = body[:MaxWebfetchBytes]
	}
	text := ""
	if isHTML(ct, body) {
		if format == "text" {
			text = htmlToText(string(body))
		} else {
			text = htmlToMarkdown(string(body))
		}
	} else {
		text = string(body)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("webfetch %s: no readable text in the page", u.String())
	}
	return &webfetchCacheEntry{text: text, truncated: truncated, finalURL: finalURL, fetchedAt: time.Now()}, nil
}

// isBinaryContentType refuses types that are never readable text:
// images, media, fonts, archives, and application/* except the text
// ones (json, xml, javascript, ecmascript). An empty or unknown type
// is not binary: the body is sniffed instead.
func isBinaryContentType(ct string) bool {
	ct = strings.ToLower(strings.Split(ct, ";")[0])
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return false
	}
	for _, p := range []string{"image/", "video/", "audio/", "font/"} {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	if strings.HasPrefix(ct, "application/") {
		switch ct {
		case "application/json", "application/xml", "application/xhtml+xml",
			"application/javascript", "application/ecmascript":
			return false
		}
		return true
	}
	return false
}

// isHTML decides whether to strip tags: the type says so, or the body
// looks like a document when the server sent no usable type.
func isHTML(ct string, body []byte) bool {
	lower := strings.ToLower(strings.Split(ct, ";")[0])
	if strings.Contains(lower, "html") || strings.Contains(lower, "xml") {
		return true
	}
	if strings.TrimSpace(lower) == "" {
		trimmed := strings.TrimSpace(string(body))
		return strings.HasPrefix(strings.ToLower(trimmed), "<!doctype html") ||
			strings.HasPrefix(strings.ToLower(trimmed), "<html")
	}
	return false
}

var (
	commentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	scriptRe  = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	styleRe   = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	blockRe   = regexp.MustCompile(`(?i)</?(p|div|h[1-6]|li|tr|br|hr|section|article|header|footer|nav|ul|ol|table|blockquote|pre|title)(\s[^>]*)?>`)
	tagRe     = regexp.MustCompile(`<[^>]+>`)
	spaceRe   = regexp.MustCompile(`[ \t]+`)
)

// htmlToText converts an HTML document to readable text: comments,
// scripts and styles are dropped wholesale, block elements become
// line breaks, remaining tags are stripped, entities are decoded,
// and blank lines collapse to one. Kept for format=text; the
// Markdown path uses htmlToMarkdown instead.
func htmlToText(s string) string {
	s = commentRe.ReplaceAllString(s, " ")
	s = scriptRe.ReplaceAllString(s, "")
	s = styleRe.ReplaceAllString(s, "")
	s = blockRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = spaceRe.ReplaceAllString(s, " ")
	var lines []string
	blank := false
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			if !blank && len(lines) > 0 {
				lines = append(lines, "")
			}
			blank = true
			continue
		}
		blank = false
		lines = append(lines, l)
	}
	// Drop a trailing blank line from the collapse above.
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}
