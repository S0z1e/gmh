package tools

import (
	"html"
	"strconv"
	"strings"
	"unicode/utf8"
)

// htmlToMarkdown converts an HTML document to Markdown with a small
// hand-written tokenizer and a Turndown-style rule table. Pure
// stdlib, no new dependency — the light P1 option from the webfetch
// deep-dive (the real HTML5 parser costs +0.4 MB, a full Readability
// port +6.7 MB and would kill the 10 MB binary goal).
//
// What it keeps: headings, paragraphs, lists (nested, ordered and
// unordered), links, images, inline code, fenced code blocks (with
// a language- class when present), blockquotes, tables (GFM),
// emphasis and strikethrough. What it drops: scripts, styles and
// the whole <head> (the page title included — Turndown does the
// same), plus any tag it does not know. Anything it cannot
// represent degrades to plain text, never to an error.
//
// Limits, by design: no DOM tree and no Readability scoring, so it
// cannot tell an article from navigation the way Readability can —
// it converts the whole document. The <pre> content is kept
// verbatim; a backtick inside inline code is not escaped.

func htmlToMarkdown(s string) string {
	c := &mdConv{out: &strings.Builder{}}
	c.run(mdTokenize(s))
	return c.finish()
}

// ---------------------------------------------------------------------
// Tokenizer

type mdTokKind int

const (
	mdText mdTokKind = iota
	mdStartTag
	mdEndTag
)

type mdToken struct {
	kind      mdTokKind
	name      string // lowercased tag name
	attrs     map[string]string
	selfClose bool
	text      string // mdText only
}

var mdVoid = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

// mdTokenize splits s into tags and text. It is a tolerant scanner,
// not a parser: it understands quoted attribute values (so a `>`
// inside quotes does not end the tag), skips comments, and drops
// <script> and <style> wholesale — those must be skipped here, not
// in the converter, because their raw content can contain `<` that
// is not a tag. <head> is dropped the same way, but only when its
// close tag exists, so a malformed page degrades instead of
// vanishing. An unclosed <script> or <style> swallows the rest of
// the document, which is what the HTML spec does too.
func mdTokenize(s string) []mdToken {
	var toks []mdToken
	i := 0
	for i < len(s) {
		lt := strings.IndexByte(s[i:], '<')
		if lt < 0 {
			toks = append(toks, mdToken{kind: mdText, text: s[i:]})
			break
		}
		if lt > 0 {
			toks = append(toks, mdToken{kind: mdText, text: s[i : i+lt]})
		}
		i += lt // s[i] == '<'
		if i+1 >= len(s) {
			toks = append(toks, mdToken{kind: mdText, text: "<"})
			break
		}
		c := s[i+1]
		switch {
		case c == '!':
			if strings.HasPrefix(s[i:], "<!--") {
				if end := strings.Index(s[i+4:], "-->"); end >= 0 {
					i += 4 + end + 3
					continue
				}
				// Unterminated comment: treat '<' as literal text
				// rather than swallowing the rest of the page.
				toks = append(toks, mdToken{kind: mdText, text: "<"})
				i++
				continue
			}
			if end := strings.IndexByte(s[i:], '>'); end >= 0 {
				i += end + 1
				continue
			}
			toks = append(toks, mdToken{kind: mdText, text: s[i:]})
			i = len(s)
		case c == '/':
			name, j := mdReadName(s, i+2)
			if name == "" {
				toks = append(toks, mdToken{kind: mdText, text: "<"})
				i++
				continue
			}
			if end := strings.IndexByte(s[j:], '>'); end >= 0 {
				toks = append(toks, mdToken{kind: mdEndTag, name: name})
				i = j + end + 1
				continue
			}
			toks = append(toks, mdToken{kind: mdText, text: s[i:]})
			i = len(s)
		case mdIsLetter(c):
			tok, j, ok := mdReadTag(s, i)
			if !ok {
				toks = append(toks, mdToken{kind: mdText, text: s[i:]})
				i = len(s)
				continue
			}
			i = j
			if tok.name == "script" || tok.name == "style" {
				// Raw-text element: skip to the matching close tag.
				if end := mdIndexFold(s[i:], "</"+tok.name); end >= 0 {
					if gt := strings.IndexByte(s[i+end:], '>'); gt >= 0 {
						i += end + gt + 1
						continue
					}
				}
				i = len(s) // unclosed: the rest is script/style
				continue
			}
			if tok.name == "head" {
				if end := mdIndexFold(s[i:], "</head"); end >= 0 {
					if gt := strings.IndexByte(s[i+end:], '>'); gt >= 0 {
						i += end + gt + 1
						continue
					}
				}
				// No close tag: fall through and emit the tag; the
				// converter drops <title>/<meta>/<link> by name.
			}
			toks = append(toks, tok)
		default:
			// '<' followed by something that cannot start a tag.
			toks = append(toks, mdToken{kind: mdText, text: "<"})
			i++
		}
	}
	return toks
}

func mdIsLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// mdReadName reads a tag name starting at i and returns it
// lowercased with the index just past it.
func mdReadName(s string, i int) (string, int) {
	j := i
	for j < len(s) {
		c := s[j]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == ':' {
			j++
			continue
		}
		break
	}
	return strings.ToLower(s[i:j]), j
}

// mdReadTag parses a start tag at s[i] == '<'. It returns the token,
// the index just past the closing '>', and false when the tag is
// unterminated. Attribute values may be double-quoted,
// single-quoted or unquoted; a '>' inside quotes does not end the
// tag (the old regex stripper got this wrong).
func mdReadTag(s string, i int) (mdToken, int, bool) {
	name, j := mdReadName(s, i+1)
	tok := mdToken{kind: mdStartTag, name: name, attrs: map[string]string{}}
	for j < len(s) {
		for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
			j++
		}
		if j >= len(s) {
			return tok, 0, false
		}
		if s[j] == '>' {
			return tok, j + 1, true
		}
		if s[j] == '/' && j+1 < len(s) && s[j+1] == '>' {
			tok.selfClose = true
			return tok, j + 2, true
		}
		// Attribute name.
		k := j
		for k < len(s) && s[k] != '=' && s[k] != ' ' && s[k] != '\t' && s[k] != '\n' && s[k] != '\r' && s[k] != '>' && s[k] != '/' {
			k++
		}
		if k == j { // stray character: skip it
			j++
			continue
		}
		attr := strings.ToLower(s[j:k])
		j = k
		for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
			j++
		}
		if j < len(s) && s[j] == '=' {
			j++
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j++
			}
			if j >= len(s) {
				return tok, 0, false
			}
			var val string
			if s[j] == '"' || s[j] == '\'' {
				q := s[j]
				j++
				start := j
				for j < len(s) && s[j] != q {
					j++
				}
				val = s[start:j]
				if j < len(s) {
					j++ // closing quote
				}
			} else {
				start := j
				for j < len(s) && s[j] != ' ' && s[j] != '\t' && s[j] != '\n' && s[j] != '\r' && s[j] != '>' {
					j++
				}
				val = s[start:j]
			}
			tok.attrs[attr] = val
		} else {
			tok.attrs[attr] = ""
		}
	}
	return tok, 0, false
}

// mdIndexFold finds needle in s case-insensitively.
func mdIndexFold(s, needle string) int {
	if needle == "" {
		return 0
	}
	for i := 0; i+len(needle) <= len(s); i++ {
		if strings.EqualFold(s[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------
// Converter

type mdBufKind int

const (
	mdBufNormal mdBufKind = iota // plain output, or a blockquote buffer
	mdBufLink                    // link text: blocks are ignored inside
	mdBufCell                    // table cell text: blocks are ignored inside
	mdBufPre                     // <pre> content: kept verbatim
)

type mdBuf struct {
	b    *strings.Builder
	kind mdBufKind
	href string // mdBufLink only
}

type mdList struct {
	ordered bool
	num     int
}

type mdTable struct {
	rows [][]string
}

type mdConv struct {
	out     *strings.Builder
	stack   []*mdBuf
	lists   []mdList
	table   *mdTable
	inTitle bool
	preLang string
}

func (c *mdConv) top() *mdBuf {
	if len(c.stack) > 0 {
		return c.stack[len(c.stack)-1]
	}
	return &mdBuf{b: c.out, kind: mdBufNormal}
}

func (c *mdConv) push(kind mdBufKind) *mdBuf {
	b := &mdBuf{b: &strings.Builder{}, kind: kind}
	c.stack = append(c.stack, b)
	return b
}

func (c *mdConv) pop() string {
	if len(c.stack) == 0 {
		return ""
	}
	b := c.stack[len(c.stack)-1]
	c.stack = c.stack[:len(c.stack)-1]
	return b.b.String()
}

// blocksOK reports whether block-level tags may act in the current
// buffer: not inside a link, a table cell or a <pre>.
func (c *mdConv) blocksOK() bool {
	t := c.top()
	return t.kind == mdBufNormal
}

func (c *mdConv) run(toks []mdToken) {
	for _, t := range toks {
		switch t.kind {
		case mdText:
			c.text(t.text)
		case mdStartTag:
			c.start(t)
		case mdEndTag:
			c.end(t)
		}
	}
}

func (c *mdConv) text(s string) {
	if c.inTitle {
		return
	}
	t := c.top()
	if t.kind == mdBufPre {
		t.b.WriteString(html.UnescapeString(s))
		return
	}
	mdWriteCollapsed(t.b, html.UnescapeString(s))
}

func mdIsSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\u00a0'
}

// mdWriteCollapsed writes s into b with every run of whitespace
// (space, tab, newline, CR, non-breaking space) folded to one
// space — except at a line start, where it is dropped, and except
// when the buffer already ends with a space, where it is not
// doubled. A trailing whitespace run leaves one space behind, so
// the next chunk still separates from this one: without that, a
// chunk ending in a space and a chunk starting with a word would
// run together ("hello" + "world" instead of "hello world").
func mdWriteCollapsed(b *strings.Builder, s string) {
	cur := b.String()
	atStart := len(cur) == 0 || strings.HasSuffix(cur, "\n")
	endsSpace := false
	if len(cur) > 0 {
		r, _ := utf8.DecodeLastRuneInString(cur)
		endsSpace = mdIsSpace(r)
	}
	i := 0
	hadSpace := false
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !mdIsSpace(r) {
			break
		}
		hadSpace = true
		i += size
	}
	if hadSpace && !atStart && !endsSpace {
		b.WriteByte(' ')
	}
	space := false
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if mdIsSpace(r) {
			space = true
			continue
		}
		if space {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	if space && b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
		b.WriteByte(' ')
	}
}

// mdEnsureNewline ends the current line in b if it has content.
func mdEnsureNewline(b *strings.Builder) {
	s := b.String()
	if len(s) > 0 && s[len(s)-1] != '\n' {
		b.WriteByte('\n')
	}
}

// mdEnsureBlankLine ends the current paragraph in b: the buffer ends
// with a blank line, i.e. two newlines, unless it is empty.
func mdEnsureBlankLine(b *strings.Builder) {
	s := b.String()
	if len(s) == 0 {
		return
	}
	if strings.HasSuffix(s, "\n\n") {
		return
	}
	if strings.HasSuffix(s, "\n") {
		b.WriteByte('\n')
		return
	}
	b.WriteString("\n\n")
}

func (c *mdConv) start(t mdToken) {
	switch t.name {
	case "title":
		c.inTitle = true
		return
	case "pre":
		if c.top().kind == mdBufPre {
			return
		}
		c.preLang = mdCodeLang(t.attrs["class"])
		c.push(mdBufPre)
		return
	case "code":
		if c.top().kind == mdBufPre {
			if c.preLang == "" {
				c.preLang = mdCodeLang(t.attrs["class"])
			}
			return
		}
		c.top().b.WriteString("`")
		return
	}
	if c.top().kind == mdBufPre {
		return // tags inside <pre> are content, not markup
	}
	switch t.name {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		if !c.blocksOK() {
			return
		}
		b := c.top().b
		mdEnsureBlankLine(b)
		b.WriteString(strings.Repeat("#", int(t.name[1]-'0')) + " ")
	case "p", "div", "section", "article", "header", "footer", "nav", "main", "figure", "figcaption":
		if !c.blocksOK() {
			return
		}
		mdEnsureBlankLine(c.top().b)
	case "br":
		if c.top().kind == mdBufLink || c.top().kind == mdBufCell {
			c.top().b.WriteString(" ")
		} else if c.blocksOK() {
			c.top().b.WriteString("\n")
		}
	case "hr":
		if !c.blocksOK() {
			return
		}
		b := c.top().b
		mdEnsureNewline(b)
		b.WriteString("---\n")
		mdEnsureBlankLine(b)
	case "ul", "ol":
		if !c.blocksOK() {
			return
		}
		mdEnsureNewline(c.top().b)
		c.lists = append(c.lists, mdList{ordered: t.name == "ol"})
	case "li":
		if !c.blocksOK() || len(c.lists) == 0 {
			return
		}
		b := c.top().b
		mdEnsureNewline(b)
		b.WriteString(strings.Repeat("  ", len(c.lists)-1))
		l := &c.lists[len(c.lists)-1]
		if l.ordered {
			l.num++
			b.WriteString(strconv.Itoa(l.num) + ". ")
		} else {
			b.WriteString("- ")
		}
	case "blockquote":
		if !c.blocksOK() {
			return
		}
		mdEnsureBlankLine(c.top().b)
		c.push(mdBufNormal)
	case "a":
		if href := strings.TrimSpace(t.attrs["href"]); href != "" {
			b := c.push(mdBufLink)
			b.href = href
		}
	case "img":
		src := strings.TrimSpace(t.attrs["src"])
		alt := strings.TrimSpace(t.attrs["alt"])
		if src == "" {
			if alt != "" {
				c.top().b.WriteString(alt)
			}
			return
		}
		c.top().b.WriteString("![" + alt + "](" + src + ")")
	case "strong", "b":
		c.top().b.WriteString("**")
	case "em", "i":
		c.top().b.WriteString("*")
	case "del", "s":
		c.top().b.WriteString("~~")
	case "table":
		if !c.blocksOK() {
			return
		}
		mdEnsureBlankLine(c.top().b)
		c.table = &mdTable{}
	case "tr":
		if c.table != nil {
			c.table.rows = append(c.table.rows, nil)
		}
	case "th", "td":
		if c.table != nil && len(c.table.rows) > 0 {
			c.push(mdBufCell)
		}
	}
}

func (c *mdConv) end(t mdToken) {
	switch t.name {
	case "title":
		c.inTitle = false
		return
	case "pre":
		if c.top().kind != mdBufPre {
			return
		}
		content := strings.Trim(c.pop(), "\n")
		b := c.top().b
		mdEnsureBlankLine(b)
		b.WriteString("```" + c.preLang + "\n")
		b.WriteString(content)
		b.WriteString("\n```\n")
		mdEnsureBlankLine(b)
		c.preLang = ""
		return
	case "code":
		if c.top().kind == mdBufPre {
			return
		}
		c.top().b.WriteString("`")
		return
	}
	if c.top().kind == mdBufPre {
		return
	}
	switch t.name {
	case "h1", "h2", "h3", "h4", "h5", "h6", "p", "div", "section", "article", "header", "footer", "nav", "main", "figure", "figcaption":
		if c.blocksOK() {
			mdEnsureBlankLine(c.top().b)
		}
	case "ul", "ol":
		if len(c.lists) > 0 {
			c.lists = c.lists[:len(c.lists)-1]
		}
		// Only end the last line: a blank line here would split a
		// parent list when this was a nested list, and the next
		// block adds its own blank line when it needs one.
		if c.blocksOK() {
			mdEnsureNewline(c.top().b)
		}
	case "blockquote":
		if len(c.stack) == 0 || c.top().kind != mdBufNormal {
			return
		}
		// Pop the quote buffer (it is the top normal buffer) and
		// re-emit its lines with a "> " prefix; nesting composes
		// because an inner quote already prefixed its own lines.
		quoted := c.pop()
		b := c.top().b
		mdEnsureBlankLine(b)
		for _, line := range strings.Split(strings.Trim(quoted, "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				b.WriteString("\n")
				continue
			}
			b.WriteString("> " + line + "\n")
		}
		mdEnsureBlankLine(b)
	case "a":
		if len(c.stack) == 0 || c.top().kind != mdBufLink {
			return
		}
		href := c.stack[len(c.stack)-1].href
		text := strings.TrimSpace(c.pop())
		if text == "" {
			return
		}
		c.top().b.WriteString("[" + text + "](" + href + ")")
	case "th", "td":
		if c.table == nil || len(c.table.rows) == 0 || len(c.stack) == 0 || c.top().kind != mdBufCell {
			return
		}
		var sb strings.Builder
		mdWriteCollapsed(&sb, c.pop())
		cell := strings.TrimSpace(sb.String())
		cell = strings.ReplaceAll(cell, "|", "\\|")
		rows := c.table.rows
		rows[len(rows)-1] = append(rows[len(rows)-1], cell)
	case "table":
		if c.table == nil {
			return
		}
		rows := c.table.rows
		c.table = nil
		if len(rows) == 0 {
			return
		}
		b := c.top().b
		mdEnsureBlankLine(b)
		widths := len(rows[0])
		writeRow := func(row []string) {
			b.WriteString("|")
			for i := 0; i < widths; i++ {
				cell := ""
				if i < len(row) {
					cell = row[i]
				}
				b.WriteString(" " + cell + " |")
			}
			b.WriteString("\n")
		}
		writeRow(rows[0])
		b.WriteString("|")
		for i := 0; i < widths; i++ {
			b.WriteString(" --- |")
		}
		b.WriteString("\n")
		for _, row := range rows[1:] {
			writeRow(row)
		}
		mdEnsureBlankLine(b)
	case "strong", "b":
		c.top().b.WriteString("**")
	case "em", "i":
		c.top().b.WriteString("*")
	case "del", "s":
		c.top().b.WriteString("~~")
	}
}

// mdCodeLang extracts a language name from a class attribute like
// "language-go hljs", for fenced code blocks.
func mdCodeLang(class string) string {
	for _, f := range strings.Fields(class) {
		if strings.HasPrefix(f, "language-") {
			return strings.TrimPrefix(f, "language-")
		}
	}
	return ""
}

func (c *mdConv) finish() string {
	// Unclosed buffers (a link or quote the page never closed)
	// flush as plain text rather than vanishing.
	for len(c.stack) > 0 {
		kind := c.top().kind
		href := ""
		if kind == mdBufLink {
			href = c.stack[len(c.stack)-1].href
		}
		text := strings.TrimSpace(c.pop())
		if text == "" {
			continue
		}
		if kind == mdBufLink {
			c.top().b.WriteString("[" + text + "](" + href + ")")
		} else {
			c.top().b.WriteString(text)
		}
	}
	var lines []string
	for _, l := range strings.Split(c.out.String(), "\n") {
		lines = append(lines, strings.TrimRight(l, " \t"))
	}
	// Collapse runs of blank lines to one.
	var out []string
	blank := false
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}
