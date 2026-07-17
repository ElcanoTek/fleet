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
		// Re-apply previously accepted messages at their recorded positions.
		// fantasy rebuilds the step input as initialPrompt+responseMessages
		// each step, so an earlier injection is absent again until re-applied.
		for _, m := range state.accepted {
			pos := m.pos
			if pos > len(messages) {
				pos = len(messages)
			}
			if steerAlreadyPresent(messages, m.text, pos) {
				continue
			}
			injected := make([]fantasy.Message, 0, len(messages)+1)
			injected = append(injected, messages[:pos]...)
			injected = append(injected, fantasy.NewUserMessage(m.text))
			injected = append(injected, messages[pos:]...)
			messages = injected
			changed = true
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

// steerAlreadyPresent reports whether the injected user message already sits
// at (or immediately around) pos — the common case on every step after the
// accepting one, where re-application would double-inject.
func steerAlreadyPresent(messages []fantasy.Message, text string, pos int) bool {
	for _, idx := range []int{pos, pos - 1} {
		if idx < 0 || idx >= len(messages) {
			continue
		}
		m := messages[idx]
		if m.Role != fantasy.MessageRoleUser {
			continue
		}
		for _, part := range m.Content {
			if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && p.Text == text {
				return true
			}
		}
	}
	return false
}
