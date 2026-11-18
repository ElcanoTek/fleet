package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

// The history of a staged critical call that the user approved after the turn
// ended, as chat.messages holds it (conversation 8b7b4ba2, 2026-11-18): the
// call, its inline APPROVAL_REQUIRED placeholder, the model's "nothing has
// changed yet", the turn_summary, then the real outcome appended under the
// SAME tool-call id when the card was approved ~50 s later.
const (
	stagedCallID   = "toolu_01Lr56"
	stagedToolName = "mcp_magnite_publisher_mcp_mgpub_execute_plan"
	stagedReply    = "I've staged an approval card. Nothing has changed yet."
	executedResult = `{"status":"applied","deals_updated":3}`
)

func stagedThenApprovedHistory() []HistoryEntry {
	return []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "apply the plan", RuntimeToday: "2026-11-18"}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: stagedCallID, Name: stagedToolName, Input: `{"plan_id":"p1"}`}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: stagedCallID, Name: stagedToolName,
			Text: "APPROVAL_REQUIRED: " + stagedToolName + " is a critical action and has been staged for explicit user approval (approval_id=a1).", IsErr: true}),
		mustEntry("assistant", "text", TextContent{Text: stagedReply}),
		mustEntry("assistant", "turn_summary", TurnSummaryContent{Model: "anthropic/claude-opus-4.7"}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: stagedCallID, Name: stagedToolName, Text: executedResult}),
	}
}

// TestAssembleTurnMessages_ApprovalResumeKeepsStagedTimeline is the
// regression for the resume turn that "corrected" an accurate reply: the
// model must read that the call was only staged when it wrote "nothing has
// changed yet", and that the executed result arrived after that reply.
func TestAssembleTurnMessages_ApprovalResumeKeepsStagedTimeline(t *testing.T) {
	const resume = "[Approval resolved] " + stagedToolName + " approval_id=a1 outcome=approved. The result is in the conversation; continue the task."
	msgs, _, err := assembleTurnMessages(context.Background(), TurnInput{
		History:     stagedThenApprovedHistory(),
		UserMessage: resume,
		InputKind:   InputKindApprovalResume,
	}, time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	assertWellFormedToolPairs(t, msgs)

	// user, its runtime_today note, assistant(call), tool(outcome),
	// assistant(staged reply), resume input, its runtime_today note.
	wantRoles := []fantasy.MessageRole{
		fantasy.MessageRoleUser, fantasy.MessageRoleUser,
		fantasy.MessageRoleAssistant, fantasy.MessageRoleTool, fantasy.MessageRoleAssistant,
		fantasy.MessageRoleUser, fantasy.MessageRoleUser,
	}
	if len(msgs) != len(wantRoles) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(wantRoles), msgs)
	}
	for i, m := range msgs {
		if m.Role != wantRoles[i] {
			t.Fatalf("msg %d role = %q, want %q", i, m.Role, wantRoles[i])
		}
	}

	got, isErr, ok := toolResultTextFor(t, msgs, stagedCallID)
	if !ok {
		t.Fatal("no tool result for the staged call")
	}
	if isErr {
		t.Errorf("the executed outcome replayed as an error: %q", got)
	}
	// The outcome is announced as arriving after the turn, before its text.
	if !strings.HasPrefix(got, approvalOutcomeNote(true)) {
		t.Errorf("outcome does not start with the after-turn note:\n%s", got)
	}
	if !strings.Contains(got, "settled after that turn ended") || !strings.Contains(got, "do not correct it") {
		t.Errorf("note does not give the timeline: %q", got)
	}
	if !strings.HasSuffix(got, executedResult) {
		t.Errorf("outcome text lost the executed result: %q", got)
	}
	// The placeholder the model saw is described, never re-sent as pending.
	if strings.Contains(got, "APPROVAL_REQUIRED") {
		t.Errorf("replayed outcome carries the placeholder sentinel: %q", got)
	}
	if messageText(msgs[4]) != stagedReply {
		t.Errorf("msg 4 = %q, want the staged reply after the outcome", messageText(msgs[4]))
	}
	if !strings.Contains(messageText(msgs[5]), "[Approval resolved]") {
		t.Errorf("msg 5 = %q, want the resume input", messageText(msgs[5]))
	}
	// The outcome keeps its tool-output provenance: no user or assistant
	// message carries it.
	for i, m := range msgs {
		if m.Role != fantasy.MessageRoleTool && strings.Contains(messageText(m), "deals_updated") {
			t.Errorf("msg %d (%s) carries the tool outcome outside a tool result", i, m.Role)
		}
	}
}

// A card settled while its turn was still running lands before its own call
// (out of order). The model still saw only the placeholder in that turn, so
// the note says the outcome was settled during the turn, unseen — never that
// it came after the replies.
func TestReplayHistory_ApprovalOutcomeSettledDuringTurn(t *testing.T) {
	entries := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "send it", RuntimeToday: "2026-11-18"}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "s1", Name: "send_email", Text: `{"status_code":202}`}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "s1", Name: "send_email", Input: "{}"}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "s1", Name: "send_email", Text: "APPROVAL_REQUIRED: staged", IsErr: true}),
		mustEntry("assistant", "text", TextContent{Text: "Staged; not sent yet."}),
		mustEntry("assistant", "turn_summary", TurnSummaryContent{}),
	}
	msgs, err := replayHistory(context.Background(), entries, "")
	if err != nil {
		t.Fatal(err)
	}
	assertWellFormedToolPairs(t, msgs)
	got, isErr, ok := toolResultTextFor(t, msgs, "s1")
	if !ok || isErr {
		t.Fatalf("result = (%q, isErr=%v, ok=%v), want the 202 outcome", got, isErr, ok)
	}
	if !strings.HasPrefix(got, approvalOutcomeNote(false)) || !strings.HasSuffix(got, `{"status_code":202}`) {
		t.Errorf("out-of-order outcome = %q, want the during-turn note then the outcome", got)
	}
}

// A declined, timed-out or failed outcome keeps its error bit and gets the
// same timeline.
func TestReplayHistory_ApprovalOutcomeErrorKeepsNote(t *testing.T) {
	entries := stagedThenApprovedHistory()
	entries[len(entries)-1] = mustEntry("tool", "tool_result", ToolResultContent{ID: stagedCallID, Name: stagedToolName,
		Text: "Approval timed out — auto-denied. The action was not taken.", IsErr: true})
	msgs, err := replayHistory(context.Background(), entries, "")
	if err != nil {
		t.Fatal(err)
	}
	got, isErr, ok := toolResultTextFor(t, msgs, stagedCallID)
	if !ok || !isErr {
		t.Fatalf("result = (%q, isErr=%v, ok=%v), want an error result", got, isErr, ok)
	}
	if !strings.HasPrefix(got, approvalOutcomeNote(true)) || !strings.HasSuffix(got, "The action was not taken.") {
		t.Errorf("timed-out outcome = %q, want the after-turn note then the outcome", got)
	}
}

// Nothing else is annotated: a card still pending replays its placeholder as
// it was, an ordinary result is untouched, and a later result for a call
// whose inline result was not an approval placeholder carries no note.
func TestReplayHistory_ApprovalOutcomeNoteOnlyForSettledStagedCalls(t *testing.T) {
	pending := stagedThenApprovedHistory()
	pending = pending[:len(pending)-1] // not settled yet
	msgs, err := replayHistory(context.Background(), pending, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := toolResultTextFor(t, msgs, stagedCallID); !strings.HasPrefix(got, "APPROVAL_REQUIRED:") {
		t.Errorf("pending card replayed as %q, want its placeholder unchanged", got)
	}

	entries := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "go"}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "b1", Name: "bash", Input: `{"command":"ls"}`}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "b1", Name: "bash", Text: "ok"}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "p1", Name: "preview_email", Input: "{}"}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "p1", Name: "preview_email", Text: "PREVIEW_DISPLAYED", IsErr: true}),
		mustEntry("assistant", "text", TextContent{Text: "Previewed."}),
		mustEntry("assistant", "turn_summary", TurnSummaryContent{}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "p1", Name: "preview_email", Text: "Preview closed."}),
	}
	msgs, err = replayHistory(context.Background(), entries, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := toolResultTextFor(t, msgs, "b1"); got != "ok" {
		t.Errorf("ordinary result = %q, want it unchanged", got)
	}
	if got, _, _ := toolResultTextFor(t, msgs, "p1"); got != "Preview closed." {
		t.Errorf("non-approval late result = %q, want it unchanged", got)
	}
}
