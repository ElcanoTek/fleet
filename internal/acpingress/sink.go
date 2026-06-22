package acpingress

import (
	"context"

	acp "github.com/coder/acp-go-sdk"
)

// ingressSink adapts the governed turn's agent.EventSink (the SAME sink the web
// path drives, which forwards agentcore's stream-bridge events) to OUTBOUND ACP
// session/update notifications. It is the ingress Observer surface: the run's
// user-visible streaming is mirrored to the editor as it arrives.
//
// The event vocabulary is the agentcore stream-bridge's SSE event names
// (text.delta / reasoning.delta / tool.call / tool.result), so this mapping
// stays in lockstep with what the web path renders — ingress shows the editor
// the SAME turn the browser would show, chunk for chunk.
//
// The persisted transcript is NOT rebuilt here — the engine's TurnResult.NewHistory
// (the SAME the web path persists, including the assistant text entry) is the
// authoritative record the IngressAgent writes to the store. This sink is purely
// the outbound streaming mirror.
type ingressSink struct {
	conn      *acp.AgentSideConnection
	sessionID acp.SessionId
}

// newIngressSink builds a sink that streams onto conn for the given session.
func newIngressSink(conn *acp.AgentSideConnection, sessionID acp.SessionId) *ingressSink {
	return &ingressSink{conn: conn, sessionID: sessionID}
}

// Emit maps one run event to an ACP session/update. Per-event flush keeps the
// editor's stream live (no batching). A SessionUpdate error is non-fatal to the
// turn — the run continues and the final reply is still persisted — but it is
// not swallowed silently in production: the SDK logs send failures on its own
// logger, and the accumulated final text is the authoritative reply.
func (s *ingressSink) Emit(event string, payload any) {
	m, _ := payload.(map[string]any)
	ctx := context.Background()

	switch event {
	case "text.delta", "text":
		// Assistant message text → agent_message_chunk.
		if text, _ := m["text"].(string); text != "" {
			_ = s.conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: s.sessionID,
				Update:    acp.UpdateAgentMessageText(text),
			})
		}
	case "reasoning.delta", "reasoning":
		// Model reasoning → agent_thought_chunk (rendered as the agent's
		// thinking by ACP hosts that surface thoughts).
		if text, _ := m["text"].(string); text != "" {
			_ = s.conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: s.sessionID,
				Update:    acp.UpdateAgentThoughtText(text),
			})
		}
	case "tool.call":
		// A governed tool call started → tool_call update (pending). The host
		// renders the tool name + raw input; the call itself executes in fleet's
		// host sandbox under policy, NOT in the editor.
		id, _ := m["id"].(string)
		name, _ := m["name"].(string)
		if id == "" {
			return
		}
		opts := []acp.ToolCallStartOpt{acp.WithStartStatus(acp.ToolCallStatusInProgress)}
		if input, _ := m["input"].(string); input != "" {
			opts = append(opts, acp.WithStartRawInput(rawInputMap(input)))
		}
		_ = s.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: s.sessionID,
			Update:    acp.StartToolCall(acp.ToolCallId(id), toolTitle(name), opts...),
		})
	case "tool.result":
		// The governed tool finished → tool_call_update with the terminal
		// status. The host updates the same tool-call card it rendered above.
		id, _ := m["id"].(string)
		if id == "" {
			return
		}
		status := acp.ToolCallStatusCompleted
		if isErr, _ := m["is_err"].(bool); isErr {
			status = acp.ToolCallStatusFailed
		}
		_ = s.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: s.sessionID,
			Update:    acp.UpdateToolCall(acp.ToolCallId(id), acp.WithUpdateStatus(status)),
		})
	default:
		// turn.started / turn.completed / turn.cancelled / turn.model_required /
		// tool.approval_required / memory.proposed etc. are fleet-internal SSE
		// signals with no spec-compliant session/update analogue. The approval
		// flow is surfaced over OUTBOUND request_permission (see approver.go),
		// not as a passive session/update, so these are intentionally not
		// mirrored — mapping them would only emit unknown updates a generic ACP
		// host cannot render.
	}
}

// toolResult streams a terminal tool_call_update for a previously-started
// tool_call id — used by the approval resolution pass so the editor's tool card
// flips from the in-loop pending/blocked state to the real outcome once a staged
// critical tool has been approved (or denied) and executed out of band.
func (s *ingressSink) toolResult(toolCallID string, isErr bool) {
	if toolCallID == "" {
		return
	}
	status := acp.ToolCallStatusCompleted
	if isErr {
		status = acp.ToolCallStatusFailed
	}
	_ = s.conn.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: s.sessionID,
		Update:    acp.UpdateToolCall(acp.ToolCallId(toolCallID), acp.WithUpdateStatus(status)),
	})
}

// toolTitle returns a human-readable tool-call title for the editor card.
func toolTitle(name string) string {
	if name == "" {
		return "Tool call"
	}
	return name
}
