package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// A turn fleet starts itself (docs/RESUME-AFTER-APPROVAL.md) persists its
// input with Kind set, so every reader can tell it from the user's words, and
// the model still reads the labelled text as the turn's user message. An
// ordinary turn's entry is byte-for-byte what it was before (no "kind" key).
func TestAssembleTurnMessages_InputKind(t *testing.T) {
	const notice = "[Approval resolved] mcp_deals_update_deal approval_id=a1 outcome=approved."
	msgs, entry, err := assembleTurnMessages(TurnInput{UserMessage: notice, InputKind: InputKindApprovalResume})
	if err != nil {
		t.Fatal(err)
	}
	var c TextContent
	if err := json.Unmarshal(entry.Content, &c); err != nil {
		t.Fatal(err)
	}
	if entry.Role != "user" || c.Kind != InputKindApprovalResume || c.Text != notice {
		t.Fatalf("user entry = %s %s, want a user entry of kind %q with the notice text", entry.Role, entry.Content, InputKindApprovalResume)
	}
	last := msgs[len(msgs)-1]
	if last.Role != fantasy.MessageRoleUser || !strings.Contains(messageText(last), notice) {
		t.Fatalf("the model must read the notice as the turn's user message, got %+v", last)
	}

	_, plain, err := assembleTurnMessages(TurnInput{UserMessage: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain.Content), "kind") {
		t.Fatalf("an ordinary user entry must not carry a kind: %s", plain.Content)
	}

	// Replay: the resume input stays a user message for later turns, and a
	// notice entry never reaches the model.
	history := []HistoryEntry{entry, mustEntry("assistant", "text", TextContent{Text: "verified"}),
		mustEntry("system", EntryTypeNotice, NoticeContent{Kind: "approval_resume_skipped", Text: "skipped"})}
	replayed, err := replayHistory(history, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 2 || replayed[0].Role != fantasy.MessageRoleUser || !strings.Contains(messageText(replayed[0]), notice) {
		t.Fatalf("replay = %+v, want the resume input as a user message and no notice", replayed)
	}
}

func messageText(m fantasy.Message) string {
	var b strings.Builder
	for _, part := range m.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			b.WriteString(tp.Text)
		}
	}
	return b.String()
}
