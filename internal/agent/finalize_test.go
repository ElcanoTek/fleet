package agent

import (
	"testing"

	"charm.land/fantasy"
)

func TestBuildForceSummaryMessages(t *testing.T) {
	prior := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "older question"}),
		mustEntry("assistant", "text", TextContent{Text: "older answer"}),
	}
	turn := []HistoryEntry{
		mustEntry("user", "text", TextContent{Text: "pull the report"}),
		mustEntry("assistant", "tool_call", ToolCallContent{ID: "c1", Name: "run_python", Input: "{}"}),
		mustEntry("tool", "tool_result", ToolResultContent{ID: "c1", Name: "run_python", Text: `{"output":"spend=123"}`}),
		// No assistant text — this is exactly the "stopped without answering" case.
	}

	msgs, err := buildForceSummaryMessages(prior, turn)
	if err != nil {
		t.Fatalf("buildForceSummaryMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("expected messages")
	}

	// Must end with a user turn carrying the forcing nudge so the follow-up
	// call knows to write the answer.
	last := msgs[len(msgs)-1]
	if last.Role != fantasy.MessageRoleUser {
		t.Errorf("last message role = %q, want user", last.Role)
	}
	if !messageContainsText(last, forceFinalSummaryNudge) {
		t.Errorf("last message does not carry the forcing nudge: %+v", last)
	}

	// The tool work the model already did must be replayed (so it has the
	// results to summarize) and well-formed (call before result).
	assertWellFormedToolPairs(t, msgs)
	if got, _, ok := toolResultTextFor(t, msgs, "c1"); !ok || !contains([]byte(got), "spend=123") {
		t.Errorf("tool result for c1 missing/empty in replayed convo: got=%q ok=%v", got, ok)
	}
}

func messageContainsText(m fantasy.Message, want string) bool {
	for _, part := range m.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && tp.Text == want {
			return true
		}
	}
	return false
}

func TestStripLeakedToolCalls(t *testing.T) {
	// The exact leak observed in the wild — a download_url call narrated as
	// text. Should collapse to empty so the forced-summary fallback fires.
	leak := "call:default_api:download_url{output_dir:/opt/chat/workspace/abc,url:https://api.fast.io/x/read/?token=eyJ0eXAiabc._sig}"
	if got := stripLeakedToolCalls(leak); got != "" {
		t.Errorf("leaked-only reply: got %q, want empty", got)
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain prose unchanged", "Here is your report. Spend rose 12% WoW.", "Here is your report. Spend rose 12% WoW."},
		{"prose mentioning call: but not a leak", "I'll call: the publisher to confirm.", "I'll call: the publisher to confirm."},
		{
			"real answer with a stray leaked call inline",
			"Done — see the table.\ncall:default_api:download_url{url:https://x/y}\nLet me know if you need more.",
			"Done — see the table.\n\nLet me know if you need more.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripLeakedToolCalls(c.in); got != c.want {
				t.Errorf("stripLeakedToolCalls(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestStepCap(t *testing.T) {
	if stepCap(0) != nil {
		t.Error("stepCap(0) should be nil (no cap)")
	}
	if stepCap(-5) != nil {
		t.Error("stepCap(negative) should be nil (no cap)")
	}
	if got := stepCap(100); len(got) != 1 {
		t.Fatalf("stepCap(100): got %d conditions, want exactly 1", len(got))
	}
}
