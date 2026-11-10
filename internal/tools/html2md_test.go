package tools

import (
	"strings"
	"testing"
)

func TestHTMLToMarkdown(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			`<h1>Title</h1><p>Hello <b>bold</b> world.</p>`,
			"# Title\n\nHello **bold** world.",
		},
		{
			`<h2>Sub</h2><h3>Deep</h3>`,
			"## Sub\n\n### Deep",
		},
		{
			`<ul><li>one</li><li>two</li></ul>`,
			"- one\n- two",
		},
		{
			`<ol><li>one</li><li>two</li></ol>`,
			"1. one\n2. two",
		},
		{
			`<ul><li>parent<ul><li>child</li></ul></li><li>next</li></ul>`,
			"- parent\n  - child\n- next",
		},
		{
			`<p>See <a href="https://example.com">the docs</a> now.</p>`,
			"See [the docs](https://example.com) now.",
		},
		{
			`<p>Run <code>go test</code> now.</p>`,
			"Run `go test` now.",
		},
		{
			`<pre><code class="language-go">fmt.Println("hi")</code></pre>`,
			"```go\nfmt.Println(\"hi\")\n```",
		},
		{
			`<pre>  keep  spaces
  and lines
</pre>`,
			"```\n  keep  spaces\n  and lines\n```",
		},
		{
			`<blockquote><p>quoted text</p></blockquote>`,
			"> quoted text",
		},
		{
			`<blockquote><blockquote>deep</blockquote></blockquote>`,
			"> > deep",
		},
		{
			`<table><tr><th>Name</th><th>Age</th></tr><tr><td>Ada</td><td>36</td></tr></table>`,
			"| Name | Age |\n| --- | --- |\n| Ada | 36 |",
		},
		{
			`<table><tr><td>a | b</td></tr></table>`,
			"| a \\| b |\n| --- |",
		},
		{
			`<head><title>Hidden</title></head><body><script>var x = "<div>";</script><style>.x{color:red}</style><p>hi</p></body>`,
			"hi",
		},
		{
			`<script>if (a) { s = "</div>"; }</script><p>ok</p>`,
			"ok",
		},
		{
			// A '>' inside a quoted attribute must not end the tag
			// (the old regex stripper got this wrong).
			`<a href="https://example.com/?a=1>2">link</a>`,
			"[link](https://example.com/?a=1>2)",
		},
		{
			`<p>a &lt;b&gt; &amp; &quot;q&quot; &nbsp; b</p>`,
			`a <b> & "q" b`,
		},
		{
			`<p><strong>bold</strong> and <em>italic</em> and <del>gone</del></p>`,
			"**bold** and *italic* and ~~gone~~",
		},
		{
			`<p>a</p><hr><p>b</p>`,
			"a\n\n---\n\nb",
		},
		{
			`<p><img src="cat.png" alt="a cat"></p>`,
			"![a cat](cat.png)",
		},
		{
			`<p>unclosed`,
			"unclosed",
		},
		{
			`a < b and c > d`,
			"a < b and c > d",
		},
		{
			`<a href="/x"><strong>bold link</strong></a>`,
			"[**bold link**](/x)",
		},
		{
			// Unclosed <head>: the title still drops, the body
			// still shows.
			`<head><title>Secret</title><p>body</p>`,
			"body",
		},
		{
			// Entities inside code are decoded, not re-tagged.
			`<p><code>if (a &lt; b)</code></p>`,
			"`if (a < b)`",
		},
	}
	for _, c := range cases {
		if got := htmlToMarkdown(c.in); got != c.want {
			t.Fatalf("htmlToMarkdown(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHTMLToMarkdownNoRawTags(t *testing.T) {
	in := `<!doctype html><html><head><title>T</title></head><body><h1>H</h1><p>Text with <a href="/x">a link</a>, <code>code</code> and <b>bold</b>.</p><ul><li>item</li></ul></body></html>`
	got := htmlToMarkdown(in)
	for _, nope := range []string{"<h1>", "</h1>", "<p>", "<a ", "<code>", "<b>", "<title>", "T\n"} {
		if strings.Contains(got, nope) {
			t.Fatalf("markdown = %q, must not contain %q", got, nope)
		}
	}
	for _, want := range []string{"# H", "[a link](/x)", "`code`", "**bold**", "- item"} {
		if !strings.Contains(got, want) {
			t.Fatalf("markdown = %q, want %q", got, want)
		}
	}
}
