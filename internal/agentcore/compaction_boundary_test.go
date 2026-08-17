package agentcore

// Compaction tool-exchange boundary tests (#1106). Both compaction paths cut
// the history at computed indices — forceCompactMessageHistory at a fixed
// N-from-the-end tail boundary, proactiveCompact at the active midpoint — and
// replayed interactive history / multi-round scheduled carries contain
// assistant messages holding ToolCallParts followed by separate tool-role
// result messages. A raw cut landing between them produces a history whose
// kept slice begins with orphaned tool results (or whose summarized middle
// swallows the call but not the results), which providers reject with a 400
// that classifyStreamError treats as fatal — killing the very run compaction
// was rescuing. These tests pin the snap-to-safe-boundary behavior for single
// calls, parallel multi-call exchanges, and back-to-back exchanges, plus a
// deterministic sweep asserting every compacted output stays provider-valid.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// assistantToolCalls builds an assistant message issuing n parallel tool
// calls with IDs idPrefix0..idPrefix{n-1}.
func assistantToolCalls(idPrefix string, n int) fantasy.Message {
	parts := make([]fantasy.MessagePart, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, fantasy.ToolCallPart{
			ToolCallID: fmt.Sprintf("%s%d", idPrefix, i),
			ToolName:   "bash",
			Input:      `{"command":"ls"}`,
		})
	}
	return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: parts}
}

// toolResults builds one tool-role result message per call ID — results for a
// parallel multi-call assistant message span several CONSECUTIVE tool
// messages, which is exactly the shape a raw cut index can land inside.
func toolResults(idPrefix string, n int) []fantasy.Message {
	msgs := make([]fantasy.Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{
				ToolCallID: fmt.Sprintf("%s%d", idPrefix, i),
				Output:     fantasy.ToolResultOutputContentText{Text: "ok"},
			},
		}})
	}
	return msgs
}

// toolExchange is one complete exchange: an assistant message with n parallel
// calls followed by its n tool-role results.
func toolExchange(idPrefix string, n int) []fantasy.Message {
	return append([]fantasy.Message{assistantToolCalls(idPrefix, n)}, toolResults(idPrefix, n)...)
}

// providerValidityViolation walks msgs with the provider's pairing rule and
// returns a description of the first violation, or "" for a valid sequence:
// every tool-role result must answer a call issued by the immediately
// preceding assistant message, every issued call must be answered before any
// non-tool message follows, and no call may be left unanswered at the end.
// This is the acceptance rule compaction output must never break.
func providerValidityViolation(msgs []fantasy.Message) string {
	pending := map[string]bool{}
	for i, m := range msgs {
		if m.Role == fantasy.MessageRoleTool {
			for _, part := range m.Content {
				p, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
				if !ok {
					continue
				}
				if !pending[p.ToolCallID] {
					return fmt.Sprintf("message %d: tool result %q answers no pending tool call (orphaned result)", i, p.ToolCallID)
				}
				delete(pending, p.ToolCallID)
			}
			continue
		}
		if len(pending) > 0 {
			return fmt.Sprintf("message %d (role %s) arrived while %d tool call(s) are unanswered (dangling call)", i, m.Role, len(pending))
		}
		if m.Role == fantasy.MessageRoleAssistant {
			for _, part := range m.Content {
				if p, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
					pending[p.ToolCallID] = true
				}
			}
		}
	}
	if len(pending) > 0 {
		return fmt.Sprintf("history ends with %d unanswered tool call(s)", len(pending))
	}
	return ""
}

func assertProviderValid(t *testing.T, label string, msgs []fantasy.Message) {
	t.Helper()
	if v := providerValidityViolation(msgs); v != "" {
		t.Errorf("%s: provider-invalid sequence: %s", label, v)
	}
}

// countToolParts tallies the ToolCallParts and ToolResultParts present in
// msgs, so a test can assert an exchange survived (or was dropped) whole.
func countToolParts(msgs []fantasy.Message) (calls, results int) {
	for _, m := range msgs {
		for _, part := range m.Content {
			if _, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				calls++
			}
			if _, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				results++
			}
		}
	}
	return calls, results
}

// ── forceCompactMessageHistory boundary snapping ──

// The raw len-compactionKeepTail boundary lands exactly on a single call's
// tool result: the tail must EXPAND backward to include the paired assistant
// call rather than start with the orphaned result.
func TestForceCompact_SnapsTailPastSingleToolExchange(t *testing.T) {
	e := newMockEngine(t, &mockModel{})
	// HEAD(0) + 9 fillers (1-9) + assistant c0 (10) + result (11) + 19 fillers
	// (12-30). len = 31 → raw tail boundary = 31-20 = 11: the tool result.
	msgs := make([]fantasy.Message, 0, 64)
	msgs = append(msgs, fantasy.NewUserMessage("HEAD"))
	msgs = append(msgs, fillerMessages(9, 8)...)
	msgs = append(msgs, toolExchange("c", 1)...)
	msgs = append(msgs, fillerMessages(19, 8)...)

	out := e.forceCompactMessageHistory(context.Background(), msgs)

	assertProviderValid(t, "force-compacted output", out)
	if len(out) >= len(msgs) {
		t.Errorf("compaction made no progress: len %d -> %d", len(msgs), len(out))
	}
	if got := msgText(out[0]); got != "HEAD" {
		t.Errorf("head not preserved: %q", got)
	}
	if !strings.Contains(msgText(out[1]), compactionSummaryPrefix) {
		t.Errorf("summary marker missing: %q", msgText(out[1]))
	}
	// The exchange straddling the raw boundary must survive whole in the tail.
	calls, results := countToolParts(out)
	if calls != 1 || results != 1 {
		t.Errorf("tool exchange split: kept %d calls / %d results, want 1/1", calls, results)
	}
}

// A parallel multi-call assistant message whose results span several
// consecutive tool messages, with the raw boundary landing in the MIDDLE of
// the result block: backward snapping must retreat past the whole block to
// the assistant message.
func TestForceCompact_SnapsTailPastParallelCallResults(t *testing.T) {
	e := newMockEngine(t, &mockModel{})
	// HEAD(0) + 8 fillers (1-8) + assistant c0..c2 (9) + results (10-12) + 18
	// fillers (13-30). len = 31 → raw tail boundary 11: the SECOND of three
	// results.
	msgs := make([]fantasy.Message, 0, 64)
	msgs = append(msgs, fantasy.NewUserMessage("HEAD"))
	msgs = append(msgs, fillerMessages(8, 8)...)
	msgs = append(msgs, toolExchange("c", 3)...)
	msgs = append(msgs, fillerMessages(18, 8)...)

	out := e.forceCompactMessageHistory(context.Background(), msgs)

	assertProviderValid(t, "force-compacted output", out)
	calls, results := countToolParts(out)
	if calls != 3 || results != 3 {
		t.Errorf("parallel exchange split: kept %d calls / %d results, want 3/3", calls, results)
	}
}

// Back-to-back exchanges with the raw boundary inside the SECOND one: the
// snap must stop at the second exchange's assistant message (a safe cut — the
// first exchange lands whole in the summarized middle), not retreat further.
func TestForceCompact_BackToBackExchangesSplitBetween(t *testing.T) {
	e := newMockEngine(t, &mockModel{})
	// HEAD(0) + 6 fillers (1-6) + exchange a (assistant 7, result 8) +
	// exchange b (assistant 9, results 10-11) + 19 fillers (12-30). len = 31 →
	// raw tail boundary 11: exchange b's second result.
	msgs := make([]fantasy.Message, 0, 64)
	msgs = append(msgs, fantasy.NewUserMessage("HEAD"))
	msgs = append(msgs, fillerMessages(6, 8)...)
	msgs = append(msgs, toolExchange("a", 1)...)
	msgs = append(msgs, toolExchange("b", 2)...)
	msgs = append(msgs, fillerMessages(19, 8)...)

	out := e.forceCompactMessageHistory(context.Background(), msgs)

	assertProviderValid(t, "force-compacted output", out)
	// Exchange b survives whole; exchange a is summarized away whole — neither
	// half of either exchange may appear without its partner.
	calls, results := countToolParts(out)
	if calls != 2 || results != 2 {
		t.Errorf("kept %d calls / %d results, want exactly exchange b (2/2)", calls, results)
	}
}

// Degenerate: everything between the head and the raw boundary is one giant
// tool exchange, so backward snapping would consume the whole middle. The cut
// falls FORWARD instead — the exchange is summarized away whole and the run
// still gets its pressure relief.
func TestForceCompact_FallsForwardWhenBackwardSnapReachesHead(t *testing.T) {
	e := newMockEngine(t, &mockModel{})
	// HEAD(0) + assistant with 25 calls (1) + 25 results (2-26) + 5 fillers
	// (27-31). len = 32 → raw tail boundary 12, deep inside the result block;
	// backward snapping reaches index 1 <= keepHead.
	msgs := make([]fantasy.Message, 0, 64)
	msgs = append(msgs, fantasy.NewUserMessage("HEAD"))
	msgs = append(msgs, toolExchange("c", 25)...)
	msgs = append(msgs, fillerMessages(5, 8)...)

	out := e.forceCompactMessageHistory(context.Background(), msgs)

	assertProviderValid(t, "force-compacted output", out)
	if len(out) >= len(msgs) {
		t.Errorf("compaction made no progress: len %d -> %d", len(msgs), len(out))
	}
	calls, results := countToolParts(out)
	if calls != 0 || results != 0 {
		t.Errorf("the unsplittable exchange must be summarized away whole, kept %d calls / %d results", calls, results)
	}
}

// ── proactiveCompact boundary snapping ──

// The raw midpoint lands on a parallel exchange's first result: the cut snaps
// back to the assistant message so the whole exchange stays in the kept half.
func TestProactiveCompact_SnapsMidpointPastToolExchange(t *testing.T) {
	e := newMockEngine(t, &mockModel{})
	// HEAD + active: 3 fillers, assistant c0..c1, 2 results, 3 fillers →
	// len(active) = 9, raw midpoint 4 = the first tool result.
	msgs := make([]fantasy.Message, 0, 64)
	msgs = append(msgs, fantasy.NewUserMessage("HEAD"))
	msgs = append(msgs, fillerMessages(3, 8)...)
	msgs = append(msgs, toolExchange("c", 2)...)
	msgs = append(msgs, fillerMessages(3, 8)...)

	res := e.proactiveCompact(context.Background(), msgs)

	if !res.compacted {
		t.Fatal("expected compaction")
	}
	assertProviderValid(t, "proactively compacted output", res.messages)
	// Snapping back to the assistant leaves exactly the 3 leading fillers
	// droppable.
	if res.removedTurns != 3 {
		t.Errorf("removedTurns = %d, want 3", res.removedTurns)
	}
	calls, results := countToolParts(res.messages)
	if calls != 2 || results != 2 {
		t.Errorf("tool exchange split: kept %d calls / %d results, want 2/2", calls, results)
	}
}

// Degenerate: the active history is one exchange with nothing droppable on
// either side of a safe cut — backward snapping empties the droppable half
// and forward snapping empties the kept half, so compaction refuses rather
// than emit an invalid (or empty) history.
func TestProactiveCompact_NoSafeSplitIsNoop(t *testing.T) {
	e := newMockEngine(t, &mockModel{})
	// HEAD + assistant c0..c1 + 2 results → active = [assistant, tool, tool],
	// raw midpoint 1 (a result).
	msgs := make([]fantasy.Message, 0, 64)
	msgs = append(msgs, fantasy.NewUserMessage("HEAD"))
	msgs = append(msgs, toolExchange("c", 2)...)

	res := e.proactiveCompact(context.Background(), msgs)

	if res.compacted {
		t.Error("expected no compaction when no safe split makes progress")
	}
	if len(res.messages) != len(msgs) {
		t.Errorf("messages should be unchanged, got len %d want %d", len(res.messages), len(msgs))
	}
	assertProviderValid(t, "untouched history", res.messages)
}

// ── sweep: no shape × cut point may ever produce an invalid sequence ──

// buildMixedHistory deterministically interleaves plain user turns with
// single-call, parallel, and back-to-back tool exchanges. Growing n sweeps
// the raw cut indices of both compaction paths across every phase of an
// exchange (assistant, first/middle/last result, between exchanges).
func buildMixedHistory(n int) []fantasy.Message {
	msgs := make([]fantasy.Message, 0, 64)
	msgs = append(msgs, fantasy.NewUserMessage("HEAD"))
	for i := 0; len(msgs) < n; i++ {
		switch i % 6 {
		case 0, 3:
			msgs = append(msgs, fantasy.NewUserMessage(fmt.Sprintf("filler %d", i)))
		case 1:
			msgs = append(msgs, toolExchange(fmt.Sprintf("s%d-", i), 1)...)
		case 2:
			msgs = append(msgs, toolExchange(fmt.Sprintf("s%d-", i), 3)...)
		case 4: // back-to-back with case 5's exchange
			msgs = append(msgs, toolExchange(fmt.Sprintf("s%d-", i), 2)...)
		case 5:
			msgs = append(msgs, toolExchange(fmt.Sprintf("s%d-", i), 1)...)
		}
	}
	return msgs
}

func TestCompaction_SweepNeverSplitsToolExchange(t *testing.T) {
	for n := 3; n <= 64; n++ {
		msgs := buildMixedHistory(n)
		if v := providerValidityViolation(msgs); v != "" {
			t.Fatalf("n=%d: test fixture itself invalid: %s", n, v)
		}

		e := newMockEngine(t, &mockModel{})
		out := e.forceCompactMessageHistory(context.Background(), msgs)
		assertProviderValid(t, fmt.Sprintf("n=%d force-compacted", n), out)
		if len(out) > len(msgs) {
			t.Errorf("n=%d: force compaction grew the history: %d -> %d", n, len(msgs), len(out))
		}
		if res := e.proactiveCompact(context.Background(), msgs); res.compacted {
			assertProviderValid(t, fmt.Sprintf("n=%d proactively compacted", n), res.messages)
			if res.removedTurns < 1 {
				t.Errorf("n=%d: compacted without removing anything", n)
			}
		} else {
			assertProviderValid(t, fmt.Sprintf("n=%d proactive noop", n), res.messages)
		}
	}
}
