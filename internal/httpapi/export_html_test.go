package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

func htmlFixture(t *testing.T) (*store.Conversation, []agent.HistoryEntry) {
	t.Helper()
	conv := &store.Conversation{ID: "abc123def456", Title: "Nissan CPA forecast", Model: "anthropic/claude", Persona: "victoria"}
	history := []agent.HistoryEntry{
		entry(t, "user", "text", agent.TextContent{Text: "Forecast the **achievable** CPA."}),
		entry(t, "assistant", "reasoning", agent.ReasoningContent{Text: "weighing the blend"}),
		entry(t, "assistant", "tool_call", agent.ToolCallContent{Name: "bash", Input: `{"command":"unzip"}`}),
		entry(t, "tool", "tool_result", agent.ToolResultContent{Name: "bash", Text: "12 files"}),
		entry(t, "assistant", "summary", agent.SummaryContent{Text: "earlier turns compacted", Model: "anthropic/claude"}),
		entry(t, "assistant", "text", agent.TextContent{Text: "A blended CPA of $41 is defensible."}),
	}
	return conv, history
}

// TestRenderConversationHTML_ReadableDefault proves the default download is the
// document a person wants to read or forward: the conversation, with the
// agent's working trail left out.
func TestRenderConversationHTML_ReadableDefault(t *testing.T) {
	conv, history := htmlFixture(t)
	at := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

	page := renderConversationHTML(conv, history, at, scopeConversation)

	for _, want := range []string{
		"<!doctype html>",
		"<title>Nissan CPA forecast</title>",
		"<h1>Nissan CPA forecast</h1>",
		"28 June 2026",
		"Assistant: victoria",
		"Model: anthropic/claude",
		`<section class="turn user">`,
		"<strong>achievable</strong>", // the message's Markdown is rendered, not shown raw
		"A blended CPA of $41 is defensible.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	for _, unwanted := range []string{"weighing the blend", "Used tool", "12 files", "earlier turns compacted"} {
		if strings.Contains(page, unwanted) {
			t.Errorf("readable scope leaked the working trail: %q", unwanted)
		}
	}
}

// TestRenderConversationHTML_FullScope adds the working trail back, collapsed.
func TestRenderConversationHTML_FullScope(t *testing.T) {
	conv, history := htmlFixture(t)
	page := renderConversationHTML(conv, history, time.Unix(0, 0).UTC(), scopeFull)

	for _, want := range []string{
		"<summary>Thinking</summary>",
		"<summary>Used tool: bash</summary>",
		"Tool result: bash",
		"Earlier messages, summarized",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("full scope missing %q\n%s", want, page)
		}
	}
}

// TestRenderConversationHTML_EscapesUntrustedText is the security test for this
// renderer. Message text is model- and tool-authored and the export is opened
// from a file:// origin, so NOTHING in a message may become live markup — the
// Markdown renderer runs without goldmark's unsafe option, and every field
// outside it goes through html.EscapeString.
func TestRenderConversationHTML_EscapesUntrustedText(t *testing.T) {
	conv := &store.Conversation{
		ID:      "x",
		Title:   `<img src=x onerror=alert(1)>`,
		Model:   `"><script>alert(2)</script>`,
		Persona: `<b>persona</b>`,
	}
	history := []agent.HistoryEntry{
		entry(t, "user", "text", agent.TextContent{Text: "<script>alert('user turn')</script>"}),
		entry(t, "assistant", "text", agent.TextContent{Text: "<iframe src=\"evil\"></iframe>"}),
		entry(t, "assistant", "tool_call", agent.ToolCallContent{Name: "<script>x</script>", Input: `{"a":"</script><script>alert(3)</script>"}`}),
		entry(t, "tool", "tool_result", agent.ToolResultContent{Name: "sh", Text: "<script>alert(4)</script>"}),
	}

	page := renderConversationHTML(conv, history, time.Unix(0, 0).UTC(), scopeFull)

	// This renderer emits no <script>, <iframe>, <img>, <object> or <svg> tag
	// of its own, so an unescaped one in the output could only have come from
	// the untrusted text. (An escaped "&lt;img ... onerror=..." is inert text
	// and is expected to appear — the attribute names survive as characters.)
	for _, banned := range []string{"<script", "<iframe", "<img", "<object", "<svg"} {
		if strings.Contains(strings.ToLower(page), banned) {
			t.Fatalf("untrusted text became live markup (%q) in:\n%s", banned, page)
		}
	}
	if !strings.Contains(page, "&lt;img src=x onerror=alert(1)&gt;") {
		t.Errorf("the title should survive as escaped, readable text:\n%s", page)
	}
	if !strings.Contains(page, "alert(&#39;user turn&#39;)") && !strings.Contains(page, "alert('user turn')") {
		t.Error("the escaped text should still be readable to the user")
	}
}

// TestRenderConversationHTML_Degrades keeps the export best-effort: an
// undecodable entry is skipped, an empty title still names the document, and a
// blank message doesn't emit an empty card.
func TestRenderConversationHTML_Degrades(t *testing.T) {
	conv := &store.Conversation{ID: "x"}
	history := []agent.HistoryEntry{
		{Role: "assistant", Type: "text", Content: json.RawMessage(`not valid json`)},
		entry(t, "assistant", "text", agent.TextContent{Text: "   "}),
		entry(t, "user", "text", agent.TextContent{Text: "hi"}),
	}
	page := renderConversationHTML(conv, history, time.Unix(0, 0).UTC(), scopeConversation)
	if !strings.Contains(page, "<h1>Untitled conversation</h1>") {
		t.Errorf("empty title should still name the document:\n%s", page)
	}
	if got := strings.Count(page, `<section class="turn`); got != 1 {
		t.Errorf("rendered %d turn cards, want 1 (the undecodable and blank entries are skipped)", got)
	}
}

// TestParseExportScope pins the query contract: the readable transcript is the
// default, and an unrecognized value is never an error.
func TestParseExportScope(t *testing.T) {
	for value, want := range map[string]exportScope{
		"":             scopeConversation,
		"conversation": scopeConversation,
		"nonsense":     scopeConversation,
		"full":         scopeFull,
		"  FULL  ":     scopeFull,
		"all":          scopeFull,
	} {
		if got := parseExportScope(value); got != want {
			t.Errorf("parseExportScope(%q) = %v, want %v", value, got, want)
		}
	}
}
