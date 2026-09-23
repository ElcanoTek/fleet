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
//	tool.result        → tool_call_update (completed / failed; pending while
//	                     a staged call awaits approval)
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
	// turn is the watched turn's id (turn.started), under convMu; turnKnown
	// closes once it is set. A Stop names it so the server cannot cancel a
	// different turn.
	turn      string
	turnKnown chan struct{}

	// sent is the assistant text the client has been told is visible, so a
	// text.replace can be reconciled against it.
	sent          strings.Builder
	approvals     []stagedApproval
	policyBlocked bool
	usage         *acpsdk.Usage

	// awaiting counts, per tool name, approval cards created (a
	// tool.approval_required event) whose placeholder result has not been seen
	// yet. The card event carries no call id, so a result is matched to a card
	// by tool name; the placeholder alone is not proof a card exists — a
	// staging failure returns the same prefix with no card.
	awaiting map[string]int

	// terminal is set once the server reported the turn's end (completed,
	// cancelled, errored, model-required). Read by the stop watcher.
	terminalMu sync.Mutex
	terminal   bool
	endedCh    chan struct{} // closed when terminal is set
}

type stagedApproval struct{ id, tool string }

// approvalSentinel prefixes the placeholder result of a tool call that was
// staged for approval rather than run.
const approvalSentinel = "APPROVAL_REQUIRED:"

// previewEmailTool stages a display-only card (a draft preview whose one
// action is Dismiss), announced with the same tool.approval_required event as
// a real approval; previewSentinel prefixes its placeholder result.
const (
	previewEmailTool = "preview_email"
	previewSentinel  = "PREVIEW_DISPLAYED:"
)

func newTranslator(sessionID acpsdk.SessionId, convID string, send func(acpsdk.SessionUpdate)) *translator {
	t := &translator{sessionID: sessionID, send: send, convKnown: make(chan struct{}), turnKnown: make(chan struct{}), endedCh: make(chan struct{}), awaiting: map[string]int{}}
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

// ended reports whether the server said the watched turn is over.
func (t *translator) ended() bool {
	t.terminalMu.Lock()
	defer t.terminalMu.Unlock()
	return t.terminal
}

func (t *translator) setTurn(id string) {
	if id == "" {
		return
	}
	t.convMu.Lock()
	defer t.convMu.Unlock()
	if t.turn == "" {
		close(t.turnKnown)
	}
	t.turn = id
}

func (t *translator) turnID() string {
	t.convMu.Lock()
	defer t.convMu.Unlock()
	return t.turn
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
	case "turn.started", "turn.identified":
		t.setTurn(ev.Str("turn_id"))
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
			name := ev.Str("name")
			switch isErr, _ := ev.Data["is_err"].(bool); {
			case strings.HasPrefix(ev.Str("text"), previewSentinel):
				// preview_email has no execution path: its card IS the result
				// (display-only, Dismiss is the one action). Shown, not failed.
				status = acpsdk.ToolCallStatusCompleted
			case strings.HasPrefix(ev.Str("text"), approvalSentinel) && t.awaiting[name] > 0:
				// A staged critical tool resolves its call with an is_err
				// APPROVAL_REQUIRED placeholder. When a card was actually
				// created for it, that is a pause for a person (the reading
				// `fleet chat` gives it), not a failure. Without a card (the
				// staging itself failed) nothing can be approved, so it stays
				// failed.
				t.awaiting[name]--
				status = acpsdk.ToolCallStatusPending
			case isErr:
				status = acpsdk.ToolCallStatusFailed
			}
			t.send(acpsdk.UpdateToolCall(acpsdk.ToolCallId(id), acpsdk.WithUpdateStatus(status)))
		}
	case "tool.approval_required":
		if id := ev.Str("approval_id"); id != "" {
			t.approvals = append(t.approvals, stagedApproval{id: id, tool: ev.Str("tool")})
			if ev.Str("tool") != previewEmailTool {
				t.awaiting[ev.Str("tool")]++
			}
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
	switch ev.Name {
	case "turn.completed", "turn.cancelled", "turn.error", "turn.model_required":
		t.terminalMu.Lock()
		if !t.terminal {
			t.terminal = true
			close(t.endedCh)
		}
		t.terminalMu.Unlock()
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
