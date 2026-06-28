package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

func entry(t *testing.T, role, typ string, content any) agent.HistoryEntry {
	t.Helper()
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	return agent.HistoryEntry{Role: role, Type: typ, Content: raw}
}

func TestRenderConversationMarkdown(t *testing.T) {
	conv := &store.Conversation{ID: "abc123def456", Title: "Debug session", Model: "anthropic/claude", Persona: "engineer"}
	at := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	history := []agent.HistoryEntry{
		entry(t, "user", "text", agent.TextContent{Text: "find the bug"}),
		entry(t, "assistant", "reasoning", agent.ReasoningContent{Text: "let me think"}),
		entry(t, "assistant", "tool_call", agent.ToolCallContent{Name: "bash", Input: `{"command":"ls"}`}),
		entry(t, "tool", "tool_result", agent.ToolResultContent{Name: "bash", Text: "main.go", IsErr: false}),
		entry(t, "tool", "tool_result", agent.ToolResultContent{Name: "bash", Text: "boom", IsErr: true}),
		entry(t, "assistant", "summary", agent.SummaryContent{Text: "fixed it\nshipped", Model: "anthropic/claude"}),
		entry(t, "assistant", "text", agent.TextContent{Text: "done"}),
	}

	md := renderConversationMarkdown(conv, history, at)

	mustContain := []string{
		"# Conversation: Debug session",
		"Exported: 2026-06-28T12:00:00Z",
		"Model: anthropic/claude",
		"Persona: engineer",
		"## User\n\nfind the bug",
		"<details>\n<summary>Reasoning</summary>",
		"### Tool: bash",
		"```json\n{\"command\":\"ls\"}\n```",
		"**Output:**\n```\nmain.go\n```",
		"**Output ⚠ error:**\n```\nboom\n```",
		"> **Summary** (by anthropic/claude)",
		"> fixed it\n> shipped", // multi-line summary stays quoted
		"## Assistant\n\ndone",
	}
	for _, want := range mustContain {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n--- full ---\n%s", want, md)
		}
	}
}

func TestRenderConversationMarkdown_Defaults(t *testing.T) {
	// Empty title + no model/persona: header degrades gracefully, no panic on a
	// malformed entry (skipped).
	conv := &store.Conversation{ID: "x"}
	history := []agent.HistoryEntry{
		{Role: "assistant", Type: "text", Content: json.RawMessage(`not valid json`)}, // skipped
		entry(t, "user", "text", agent.TextContent{Text: "hi"}),
	}
	md := renderConversationMarkdown(conv, history, time.Unix(0, 0).UTC())
	if !strings.Contains(md, "# Conversation: Untitled") {
		t.Errorf("empty title should render 'Untitled', got:\n%s", md)
	}
	if strings.Contains(md, "Model:") {
		t.Errorf("no model should omit the Model field, got:\n%s", md)
	}
	if !strings.Contains(md, "## User\n\nhi") {
		t.Errorf("valid entry after a malformed one should still render, got:\n%s", md)
	}
}

func TestExportFilename_Extension(t *testing.T) {
	if got := exportFilename("My Chat!", "abcdef1234567890", "md"); got != "My-Chat-abcdef12.md" {
		t.Errorf("got %q, want My-Chat-abcdef12.md", got)
	}
	if got := exportFilename("", "id", "json"); got != "chat-id.json" {
		t.Errorf("got %q, want chat-id.json", got)
	}
}
