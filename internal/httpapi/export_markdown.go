package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// renderConversationMarkdown renders a conversation's replay history (#210) as
// human-readable Markdown: a header, then each turn entry by type (text /
// reasoning / tool_call / tool_result / summary). Best-effort — an entry whose
// content fails to decode is skipped rather than aborting the export. Pure
// (no I/O), so it is unit-testable.
func renderConversationMarkdown(conv *store.Conversation, history []agent.HistoryEntry, exportedAt time.Time) string {
	var b strings.Builder

	title := strings.TrimSpace(conv.Title)
	if title == "" {
		title = "Untitled"
	}
	fmt.Fprintf(&b, "# Conversation: %s\n\n", title)
	fmt.Fprintf(&b, "Exported: %s", exportedAt.UTC().Format(time.RFC3339))
	if conv.Model != "" {
		fmt.Fprintf(&b, " | Model: %s", conv.Model)
	}
	if conv.Persona != "" {
		fmt.Fprintf(&b, " | Persona: %s", conv.Persona)
	}
	b.WriteString("\n\n---\n")

	for _, e := range history {
		switch e.Type {
		case "text":
			var c agent.TextContent
			if json.Unmarshal(e.Content, &c) != nil {
				continue
			}
			fmt.Fprintf(&b, "\n## %s\n\n%s\n\n---\n", roleHeading(e.Role), c.Text)
		case "reasoning":
			var c agent.ReasoningContent
			if json.Unmarshal(e.Content, &c) != nil {
				continue
			}
			// Collapsed disclosure so the (often long) reasoning doesn't dominate.
			fmt.Fprintf(&b, "\n<details>\n<summary>Reasoning</summary>\n\n%s\n\n</details>\n\n---\n", c.Text)
		case entryTypeToolCallMD:
			var c agent.ToolCallContent
			if json.Unmarshal(e.Content, &c) != nil {
				continue
			}
			fmt.Fprintf(&b, "\n### Tool: %s\n\n**Input:**\n```json\n%s\n```\n\n---\n", c.Name, c.Input)
		case "tool_result":
			var c agent.ToolResultContent
			if json.Unmarshal(e.Content, &c) != nil {
				continue
			}
			errMark := ""
			if c.IsErr {
				errMark = " ⚠ error"
			}
			fmt.Fprintf(&b, "\n**Output%s:**\n```\n%s\n```\n\n---\n", errMark, c.Text)
		case "summary":
			var c agent.SummaryContent
			if json.Unmarshal(e.Content, &c) != nil {
				continue
			}
			model := c.Model
			if model == "" {
				model = "unknown"
			}
			// Markdown blockquote; indent each line so multi-line summaries stay quoted.
			quoted := strings.ReplaceAll(c.Text, "\n", "\n> ")
			fmt.Fprintf(&b, "\n> **Summary** (by %s)\n>\n> %s\n\n---\n", model, quoted)
		}
	}
	return b.String()
}

// entryTypeToolCallMD mirrors the agent package's tool-call entry type string
// (unexported there); kept local to the renderer's switch.
const entryTypeToolCallMD = "tool_call"

// roleHeading title-cases a history entry's role for a Markdown heading.
func roleHeading(role string) string {
	switch role {
	case "user":
		return "User"
	case "assistant":
		return "Assistant"
	case "tool":
		return "Tool"
	case "":
		return "Message"
	default:
		return strings.ToUpper(role[:1]) + role[1:]
	}
}
