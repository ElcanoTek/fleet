package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// runMockTurn emits a deterministic, LLM-free SSE stream so Playwright and
// CI can exercise the full frontend state machine (reasoning block → tool
// chip → python output → assistant text → pin/delete) without burning
// OpenRouter credits or depending on provider latency.
//
// The script is intentionally simple: one reasoning block, one run_python
// tool call whose result is echoed back in the assistant message. Every
// user prompt gets the same canned response, which is exactly what the
// e2e tests assert on.
func runMockTurn(ctx context.Context, st chatStore, conv *store.Conversation, userMessage string, sink agent.EventSink) error {
	sink.Emit("turn.started", map[string]any{"persona": conv.Persona})

	// Test-only shortcut: when the user prompt contains "send email" we
	// stage a fake sendgrid approval so Playwright can exercise the
	// approval card + /approvals endpoint without a real SendGrid key.
	if shouldStageMockApproval(userMessage) {
		// Give the mock card a fixed 5-minute default-deny window (#225) so the
		// mocked Playwright lane can assert the approval countdown renders.
		expiresAt := time.Now().Add(5 * time.Minute).Unix()
		approval, err := st.CreateApproval(ctx, conv.ID, conv.UserEmail,
			"mcp_sendgrid_send_email", "mock-tool-call",
			`{"to_email":"demo@example.com","subject":"Mock subject","content":"<p>Hi from the mock turn.</p>"}`, expiresAt)
		if err == nil {
			sink.Emit("tool.approval_required", map[string]any{
				"approval_id": approval.ID,
				"tool":        "mcp_sendgrid_send_email",
				"summary": map[string]any{
					"tool":    "mcp_sendgrid_send_email",
					"to":      "demo@example.com",
					"subject": "Mock subject",
					"preview": "<p>Hi from the mock turn.</p>",
				},
				"expires_at": approval.ExpiresAt,
			})
		}
	}

	// A tiny "thinking" block.
	sink.Emit("reasoning.start", map[string]any{"id": "r1"})
	for _, chunk := range []string{"Let me ", "think about this ", "for a moment."} {
		sink.Emit("reasoning.delta", map[string]any{"id": "r1", "text": chunk})
		sleep(ctx, 5*time.Millisecond)
	}
	sink.Emit("reasoning.end", map[string]any{"id": "r1", "text": "Let me think about this for a moment."})

	// Fake run_python tool call + result — exercises the tool-chip UI and
	// the python-stdout monospace block.
	sink.Emit("tool.call", map[string]any{
		"id":    "call_mock_1",
		"name":  "run_python",
		"input": `{"code":"print('mock result')"}`,
	})
	sleep(ctx, 20*time.Millisecond)
	sink.Emit("tool.result", map[string]any{
		"id":     "call_mock_1",
		"name":   "run_python",
		"text":   `{"status":"success","output":"mock result","stdout":"mock result\n","stderr":"","vars":{}}`,
		"is_err": false,
	})

	// Final text — skipped when the prompt asks us to simulate the "model
	// stopped after tool calls without writing a reply" failure mode, so the
	// e2e suite can assert the empty-reply UI fallback. The real server forces
	// a summary in that case; mock mode short-circuits RunTurn so it can't, but
	// the UI fallback (and any older persisted empty turn) is what we cover here.
	reply := buildMockReply(userMessage)
	emptyReply := shouldMockEmptyReply(userMessage)
	if !emptyReply {
		for _, chunk := range splitForStream(reply) {
			sink.Emit("text.delta", map[string]any{"text": chunk})
			sleep(ctx, 10*time.Millisecond)
		}
	}

	sink.Emit("turn.completed", map[string]any{
		"cost_usd":          0.0,
		"prompt_tokens":     0,
		"completion_tokens": 0,
		"cached_tokens":     0,
		"model":             "mock/chat-mock",
	})

	// Persist the user turn + assistant reply so history replay + pin/reload
	// round-trips look the same as a real turn.
	entries := []agent.HistoryEntry{
		mustJSONEntry("user", "text", map[string]any{"text": userMessage}),
		mustJSONEntry("assistant", "reasoning", map[string]any{"text": "Let me think about this for a moment."}),
		mustJSONEntry("assistant", "tool_call", map[string]any{
			"id":    "call_mock_1",
			"name":  "run_python",
			"input": `{"code":"print('mock result')"}`,
		}),
		mustJSONEntry("tool", "tool_result", map[string]any{
			"id":     "call_mock_1",
			"name":   "run_python",
			"text":   `{"status":"success","output":"mock result","stdout":"mock result\n","stderr":"","vars":{}}`,
			"is_err": false,
		}),
	}
	if !emptyReply {
		entries = append(entries, mustJSONEntry("assistant", "text", map[string]any{"text": reply}))
	}
	return st.AppendHistory(ctx, conv.ID, entries)
}

// shouldMockEmptyReply returns true when the prompt asks the mock turn to end
// after its tool calls without any assistant text — the "agent stopped without
// an answer" failure mode the empty-reply UI fallback guards against.
func shouldMockEmptyReply(userMessage string) bool {
	return strings.Contains(strings.ToLower(userMessage), "simulate empty reply")
}

// shouldStageMockApproval returns true when the prompt contains the phrase
// "send email" (case-insensitive). Used only in mock mode so the e2e
// suite can cover the approval card without real credentials.
func shouldStageMockApproval(userMessage string) bool {
	return strings.Contains(strings.ToLower(userMessage), "send email")
}

func buildMockReply(userMessage string) string {
	u := strings.TrimSpace(userMessage)
	if u == "" {
		return "Mock reply."
	}
	// Keep the reply deterministic and easy to assert on in Playwright.
	return "Mock reply to: " + u
}

// splitForStream chops the reply into small chunks so the SSE stream
// resembles real token-by-token delivery.
func splitForStream(s string) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{s}
	}
	out := make([]string, 0, len(words))
	for i, w := range words {
		if i == 0 {
			out = append(out, w)
		} else {
			out = append(out, " "+w)
		}
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func mustJSONEntry(role, typ string, content any) agent.HistoryEntry {
	b, err := json.Marshal(content)
	if err != nil {
		panic(err)
	}
	return agent.HistoryEntry{Role: role, Type: typ, Content: b}
}
