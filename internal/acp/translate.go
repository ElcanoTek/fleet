package acp

import (
	"strings"
	"sync"
	"unicode"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/chattui"
)

// translator turns one fleet turn's SSE frames (POST /chat — the same stream
// the web UI and `fleet chat` render) into ACP session/update notifications.
//
//	text.delta         → agent_message_chunk
//	text.replace       → nothing when the final step's streamed text already
//	                     reads as it; else the missing suffix, or
//	                     revisedMarker + the final text (see replace)
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

	// sent is all the assistant text the client has been shown this turn,
	// step the part of it streamed since the last tool event (the model step
	// still writing), and lastStep the latest closed step that streamed
	// non-blank text. A text.replace is reconciled against them (see
	// replace): fleet's final text is one step's text, not the whole turn's.
	sent          strings.Builder
	step          strings.Builder
	lastStep      string
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
	endedBy    string        // the terminal frame's name (turn.completed, turn.cancelled, …)
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

// endedOnItsOwn reports whether the watched turn ended other than by being
// cancelled — it completed, failed (turn.error), or needed a model choice
// (turn.model_required) — so a Stop sent to it stopped nothing and what it
// did stands. Only turn.cancelled is a stop.
func (t *translator) endedOnItsOwn() bool {
	t.terminalMu.Lock()
	defer t.terminalMu.Unlock()
	return t.terminal && t.endedBy != "turn.cancelled"
}

// cancelledTurn reports whether the watched turn's terminal frame is
// turn.cancelled: the one frame that confirms a Stop.
func (t *translator) cancelledTurn() bool {
	t.terminalMu.Lock()
	defer t.terminalMu.Unlock()
	return t.endedBy == "turn.cancelled"
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
			t.step.WriteString(s)
			t.send(acpsdk.UpdateAgentMessageText(s))
		}
	case "text.replace":
		t.replace(ev.Str("text"))
	case "reasoning.delta":
		if s := ev.Str("text"); s != "" {
			t.send(acpsdk.UpdateAgentThoughtText(s))
		}
	case "tool.call":
		t.endStep()
		id := orDefault(ev.Str("id"), "call-"+randomID())
		t.send(acpsdk.StartToolCall(acpsdk.ToolCallId(id), orDefault(ev.Str("name"), "tool"),
			acpsdk.WithStartKind(acpsdk.ToolKindOther),
			acpsdk.WithStartStatus(acpsdk.ToolCallStatusInProgress)))
	case "tool.result":
		t.endStep()
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
			t.endedBy = ev.Name
			close(t.endedCh)
		}
		t.terminalMu.Unlock()
	}
}

// endStep closes the model step whose text was streaming. fleet's tool loop
// streams a step's text first and announces its tool calls only once the
// step's stream has ended, then their results, and the next step's text
// follows those: a tool event is where one step's text ends. Several calls in
// a row close one step, so an empty step never displaces lastStep.
func (t *translator) endStep() {
	if s := t.step.String(); strings.TrimSpace(s) != "" {
		t.lastStep = s
	}
	t.step.Reset()
}

// replace reconciles fleet's authoritative final text with what was streamed.
//
// The final text is not everything the turn streamed. agentcore takes it from
// the round's completed response, which is the latest model step that wrote
// text, trimmed: the narration a model writes before a tool call ("I'll
// compute that first") is streamed but is not part of it. So it is compared
// first with the text streamed since the last tool event, or, when the step
// after the last tool wrote nothing, with the latest step that did. Only if
// that fails is it compared with the whole turn's text, which is what fleet
// falls back to for a provider that returns no completed text. Comparing
// against the whole turn alone made every turn that wrote before a tool call
// look revised, and the client got its answer twice.
//
// ACP cannot retract a chunk, so: already shown → nothing; an extension →
// only the missing suffix; nothing streamed → the final text; a divergence
// (fleet replaced what was streamed: a finalize pass stripped a tool call the
// model wrote into its final answer, a model call was retried after streaming
// part of a reply) → the final text again after revisedMarker, so the client
// ends on what fleet persisted. A tool call the model wrote as text that the
// finalize pass then ran for real is a closed step like any narration: the
// client shows it, the tool, then the answer, with no marker.
func (t *translator) replace(final string) {
	step := t.step.String()
	if strings.TrimSpace(step) == "" {
		step = t.lastStep
	}
	// A whole turn that already reads as final exactly is checked first: when
	// fleet's final text is the whole turn (no completed response text) and
	// its last step is a prefix of it (two steps that each wrote "A", a final
	// "AA"), the last step alone would read as an extension and repeat it.
	rest, shown := unsent(final, t.sent.String())
	if !shown || rest != "" {
		rest, shown = unsent(final, step)
		if !shown {
			rest, shown = unsent(final, t.sent.String())
		}
	}
	switch {
	case !shown:
		t.send(acpsdk.UpdateAgentMessageText(revisedMarker + final))
	case rest != "":
		t.send(acpsdk.UpdateAgentMessageText(rest))
	}
	// The client now ends on final, whichever way it got there.
	t.sent.Reset()
	t.sent.WriteString(final)
	t.step.Reset()
	t.step.WriteString(final)
	t.lastStep = ""
}

// unsent reports whether a client that was streamed `streamed` already reads
// as `final` up to a missing tail, and returns that tail: "" when it reads as
// final, the rest when final extends it. fleet trims its final text, so
// whitespace streamed before or after it is no difference (a step after a
// tool often opens with a blank line). Trailing whitespace is ignored only
// when nothing follows it, though: appending a tail after streamed whitespace
// final does not have ("Hello\n\n" then "Hello world") would leave the client
// reading neither. Blank streamed text is extended by any final text, so a
// turn that streamed nothing gets the final text whole.
func unsent(final, streamed string) (rest string, shown bool) {
	s := strings.TrimLeftFunc(streamed, unicode.IsSpace)
	if strings.HasPrefix(final, s) {
		return final[len(s):], true
	}
	if strings.TrimRightFunc(s, unicode.IsSpace) == final {
		return "", true
	}
	return "", false
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
