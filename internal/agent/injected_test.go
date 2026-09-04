package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
)

const injectedAttachmentBlock = "\n\n---\n**User attached files:**\n" +
	"- `spend.csv` (1.2 KB, /var/lib/fleet/workspace/conv-a/attachments/spend.csv)\n"

// The model must see exactly what it saw before the split: the user's text
// followed by the injected suffix, with the same byte layout the block
// appenders produced when they concatenated onto the message directly. That is
// the prompt-cache prefix-stability requirement (docs/PROMPT-CACHE-CONTRACT.md)
// — a replayed old turn must not read as a new prefix.
func TestComposeUserMessage(t *testing.T) {
	if got := ComposeUserMessage("hello", ""); got != "hello" {
		t.Errorf("no injected context should be the identity, got %q", got)
	}
	if got := ComposeUserMessage("hello", injectedAttachmentBlock); got != "hello"+injectedAttachmentBlock {
		t.Errorf("composed = %q", got)
	}
	// The appenders trimmed the trailing newlines of whatever they appended
	// to; composition reproduces that, so a message the user ended with a
	// newline composes identically to the old single-blob build.
	if got := ComposeUserMessage("hello\n\n", injectedAttachmentBlock); got != "hello"+injectedAttachmentBlock {
		t.Errorf("trailing newlines must be trimmed at the seam, got %q", got)
	}
	// An empty message with injected context is legal (attachment-only turn).
	if got := ComposeUserMessage("", injectedAttachmentBlock); got != injectedAttachmentBlock {
		t.Errorf("composed = %q", got)
	}
}

func TestStripLegacyInjectedContext(t *testing.T) {
	for _, block := range injectedBlockMarkers {
		text, injected, stripped := StripLegacyInjectedContext("ask" + block + " tail")
		if !stripped || text != "ask" || !strings.HasPrefix(injected, block) {
			t.Errorf("marker %q: text=%q injected=%q stripped=%v", block, text, injected, stripped)
		}
	}
	// Cuts at the EARLIEST marker, so every later block goes with it — that is
	// what makes the strip cover blocks added after this list was written.
	text, injected, stripped := StripLegacyInjectedContext(
		"ask" + injectedAttachmentBlock + "\n\n---\n**Shared file library** (…)\n" + "\n\n---\n**Something Newer**\n")
	if !stripped || text != "ask" {
		t.Fatalf("text = %q, stripped = %v", text, stripped)
	}
	if !strings.Contains(injected, "Shared file library") || !strings.Contains(injected, "Something Newer") {
		t.Errorf("later blocks must ride along in the injected half: %q", injected)
	}
	// No marker: nothing is cut, including prose that names a header without
	// the injected separator in front of it.
	if text, injected, stripped := StripLegacyInjectedContext("what does **User attached files:** mean"); stripped || injected != "" || text != "what does **User attached files:** mean" {
		t.Errorf("plain prose was cut: text=%q injected=%q stripped=%v", text, injected, stripped)
	}
}

// replayHistory recomposes a stored turn for the model: the user text plus its
// injected context, so turn 4 can still see the attachment path turn 1 was
// given. Legacy rows (blocks inside the text, empty column) replay unchanged.
func TestReplayHistoryRecomposesInjectedContext(t *testing.T) {
	raw, err := json.Marshal(TextContent{Text: "what is the CPM?"})
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := replayHistory([]HistoryEntry{
		{Role: "user", Type: "text", Content: raw, InjectedContext: injectedAttachmentBlock},
	})
	if err != nil {
		t.Fatalf("replayHistory: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	var text string
	for _, part := range msgs[0].Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			text += tp.Text
		}
	}
	if text != "what is the CPM?"+injectedAttachmentBlock {
		t.Errorf("replayed user message = %q; want the recomposed turn", text)
	}
}

// The turn's user entry is persisted SPLIT — the user's words in content, the
// injected suffix beside them — while the model message is composed. This is
// the write that makes a branch copy clean.
func TestAssembleTurnMessagesSplitsThePersistedEntry(t *testing.T) {
	msgs, entry, err := assembleTurnMessages(TurnInput{
		UserMessage:     "what is the CPM?",
		InjectedContext: injectedAttachmentBlock,
	})
	if err != nil {
		t.Fatalf("assembleTurnMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	var sent string
	for _, part := range msgs[0].Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			sent += tp.Text
		}
	}
	if sent != "what is the CPM?"+injectedAttachmentBlock {
		t.Errorf("model saw %q; want the composed message", sent)
	}
	if entry.InjectedContext != injectedAttachmentBlock {
		t.Errorf("entry.InjectedContext = %q", entry.InjectedContext)
	}
	var tc TextContent
	if err := json.Unmarshal(entry.Content, &tc); err != nil {
		t.Fatal(err)
	}
	if tc.Text != "what is the CPM?" {
		t.Errorf("persisted text = %q; want ONLY what the user typed", tc.Text)
	}
}

// The field must never serialize by default: the same struct is projected into
// the public share snapshot and the team-shared read view, which expose "the
// transcript only". A handler that wants it names it in its own shape.
func TestHistoryEntryDoesNotMarshalInjectedContext(t *testing.T) {
	raw, err := json.Marshal(HistoryEntry{
		Role: "user", Type: "text",
		Content:         json.RawMessage(`{"text":"hi"}`),
		InjectedContext: injectedAttachmentBlock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "injected_context") || strings.Contains(string(raw), "attachments/spend.csv") {
		t.Errorf("HistoryEntry serialized its injected context: %s", raw)
	}
}
