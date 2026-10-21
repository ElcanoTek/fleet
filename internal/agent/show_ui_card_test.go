package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// A by-reference show_ui card is persisted on its result row (so a reload and
// a second tab draw it), and replay sends the model only the result's text,
// never the card. Every other result row is unchanged (no card key).
func TestShowUICard_PersistedNotReplayed(t *testing.T) {
	const card = `{"title":"Intake","components":[{"type":"text","text":"hi"}]}`
	entries := mapRunEntries([]agentcore.RunEntry{
		{Type: "tool_call", ToolCallID: "ui1", ToolName: "show_ui", ToolInput: `{"from_result":{"tool":"intake_card"}}`},
		{Type: "tool_result", ToolCallID: "ui1", ToolName: "show_ui", Text: "UI_DISPLAYED card_id=ui1", Card: card},
		{Type: "tool_call", ToolCallID: "b1", ToolName: "bash", ToolInput: `{}`},
		{Type: "tool_result", ToolCallID: "b1", ToolName: "bash", Text: "ok"},
	})
	var shown, plain ToolResultContent
	if err := json.Unmarshal(entries[1].Content, &shown); err != nil || shown.Card != card {
		t.Fatalf("show_ui row = %s (%v)", entries[1].Content, err)
	}
	if err := json.Unmarshal(entries[3].Content, &plain); err != nil || strings.Contains(string(entries[3].Content), `"card"`) {
		t.Fatalf("a plain result row gained a card key: %s", entries[3].Content)
	}

	msgs, err := replayHistory(entries, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		for _, part := range m.Content {
			tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok || tr.ToolCallID != "ui1" {
				continue
			}
			txt, _ := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output)
			if txt.Text != "UI_DISPLAYED card_id=ui1" {
				t.Fatalf("the model is replayed %q for the card's result", txt.Text)
			}
			return
		}
	}
	t.Fatal("the show_ui result was not replayed")
}
