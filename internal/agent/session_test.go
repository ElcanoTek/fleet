package agent

import (
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// TestReplayHistory_RoundTrip builds a plausible conversation history, replays
// it through replayHistory, and verifies the resulting fantasy.Message slice
// has the right shape (alternating user → assistant(text + tool_calls) → tool
// results).
func TestReplayHistory_RoundTrip(t *testing.T) {
	entries := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "find me the Amazon Ads reports"}),
		mustEntry("assistant", "reasoning", ReasoningContent{Text: "I should search S3."}),
		mustEntry("assistant", "tool_call", ToolCallContent{
			ID:    "call_1",
			Name:  "mcp_email_search_emails",
			Input: `{"sender_contains":"amazon"}`,
		}),
		mustEntry("tool", "tool_result", ToolResultContent{
			ID:    "call_1",
			Name:  "mcp_email_search_emails",
			Text:  `{"emails":[]}`,
			IsErr: false,
		}),
		mustEntry("assistant", "text", TextContent{Text: "I found zero hits."}),
		mustEntry("user", "text", TextContent{Text: "expand to last month"}),
	}

	msgs, err := replayHistory(entries, "")
	if err != nil {
		t.Fatalf("replayHistory: %v", err)
	}

	// Expected role sequence: user, assistant (tool_call), tool, assistant (text), user
	// Reasoning must be filtered out.
	if len(msgs) != 5 {
		t.Fatalf("message count: got %d", len(msgs))
	}
	wantRoles := []fantasy.MessageRole{
		fantasy.MessageRoleUser,
		fantasy.MessageRoleAssistant,
		fantasy.MessageRoleTool,
		fantasy.MessageRoleAssistant,
		fantasy.MessageRoleUser,
	}
	for i, m := range msgs {
		if m.Role != wantRoles[i] {
			t.Errorf("msg %d: role %q want %q", i, m.Role, wantRoles[i])
		}
	}

	// Assistant tool_call must carry a ToolCallPart with the right name.
	ac := msgs[1].Content
	var found bool
	for _, part := range ac {
		if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
			if tc.ToolName == "mcp_email_search_emails" && tc.ToolCallID == "call_1" {
				found = true
			}
		}
	}
	if !found {
		t.Error("tool call part not found on assistant msg")
	}
}

// TestReplayHistory_DropsReasoning — reasoning entries are UI-only and must
// not be sent back to the provider on the next turn.
func TestReplayHistory_DropsReasoning(t *testing.T) {
	entries := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "hi"}),
		mustEntry("assistant", "reasoning", ReasoningContent{Text: "internal thought"}),
		mustEntry("assistant", "text", TextContent{Text: "hello"}),
	}
	msgs, err := replayHistory(entries, "")
	if err != nil {
		t.Fatalf("replayHistory: %v", err)
	}

	// Serialize to JSON and make sure "internal thought" is nowhere to be seen.
	b, _ := json.Marshal(msgs)
	if contains(b, "internal thought") {
		t.Error("reasoning leaked into replay payload")
	}
}

// TestReplayHistory_ToolErrorPreserved — error tool results round-trip as
// ToolResultOutputContentError.
// TestReplayHistory_NormalizesNonObjectToolInput: a stored tool-call input the
// model streamed as a non-object (here an array) is replayed as a JSON object,
// so a resumed conversation does not 400 on every later turn
// ("tool_use.input: Input should be a valid dictionary"). The pairing with
// its result is kept.
func TestReplayHistory_NormalizesNonObjectToolInput(t *testing.T) {
	entries := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "run it"}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "c1", Name: "run_python", Input: `["print(1)"]`}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "c1", Name: "run_python", Text: "invalid arguments", IsErr: true}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "c2", Name: "bash", Input: `{"command":"ls"}`}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "c2", Name: "bash", Text: "ok"}),
	}
	msgs, err := replayHistory(entries, "")
	if err != nil {
		t.Fatalf("replayHistory: %v", err)
	}
	assertWellFormedToolPairs(t, msgs)
	inputs := map[string]string{}
	for _, m := range msgs {
		for _, part := range m.Content {
			if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				inputs[tc.ToolCallID] = tc.Input
			}
		}
	}
	if inputs["c1"] != "{}" {
		t.Errorf("non-object input replayed as %q, want {}", inputs["c1"])
	}
	if inputs["c2"] != `{"command":"ls"}` {
		t.Errorf("valid input changed to %q", inputs["c2"])
	}
}

func TestReplayHistory_ToolErrorPreserved(t *testing.T) {
	entries := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "run it"}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "c1", Name: "bash", Input: `{"command":"false"}`}),
		mustEntry("tool", "tool_result", ToolResultContent{
			ID:    "c1",
			Name:  "bash",
			Text:  "exit status 1",
			IsErr: true,
		}),
	}
	msgs, err := replayHistory(entries, "")
	if err != nil {
		t.Fatalf("replayHistory: %v", err)
	}
	toolMsg := msgs[len(msgs)-1]
	if toolMsg.Role != fantasy.MessageRoleTool {
		t.Fatalf("last role: %q", toolMsg.Role)
	}
	if len(toolMsg.Content) != 1 {
		t.Fatalf("tool content len: %d", len(toolMsg.Content))
	}
	trp, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](toolMsg.Content[0])
	if !ok {
		t.Fatal("not a ToolResultPart")
	}
	if _, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](trp.Output); !ok {
		t.Error("error output not preserved")
	}
}

// assertWellFormedToolPairs checks the provider-required invariant: every
// tool-role message's ToolResultPart references a tool_use that appears in a
// PRECEDING assistant message, and no tool_use is left without a result.
func assertWellFormedToolPairs(t *testing.T, msgs []fantasy.Message) {
	t.Helper()
	seen := map[string]bool{}
	resolved := map[string]bool{}
	for i, m := range msgs {
		for _, part := range m.Content {
			if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				seen[tc.ToolCallID] = true
			}
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if !seen[tr.ToolCallID] {
					t.Errorf("msg %d: tool_result %q has no preceding tool_use", i, tr.ToolCallID)
				}
				if resolved[tr.ToolCallID] {
					t.Errorf("msg %d: tool_result %q emitted more than once", i, tr.ToolCallID)
				}
				resolved[tr.ToolCallID] = true
			}
		}
	}
	for id := range seen {
		if !resolved[id] {
			t.Errorf("tool_use %q has no result in replay", id)
		}
	}
}

// toolResultTextFor returns the text of the (single) tool_result for id, and
// whether it was emitted as an error result.
func toolResultTextFor(t *testing.T, msgs []fantasy.Message, id string) (string, bool, bool) {
	t.Helper()
	for _, m := range msgs {
		for _, part := range m.Content {
			tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok || tr.ToolCallID != id {
				continue
			}
			if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output); ok {
				return txt.Text, false, true
			}
			if errv, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Output); ok {
				return errv.Error.Error(), true, true
			}
		}
	}
	return "", false, false
}

// TestReplayHistory_ApprovalResolutionOutOfOrder — the send_email approval
// resolution (real 202) is appended with the original tool_call id but lands
// BEFORE its own call because the user clicked Send before the staging turn
// was persisted. Replay must still pair exactly one (the real) result with
// the call, never emit a dangling result, and never leak the placeholder.
func TestReplayHistory_ApprovalResolutionOutOfOrder(t *testing.T) {
	entries := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "put it in an email"}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "p1", Name: "preview_email", Input: "{}"}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "p1", Name: "preview_email", Text: "PREVIEW_DISPLAYED", IsErr: true}),
		mustEntry("assistant", "text", TextContent{Text: "Staged a preview."}),
		// Resolution appended out of order — its tool_call is below.
		mustEntry("tool", "tool_result", ToolResultContent{ID: "s1", Name: "mcp_sendgrid_send_email", Text: `{"status_code":202}`, IsErr: false}),
		mustEntry("user", "text", TextContent{Text: "send to jeanne"}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "s1", Name: "mcp_sendgrid_send_email", Input: "{}"}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "s1", Name: "mcp_sendgrid_send_email", Text: "APPROVAL_REQUIRED: staged", IsErr: true}),
		mustEntry("assistant", "text", TextContent{Text: "I have staged the email."}),
	}
	msgs, err := replayHistory(entries, "")
	if err != nil {
		t.Fatalf("replayHistory: %v", err)
	}
	assertWellFormedToolPairs(t, msgs)
	txt, isErr, ok := toolResultTextFor(t, msgs, "s1")
	if !ok {
		t.Fatal("no tool_result emitted for the send call")
	}
	if isErr {
		t.Errorf("send result emitted as error; want the real 202 outcome (text=%q)", txt)
	}
	if !contains([]byte(txt), "202") {
		t.Errorf("send result text = %q, want the 202 outcome", txt)
	}
	if b, _ := json.Marshal(msgs); contains(b, "APPROVAL_REQUIRED") {
		t.Error("APPROVAL_REQUIRED placeholder leaked into replay")
	}
}

// TestReplayHistory_ApprovalResolutionAppendedAfter — non-race ordering: the
// resolution row is appended AFTER the inline placeholder (user clicked Send
// once the turn had finished). Still must collapse to one real result.
func TestReplayHistory_ApprovalResolutionAppendedAfter(t *testing.T) {
	entries := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "send it"}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "s1", Name: "mcp_sendgrid_send_email", Input: "{}"}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "s1", Name: "mcp_sendgrid_send_email", Text: "APPROVAL_REQUIRED: staged", IsErr: true}),
		mustEntry("assistant", "text", TextContent{Text: "Staged."}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "s1", Name: "mcp_sendgrid_send_email", Text: `{"status_code":202}`, IsErr: false}),
	}
	msgs, err := replayHistory(entries, "")
	if err != nil {
		t.Fatalf("replayHistory: %v", err)
	}
	assertWellFormedToolPairs(t, msgs)
	txt, isErr, ok := toolResultTextFor(t, msgs, "s1")
	if !ok || isErr || !contains([]byte(txt), "202") {
		t.Errorf("send result = (%q, isErr=%v, ok=%v); want the single 202 outcome", txt, isErr, ok)
	}
}

// TestReplayHistory_DanglingResultDropped — a tool_result whose id never
// appears as a tool_call must be dropped (a tool_result with no preceding
// tool_use is rejected by every provider).
func TestReplayHistory_DanglingResultDropped(t *testing.T) {
	entries := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "hi"}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "ghost", Name: "bash", Text: "orphan", IsErr: false}),
		mustEntry("assistant", "text", TextContent{Text: "ok"}),
	}
	msgs, err := replayHistory(entries, "")
	if err != nil {
		t.Fatalf("replayHistory: %v", err)
	}
	assertWellFormedToolPairs(t, msgs)
	for _, m := range msgs {
		if m.Role == fantasy.MessageRoleTool {
			t.Error("dangling tool_result was emitted")
		}
	}
}

// TestReplayHistory_SummaryReplacesPriorTurns — the latest "summary"
// entry is the boundary: every entry before it is dropped from the
// LLM context and the summary itself is emitted as a single
// synthetic assistant message. Pre-summary tool calls / user prompts
// must NOT leak into the replay payload.
func TestReplayHistory_SummaryReplacesPriorTurns(t *testing.T) {
	entries := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "first user message — pre-summary"}),
		mustEntry("assistant", "text", TextContent{Text: "first assistant reply"}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "x1", Name: "bash", Input: `{"command":"ls"}`}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "x1", Name: "bash", Text: "secret-file"}),
		mustEntry("assistant", "summary", SummaryContent{Text: "User asked X. We did Y. Open: Z.", Model: "anthropic/claude-sonnet-4.6"}),
		mustEntry("user", "text", TextContent{Text: "post-summary user follow-up"}),
	}

	msgs, err := replayHistory(entries, "")
	if err != nil {
		t.Fatalf("replayHistory: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("expected exactly 2 messages (summary + post-summary user), got %d", len(msgs))
	}
	if msgs[0].Role != fantasy.MessageRoleAssistant {
		t.Errorf("msg 0: want assistant role for summary, got %q", msgs[0].Role)
	}
	if msgs[1].Role != fantasy.MessageRoleUser {
		t.Errorf("msg 1: want user role for follow-up, got %q", msgs[1].Role)
	}

	payload, _ := json.Marshal(msgs)
	if contains(payload, "first user message — pre-summary") {
		t.Error("pre-summary user message leaked into replay payload")
	}
	if contains(payload, "first assistant reply") {
		t.Error("pre-summary assistant reply leaked into replay payload")
	}
	if contains(payload, "secret-file") {
		t.Error("pre-summary tool result leaked into replay payload")
	}
	if !contains(payload, "User asked X.") {
		t.Error("summary text missing from replay payload")
	}
	if !contains(payload, "post-summary user follow-up") {
		t.Error("post-summary user follow-up missing from replay payload")
	}
}

// TestReplayHistory_LatestSummaryWins — when the entry list contains
// multiple summaries (e.g. a user re-summarized after the storage
// layer's replace logic was bypassed in tests, or future schemas), only
// the latest one is the boundary.
func TestReplayHistory_LatestSummaryWins(t *testing.T) {
	entries := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "session 1 first turn"}),
		mustEntry("assistant", "summary", SummaryContent{Text: "old summary"}),
		mustEntry("user", "text", TextContent{Text: "session 2 first turn"}),
		mustEntry("assistant", "text", TextContent{Text: "session 2 reply"}),
		mustEntry("assistant", "summary", SummaryContent{Text: "new summary"}),
		mustEntry("user", "text", TextContent{Text: "session 3 follow-up"}),
	}
	msgs, err := replayHistory(entries, "")
	if err != nil {
		t.Fatalf("replayHistory: %v", err)
	}
	payload, _ := json.Marshal(msgs)
	if contains(payload, "old summary") {
		t.Error("older summary should not appear in replay")
	}
	if !contains(payload, "new summary") {
		t.Error("latest summary should anchor replay")
	}
	if contains(payload, "session 2 reply") {
		t.Error("turn between two summaries should not appear in replay")
	}
}

// TestReplayHistory_BlankSummaryIsNoOp — a malformed/blank summary
// should not produce an empty assistant message that would confuse
// the next provider call.
func TestReplayHistory_BlankSummaryIsNoOp(t *testing.T) {
	entries := []HistoryEntry{
		mustEntry("assistant", "summary", SummaryContent{Text: "   "}),
		mustEntry("user", "text", TextContent{Text: "after blank summary"}),
	}
	msgs, err := replayHistory(entries, "")
	if err != nil {
		t.Fatalf("replayHistory: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("blank summary should yield only the user follow-up, got %d", len(msgs))
	}
	if msgs[0].Role != fantasy.MessageRoleUser {
		t.Errorf("got role %q want user", msgs[0].Role)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("short: got %q", got)
	}
	if got := truncate("abcdefghij", 4); got != "abcd…[truncated]" {
		t.Errorf("long: got %q", got)
	}
}

func TestReplayHistory_AppliesPersistedTurnCompactions(t *testing.T) {
	summaryMsg := "[context compaction — earlier messages summarized to fit context window]\n\nCondensed middle."
	entries := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "turn 1 head", RuntimeToday: "2026-10-26"}),
		mustEntry("assistant", "text", TextContent{Text: "turn 1 reply — should be compacted away"}),
		mustEntry("user", "text", TextContent{Text: "turn 2 question — should be compacted away"}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "c2", Name: "bash", Input: `{"command":"pwd"}`}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "c2", Name: "bash", Text: "/workspace"}),
		mustEntry("assistant", "text", TextContent{Text: "turn 2 reply — kept in tail"}),
		mustEntry("user", "text", TextContent{Text: "turn 3 question — triggers compaction"}),
		mustEntry("assistant", "text", TextContent{Text: "turn 3 answer after compaction"}),
		mustEntry("assistant", "turn_summary", TurnSummaryContent{
			Model:                "anthropic/claude-opus-4.7",
			PromptTokensLastStep: 42_000,
			Compactions: []agentcore.CompactionRecord{
				{
					Head:    2, // user + runtime_today note
					Removed: 4, // turn 1 reply, turn 2 question, tool_call c2, tool_result c2
					Summary: summaryMsg,
				},
			},
		}),
		mustEntry("user", "text", TextContent{Text: "turn 4 follow-up"}),
	}

	msgs, err := replayHistory(entries, "")
	if err != nil {
		t.Fatalf("replayHistory: %v", err)
	}
	assertWellFormedToolPairs(t, msgs)

	payload, _ := json.Marshal(msgs)
	if !contains(payload, "turn 1 head") {
		t.Error("pinned head message should survive compaction replay")
	}
	if !contains(payload, "Condensed middle.") {
		t.Error("persisted compaction summary should appear in replayed history")
	}
	if contains(payload, "turn 1 reply — should be compacted away") || contains(payload, "turn 2 question — should be compacted away") {
		t.Error("messages in the compacted middle should not re-enter replayed history on turn 4")
	}
	if !contains(payload, "turn 2 reply — kept in tail") || !contains(payload, "turn 3 answer after compaction") || !contains(payload, "turn 4 follow-up") {
		t.Error("kept tail and subsequent turns should remain in replayed history")
	}
}

func TestReplayHistory_PreservesApprovalOutcomesAcrossCompaction(t *testing.T) {
	// 1. Post-compaction approval resolution: a staged call in Turn 1 is
	// compacted away at Turn 2's turn_summary, and later settled. The resolved
	// exchange must survive at its chronological tail position in Role=assistant
	// + Role=tool (never Role=user).
	postCompactionApprovalEntries := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "turn 1 head", RuntimeToday: "2026-10-26"}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "call_compacted", Name: "send_email", Input: `{}`}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "call_compacted", Name: "send_email", Text: "APPROVAL_REQUIRED: staged card", IsErr: true}),
		mustEntry("assistant", "text", TextContent{Text: "Staged."}),
		mustEntry("user", "text", TextContent{Text: "turn 2 kept"}),
		mustEntry("assistant", "text", TextContent{Text: "turn 2 reply"}),
		mustEntry("assistant", "turn_summary", TurnSummaryContent{
			Model: "anthropic/claude-opus-4.7",
			Compactions: []agentcore.CompactionRecord{
				{
					Head:    2,
					Removed: 3, // call_compacted + result + "Staged."
					Summary: "[context compaction — earlier messages summarized to fit context window]\n\nSummary.",
				},
			},
		}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "call_compacted", Name: "send_email", Text: `{"status":"sent"}`, IsErr: false}),
	}
	msgs, err := replayHistory(postCompactionApprovalEntries, "")
	if err != nil {
		t.Fatalf("replayHistory post-compaction approval: %v", err)
	}
	assertWellFormedToolPairs(t, msgs)
	txt, isErr, ok := toolResultTextFor(t, msgs, "call_compacted")
	if !ok || isErr || !contains([]byte(txt), `"status":"sent"`) {
		t.Errorf("post-compaction approval result = (%q, isErr=%v, ok=%v); want sent outcome in Role=tool", txt, isErr, ok)
	}
	if msgs[len(msgs)-1].Role != fantasy.MessageRoleTool {
		t.Errorf("tail message role = %q, want tool (never user)", msgs[len(msgs)-1].Role)
	}

	// 2. Out-of-order approval resolution on a turn that itself compacts the
	// call: the resolution row lands before the tool_call, and the turn's
	// turn_summary compacts the middle. The resolved exchange must still
	// survive in Role=assistant + Role=tool.
	oooCompactedEntries := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "turn 1 head", RuntimeToday: "2026-10-26"}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "call_ooo", Name: "send_email", Text: `{"status":"sent_ooo"}`, IsErr: false}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "call_ooo", Name: "send_email", Input: `{}`}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "call_ooo", Name: "send_email", Text: "APPROVAL_REQUIRED: staged card", IsErr: true}),
		mustEntry("assistant", "text", TextContent{Text: "Staged."}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "call_tail", Name: "bash", Input: `{"command":"true"}`}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "call_tail", Name: "bash", Text: "ok", IsErr: false}),
		mustEntry("assistant", "text", TextContent{Text: "Done."}),
		mustEntry("assistant", "turn_summary", TurnSummaryContent{
			Model: "anthropic/claude-opus-4.7",
			Compactions: []agentcore.CompactionRecord{
				{
					Head:    2,
					Removed: 3, // call_ooo + result + "Staged."
					Summary: "[context compaction — earlier messages summarized to fit context window]\n\nSummary.",
				},
			},
		}),
	}
	msgsOOO, err := replayHistory(oooCompactedEntries, "")
	if err != nil {
		t.Fatalf("replayHistory out-of-order compacted: %v", err)
	}
	assertWellFormedToolPairs(t, msgsOOO)
	txtOOO, isErrOOO, okOOO := toolResultTextFor(t, msgsOOO, "call_ooo")
	if !okOOO || isErrOOO || !contains([]byte(txtOOO), `"status":"sent_ooo"`) {
		t.Errorf("out-of-order compacted result = (%q, isErr=%v, ok=%v); want sent_ooo outcome preserved", txtOOO, isErrOOO, okOOO)
	}
}

// contains is a minimal substring check on []byte — avoids pulling in bytes
// just for this file.
func contains(haystack []byte, needle string) bool {
	nb := []byte(needle)
	for i := 0; i+len(nb) <= len(haystack); i++ {
		if string(haystack[i:i+len(nb)]) == needle {
			return true
		}
	}
	return false
}
