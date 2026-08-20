package agentcore

import (
	"context"
	"sync"

	"charm.land/fantasy"
)

// SteerSource is the OPTIONAL mid-turn input seam (#785). The interactive
// driver supplies an implementation backed by the conversation's durable
// input queue; scheduled/evals leave it nil. Like TurnJournal (#798) this is
// driver-supplied DATA, not a Mode branch — the trunk polls it nil-safely.
//
// Poll is consumed ONLY at the PrepareStep boundary: between provider steps,
// after every parallel tool goroutine of the prior step has settled, never
// mid-tool. Acknowledge durably marks the message injected BEFORE the model
// can see it; an error (or a lost race with remove/cancel) refuses injection
// and the message stays queued — it runs as the next turn instead (the
// durable fallback). A message is therefore never both injected and queued,
// and never silently dropped.
type SteerSource interface {
	Poll() (SteerMessage, bool)
	Acknowledge(ctx context.Context, id string) error
}

// SteerMessage is one steerable user input.
type SteerMessage struct {
	ID   string
	Text string
}

// steerState carries a run's accepted steer messages across steps, rounds,
// and resilience re-drives. Each accepted message records the message-slice
// position it was appended at, so later steps re-apply it at a stable index —
// the cacheable prefix keeps extending byte-stably instead of diverging
// (docs/PROMPT-CACHE-CONTRACT.md). A resilience rollback that rebuilt the
// slice shorter degrades to a clamped append: provider-valid, worst case one
// cache-miss step, never corruption.
type steerState struct {
	mu       sync.Mutex
	accepted []steeredMessage
	// initialLen is the length of the run's FIRST step-entry slice (system
	// prompt + seed history), captured on the steering step's first invocation
	// — necessarily before any acceptance has appended a steer. It is the
	// floor of the dedupe scan window: injected steers only ever live at or
	// after this index (acceptance appends past the entry slice's end, and
	// re-insertion lands at the recorded acceptance position, itself past this
	// floor), while everything below it is prior history a steer's text may
	// byte-collide with (#1125).
	initialLen int
	initialSet bool
}

type steeredMessage struct {
	id   string
	text string
	pos  int
}

// steeringStep is the #785 injection boundary, chained FIRST so the budget
// step accounts the injected tokens and the cache step attaches markers to
// the final slice. The ceiling check in budgetGuardedStep runs BEFORE the
// chain, so a run that already hit its budget aborts without accepting new
// input (the message stays queued for the next turn).
func steeringStep(source SteerSource, state *steerState, sink *streamSink) fantasy.PrepareStepFunction {
	if source == nil {
		return nil
	}
	return func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		state.mu.Lock()
		defer state.mu.Unlock()

		messages := opts.Messages
		changed := false
		// Capture the run's initial entry length exactly once, on the first
		// invocation — nothing has been accepted yet, so this slice is pure
		// system prompt + seed history and its length is the injection floor.
		if !state.initialSet {
			state.initialSet = true
			state.initialLen = len(messages)
		}
		// Bound the dedupe probes to where injected steers can actually live.
		// Everything below the floor is prior history; matching there turns a
		// byte-collision with an unrelated user message (the user's last turn
		// was "continue" and they steer "continue" now) into PERMANENT
		// suppression — the scan would claim the historical message on every
		// step and the steer would never be re-injected for the rest of the
		// run. min() is defensive: a compaction that shrank the slice below
		// the initial length must not leave an out-of-range floor.
		floor := min(state.initialLen, len(messages))
		// Re-apply previously accepted messages at their recorded positions.
		// fantasy rebuilds the step input as initialPrompt+responseMessages
		// each step, so an earlier injection is absent again until re-applied.
		//
		// claimed maps message index → already matched to an earlier accepted
		// steer this pass, so two accepted messages with identical text each
		// account for their own copy instead of both matching the same one.
		claimed := make(map[int]bool, len(state.accepted))
		for _, m := range state.accepted {
			pos := m.pos
			if pos > len(messages) {
				pos = len(messages)
			}
			// Never split a tool exchange: a mid-slice position (rollback or
			// compaction rebuilt the history shorter) advances past any tool
			// messages so a ToolCallPart keeps its results adjacent —
			// provider-valid at the cost of, at worst, one cache-miss step.
			// (Same boundary rule as compaction — see snapCutForward.)
			pos = snapCutForward(messages, pos)
			if idx, present := findInjectedSteer(messages, m.text, pos, floor, claimed); present {
				claimed[idx] = true
				// Re-record the transcript entry on the found path too: the
				// sink dedupes by SteerID, so this is idempotent when the
				// entry survived — and it closes the gap where a rollback
				// erased the entry while the injected copy itself survived
				// in the slice.
				if sink != nil {
					sink.onUserInjected(m.id, m.text)
				}
				continue
			}
			injected := make([]fantasy.Message, 0, len(messages)+1)
			injected = append(injected, messages[:pos]...)
			injected = append(injected, fantasy.NewUserMessage(m.text))
			injected = append(injected, messages[pos:]...)
			messages = injected
			changed = true
			// The insert shifted every index at or past pos up by one; move the
			// existing claims with them, then claim the inserted slot itself so a
			// later identical-text steer cannot match this one's fresh copy.
			shifted := make(map[int]bool, len(claimed)+1)
			for idx := range claimed {
				if idx >= pos {
					shifted[idx+1] = true
				} else {
					shifted[idx] = true
				}
			}
			shifted[pos] = true
			claimed = shifted
			// Re-record the transcript entry too: a resilience rollback
			// truncated the sink to the attempt mark, erasing the original
			// user_text entry — without this the model acts on steered text
			// the committed transcript never shows. The sink dedupes by
			// SteerID, so intact entries are not duplicated.
			if sink != nil {
				sink.onUserInjected(m.id, m.text)
			}
		}

		// Accept at most one NEW message per step boundary: the durable
		// queued->injected flip must land before the model can see the text.
		if msg, ok := source.Poll(); ok {
			if err := source.Acknowledge(ctx, msg.ID); err == nil {
				messages = append(messages, fantasy.NewUserMessage(msg.Text))
				state.accepted = append(state.accepted, steeredMessage{id: msg.ID, text: msg.Text, pos: len(messages) - 1})
				changed = true
				if sink != nil {
					sink.onUserInjected(msg.ID, msg.Text)
				}
			}
		}

		if !changed {
			return ctx, fantasy.PrepareStepResult{}, nil
		}
		return ctx, fantasy.PrepareStepResult{Messages: messages}, nil
	}
}

// findInjectedSteer locates an already-present copy of an injected steer
// message so re-application never double-injects. It probes pos and pos-1
// first — the cheap positional guess — then falls back to scanning the rest
// of the window: a shift of more than one slot (a shortened rebuild, a future
// flow that carries injections) would escape the positional probe alone and
// re-insert duplicate text into the provider input (#1125). A steer message
// is a plain user message carrying the exact accepted text (see
// steeringStep), so an exact text match on a user message identifies one.
//
// Both probes honor floor — the run's initial entry length — because prior
// history is exactly where an UNRELATED user message can byte-collide with a
// steer, and injected steers can never live below it. claimed excludes
// indices an earlier accepted steer already matched this pass, so
// identical-text steers each account for their own copy.
//
// The residual trade-off, stated honestly: a byte-collision WITHIN the window
// (a user message at/after the floor whose text equals the steer's) is
// claimed as the steer's copy, which suppresses re-injection on EVERY
// subsequent step, not just once. That is still the right direction — within
// the window an identical user message is overwhelmingly the steer's own
// copy (the floor already excludes seed history), and the alternative failure
// mode is duplicated user text in the provider input on every step.
func findInjectedSteer(messages []fantasy.Message, text string, pos, floor int, claimed map[int]bool) (int, bool) {
	matches := func(idx int) bool {
		if idx < floor || idx >= len(messages) || claimed[idx] {
			return false
		}
		m := messages[idx]
		if m.Role != fantasy.MessageRoleUser {
			return false
		}
		for _, part := range m.Content {
			if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && p.Text == text {
				return true
			}
		}
		return false
	}
	for _, idx := range []int{pos, pos - 1} {
		if matches(idx) {
			return idx, true
		}
	}
	for idx := floor; idx < len(messages); idx++ {
		if matches(idx) {
			return idx, true
		}
	}
	return -1, false
}
