package tools

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// MaxWebfetchBytes caps one fetched page: a model does not need
	// more than this of one page in a turn (the loop truncates tool
	// results for the LLM view on top of this).
	MaxWebfetchBytes = 64 << 10
	// DefaultWebfetchTimeout bounds one fetch, redirects included.
	DefaultWebfetchTimeout = 30 * time.Second
)

// WebfetchArgs names the URL to fetch.
type WebfetchArgs struct {
	URL string `json:"url"`
}

// Webfetch fetches a URL and returns its content as plain text. HTML
// is converted to text (scripts, styles and tags dropped); other
// text bodies come back as-is. Only http(s) is allowed, and non-text
// content (images, PDFs, binaries) is refused — the model can still
// fetch those with curl in bash when it really must. The result is
// capped at MaxWebfetchBytes with a marker. Read-only, like read and
// grep: it needs no approval in suggest mode.
func (e *Executor) Webfetch(ctx context.Context, a WebfetchArgs) (string, error) {
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("webfetch needs an http(s) URL, got %q", a.URL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("webfetch refuses non-http(s) URL: %s", u.Scheme)
	}
	timeout := DefaultWebfetchTimeout
	if e.WebfetchTimeout > 0 {
		timeout = e.WebfetchTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := e.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "minimalharness/1.0 (+https://github.com/S0z1e/gmh)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("webfetch %s: %w", u.String(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("webfetch %s: HTTP %s", u.String(), resp.Status)
	}
	ct := resp.Header.Get("Content-Type")
	if isBinaryContentType(ct) {
		return "", fmt.Errorf("webfetch %s: unsupported content type %q (text and HTML only)", u.String(), ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxWebfetchBytes+1))
	if err != nil {
		return "", fmt.Errorf("webfetch %s: %w", u.String(), err)
	}
	truncated := len(body) > MaxWebfetchBytes
	if truncated {
		body = body[:MaxWebfetchBytes]
	}
	text := ""
	if isHTML(ct, body) {
		text = htmlToText(string(body))
	} else {
		text = string(body)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("webfetch %s: no readable text in the page", u.String())
	}
	if truncated {
		text += "\n[truncated: page continues, first 64 KiB shown]"
	}
	if resp.Request.URL.String() != u.String() {
		text = "Redirected to " + resp.Request.URL.String() + "\n\n" + text
	}
	return text, nil
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
// and blank lines collapse to one.
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
