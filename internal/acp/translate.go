package acp

import (
	"strings"
	"sync"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/chattui"
)

// translator turns one fleet turn's SSE frames (POST /chat — the same stream
// the web UI and `fleet chat` render) into ACP session/update notifications.
//
//	text.delta         → agent_message_chunk
//	text.replace       → the missing suffix, or revisedMarker + the final text
//	reasoning.delta    → agent_thought_chunk
//	tool.call          → tool_call (title = tool name, status in_progress)
//	tool.result        → tool_call_update (completed / failed)
//	tool.approval_required → a text pointer to the fleet approval card, sent
//	                     once the turn ends
//	turn.policy_blocked → stop reason "refusal"
//	turn.completed     → usage on the prompt response
//
// Tool inputs and outputs are deliberately not forwarded: they stay in fleet's
// run log, the audited record, and the ACP client gets the shape of the work.
type translator struct {
	sessionID acpsdk.SessionId
	send      func(acpsdk.SessionUpdate)

	// convID is read by the prompt's stop watcher while the stream writes it,
	// hence the lock; convKnown closes once it is set.
	convMu    sync.Mutex
	convID    string
	convKnown chan struct{}

	// sent is the assistant text the client has been told is visible, so a
	// text.replace can be reconciled against it.
	sent          strings.Builder
	approvals     []stagedApproval
	policyBlocked bool
	usage         *acpsdk.Usage
}

type stagedApproval struct{ id, tool string }

func newTranslator(sessionID acpsdk.SessionId, convID string, send func(acpsdk.SessionUpdate)) *translator {
	t := &translator{sessionID: sessionID, send: send, convKnown: make(chan struct{})}
	t.setConversation(convID)
	return t
}

func (t *translator) setConversation(id string) {
	if id == "" {
		return
	}
	t.convMu.Lock()
	defer t.convMu.Unlock()
	if t.convID == "" {
		close(t.convKnown)
	}
	t.convID = id
}

func (t *translator) conversationID() string {
	t.convMu.Lock()
	defer t.convMu.Unlock()
	return t.convID
}

func (t *translator) handle(ev chattui.Event) {
	switch ev.Name {
	case "conversation":
		t.setConversation(ev.Str("id"))
	case "text.delta":
		if s := ev.Str("text"); s != "" {
			t.sent.WriteString(s)
			t.send(acpsdk.UpdateAgentMessageText(s))
		}
	case "text.replace":
		t.replace(ev.Str("text"))
	case "reasoning.delta":
		if s := ev.Str("text"); s != "" {
			t.send(acpsdk.UpdateAgentThoughtText(s))
		}
	case "tool.call":
		id := orDefault(ev.Str("id"), "call-"+randomID())
		t.send(acpsdk.StartToolCall(acpsdk.ToolCallId(id), orDefault(ev.Str("name"), "tool"),
			acpsdk.WithStartKind(acpsdk.ToolKindOther),
			acpsdk.WithStartStatus(acpsdk.ToolCallStatusInProgress)))
	case "tool.result":
		if id := ev.Str("id"); id != "" {
			status := acpsdk.ToolCallStatusCompleted
			if isErr, _ := ev.Data["is_err"].(bool); isErr {
				status = acpsdk.ToolCallStatusFailed
			}
			t.send(acpsdk.UpdateToolCall(acpsdk.ToolCallId(id), acpsdk.WithUpdateStatus(status)))
		}
	case "tool.approval_required":
		if id := ev.Str("approval_id"); id != "" {
			t.approvals = append(t.approvals, stagedApproval{id: id, tool: ev.Str("tool")})
		}
	case "tool.approval_superseded":
		kept := t.approvals[:0]
		for _, a := range t.approvals {
			if a.tool != ev.Str("tool") {
				kept = append(kept, a)
			}
		}
		t.approvals = kept
	case "turn.policy_blocked":
		t.policyBlocked = true
	case "turn.completed":
		t.usage = usageFrom(ev.Data)
	}
}

// replace reconciles fleet's authoritative final text with what was streamed.
// ACP cannot retract a chunk, so: identical → nothing; an extension → only the
// missing suffix; a divergence (an enforcement round replaced the draft) → the
// final text again after revisedMarker, so the client ends on what fleet
// persisted.
func (t *translator) replace(final string) {
	sent := t.sent.String()
	switch {
	case final == sent:
		return
	case strings.HasPrefix(final, sent):
		t.send(acpsdk.UpdateAgentMessageText(final[len(sent):]))
	case sent == "":
		t.send(acpsdk.UpdateAgentMessageText(final))
	default:
		t.send(acpsdk.UpdateAgentMessageText(revisedMarker + final))
	}
	t.sent.Reset()
	t.sent.WriteString(final)
}

// flushApprovals appends a pointer for every approval still pending when the
// turn ended.
func (t *translator) flushApprovals(pointer func(convID, approvalID, tool string) string) {
	for _, a := range t.approvals {
		t.send(acpsdk.UpdateAgentMessageText(pointer(t.conversationID(), a.id, a.tool)))
	}
}

func usageFrom(d map[string]any) *acpsdk.Usage {
	num := func(k string) int {
		f, _ := d[k].(float64)
		return int(f)
	}
	in, out := num("prompt_tokens"), num("completion_tokens")
	if in == 0 && out == 0 {
		return nil
	}
	u := &acpsdk.Usage{InputTokens: in, OutputTokens: out, TotalTokens: in + out}
	if c := num("cached_tokens"); c > 0 {
		u.CachedReadTokens = &c
	}
	if c := num("cache_creation_tokens"); c > 0 {
		u.CachedWriteTokens = &c
	}
	return u
}
