package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// renderConversationHTML renders a conversation as a single self-contained web
// page: one file, no external assets, opens on a double-click in any browser,
// and prints to PDF from Ctrl/Cmd-P. It exists because "download the chat" is
// most often "I want to keep this, read it later, or send it to someone" — and
// for that, a .json file (or even a .md one) is a worse answer than a page that
// simply looks like the conversation.
//
// SECURITY: message text is model- and tool-authored, and this file is opened
// from disk (a file:// origin). The Markdown renderer is built WITHOUT
// goldmark's html.WithUnsafe() option, so raw HTML inside a message can never
// become markup — a tool result carrying a <script> tag renders as characters.
// Everything not passed through the Markdown renderer (titles, tool names, tool
// JSON, tool output) is written through html.EscapeString. There is no template
// execution and no template.HTML conversion anywhere in this file: the only
// markup is the literal shell below.
func renderConversationHTML(conv *store.Conversation, history []agent.HistoryEntry, exportedAt time.Time, scope exportScope) string {
	title := strings.TrimSpace(conv.Title)
	if title == "" {
		title = "Untitled conversation"
	}

	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(
			renderer.WithNodeRenderers(util.Prioritized(rawHTMLAsText{}, 1)),
		),
	)
	// render turns a message's Markdown into HTML. On the (unexpected) failure
	// of a pure in-memory conversion, fall back to the escaped source text so
	// the export never silently drops a turn.
	render := func(src string) string {
		var buf bytes.Buffer
		if err := md.Convert([]byte(src), &buf); err != nil {
			return "<p>" + html.EscapeString(src) + "</p>"
		}
		return buf.String()
	}

	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	fmt.Fprintf(&b, "<title>%s</title>\n", html.EscapeString(title))
	b.WriteString(exportStyles)
	b.WriteString("</head>\n<body>\n<main>\n")

	fmt.Fprintf(&b, "<h1>%s</h1>\n", html.EscapeString(title))
	b.WriteString("<p class=\"meta\">")
	fmt.Fprintf(&b, "Exported %s", html.EscapeString(exportedAt.UTC().Format("2 January 2006 at 15:04 MST")))
	if conv.Persona != "" {
		fmt.Fprintf(&b, " &middot; Assistant: %s", html.EscapeString(conv.Persona))
	}
	if conv.Model != "" {
		fmt.Fprintf(&b, " &middot; Model: %s", html.EscapeString(conv.Model))
	}
	b.WriteString("</p>\n")

	for _, e := range history {
		if !scope.keeps(e.Type) {
			continue
		}
		switch e.Type {
		case "text":
			var c agent.TextContent
			if json.Unmarshal(e.Content, &c) != nil {
				continue
			}
			if strings.TrimSpace(c.Text) == "" {
				continue
			}
			// The user's own turns get the tinted card so a reader can scan the
			// page for "what was asked" without reading every reply.
			class := "turn assistant"
			if e.Role == "user" {
				class = "turn user"
			}
			fmt.Fprintf(&b, "<section class=%q>\n<div class=\"who\">%s</div>\n%s</section>\n",
				class, html.EscapeString(roleHeading(e.Role)), render(c.Text))
		case "reasoning":
			var c agent.ReasoningContent
			if json.Unmarshal(e.Content, &c) != nil {
				continue
			}
			// Collapsed: the working trail is context, not the document.
			fmt.Fprintf(&b, "<details class=\"trail\">\n<summary>Thinking</summary>\n%s</details>\n", render(c.Text))
		case entryTypeToolCallMD:
			var c agent.ToolCallContent
			if json.Unmarshal(e.Content, &c) != nil {
				continue
			}
			fmt.Fprintf(&b, "<details class=\"trail\">\n<summary>Used tool: %s</summary>\n<pre>%s</pre>\n</details>\n",
				html.EscapeString(c.Name), html.EscapeString(c.Input))
		case "tool_result":
			var c agent.ToolResultContent
			if json.Unmarshal(e.Content, &c) != nil {
				continue
			}
			label := "Tool result"
			if c.IsErr {
				label = "Tool error"
			}
			if c.Name != "" {
				label += ": " + c.Name
			}
			fmt.Fprintf(&b, "<details class=\"trail\">\n<summary>%s</summary>\n<pre>%s</pre>\n</details>\n",
				html.EscapeString(label), html.EscapeString(c.Text))
		case "summary":
			var c agent.SummaryContent
			if json.Unmarshal(e.Content, &c) != nil {
				continue
			}
			fmt.Fprintf(&b, "<aside class=\"summary\">\n<div class=\"who\">Earlier messages, summarized</div>\n%s</aside>\n", render(c.Text))
		}
	}

	b.WriteString("</main>\n</body>\n</html>\n")
	return b.String()
}

// exportStyles is the whole stylesheet for an exported page. It is inlined
// (rather than linked) so the download is one portable file, and it carries a
// print block so Ctrl-P produces a clean PDF: turns are kept off page breaks,
// and a working-trail disclosure the reader left collapsed is dropped from the
// printout rather than printed as a stray "Thinking" label with nothing under
// it. (CSS cannot force a <details> open, so print follows what is on screen.)
const exportStyles = `<style>
:root { color-scheme: light; }
* { box-sizing: border-box; }
body {
  margin: 0;
  background: #f6f7f9;
  color: #1c1f23;
  font: 16px/1.65 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
}
main { max-width: 46rem; margin: 0 auto; padding: 3rem 1.25rem 4rem; }
h1 { font-size: 1.75rem; line-height: 1.25; margin: 0 0 .35rem; }
.meta { color: #5d646d; font-size: .85rem; margin: 0 0 2rem; }
.turn { border-radius: 12px; padding: 1rem 1.25rem; margin: 0 0 1rem; background: #fff; border: 1px solid #e3e6ea; }
.turn.user { background: #eef4ff; border-color: #d3e0f8; }
.who { font-size: .72rem; letter-spacing: .08em; text-transform: uppercase; color: #5d646d; font-weight: 600; margin-bottom: .5rem; }
.turn > :last-child, .summary > :last-child { margin-bottom: 0; }
.turn p:first-of-type { margin-top: 0; }
.summary { border-left: 3px solid #c3c9d1; background: #fff; padding: .85rem 1.25rem; margin: 0 0 1rem; color: #41474e; }
details.trail { margin: 0 0 1rem; font-size: .9rem; color: #41474e; }
details.trail > summary { cursor: pointer; color: #5d646d; padding: .35rem 0; }
pre { background: #f2f3f5; border: 1px solid #e3e6ea; border-radius: 8px; padding: .75rem 1rem; overflow-x: auto; font-size: .82rem; }
code { background: #f2f3f5; border-radius: 4px; padding: .1rem .3rem; font-size: .88em; }
pre code { background: none; padding: 0; }
table { border-collapse: collapse; width: 100%; margin: 0 0 1rem; font-size: .92rem; }
th, td { border: 1px solid #e3e6ea; padding: .4rem .6rem; text-align: left; }
th { background: #f2f3f5; }
img { max-width: 100%; height: auto; }
blockquote { margin: 0 0 1rem; padding-left: 1rem; border-left: 3px solid #d7dbe0; color: #41474e; }
a { color: #1a56b8; }
@media print {
  body { background: #fff; }
  main { max-width: none; padding: 0; }
  .turn, .summary, pre { break-inside: avoid; }
  .turn { border-color: #d7dbe0; }
  details.trail:not([open]) { display: none; }
  details.trail > summary { list-style: none; font-weight: 600; color: #41474e; }
}
</style>
`

// rawHTMLAsText renders Markdown's raw-HTML nodes as visible, escaped text.
//
// goldmark's safe default replaces raw HTML with an invisible
// "<!-- raw HTML omitted -->" comment, which silently DELETES content: an
// assistant reply explaining a <div> would export with the explanation gone.
// Showing the markup as text keeps the export faithful and matches what the
// reader saw on screen, while staying exactly as inert as the omission was.
// Registered at priority 1 so it wins over goldmark's own renderer (1000).
type rawHTMLAsText struct{}

func (rawHTMLAsText) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindRawHTML, renderRawHTMLInline)
	reg.Register(ast.KindHTMLBlock, renderHTMLBlockAsText)
}

func renderRawHTMLInline(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkSkipChildren, nil
	}
	raw, ok := n.(*ast.RawHTML)
	if !ok {
		return ast.WalkSkipChildren, nil
	}
	for i := 0; i < raw.Segments.Len(); i++ {
		seg := raw.Segments.At(i)
		_, _ = w.WriteString(html.EscapeString(string(seg.Value(source))))
	}
	return ast.WalkSkipChildren, nil
}

func renderHTMLBlockAsText(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkSkipChildren, nil
	}
	block, ok := n.(*ast.HTMLBlock)
	if !ok {
		return ast.WalkSkipChildren, nil
	}
	// A <pre> keeps the block's own line breaks, which is how the reader saw it.
	_, _ = w.WriteString("<pre>")
	lines := block.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		_, _ = w.WriteString(html.EscapeString(string(seg.Value(source))))
	}
	if block.HasClosure() {
		_, _ = w.WriteString(html.EscapeString(string(block.ClosureLine.Value(source))))
	}
	_, _ = w.WriteString("</pre>\n")
	return ast.WalkSkipChildren, nil
}
