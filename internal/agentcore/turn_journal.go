package agentcore

import "context"

// TurnJournal is the OPTIONAL durable side-effect journal seam (#798). The
// interactive driver supplies an implementation that persists to the chat
// store; the scheduled driver leaves it nil (its session log is the durable
// record). Like UsageReporter and Finalize this is driver-supplied DATA, not
// a Mode branch — the trunk calls it nil-safely on every route.
//
// ToolIntent MUST return before the tool dispatches; a non-nil error blocks
// dispatch (fail closed: no side effect without a durable intent record).
// ToolOutcome persists the exact governed, bounded model-visible result —
// byte-identical to what the model, policy audit, and stream sink receive —
// before the response returns to the provider loop. An outcome-write failure
// cannot un-run the tool, so the trunk ignores its error; the driver's
// implementation degrades itself (blocking further intents and terminal
// success) instead.
type TurnJournal interface {
	ToolIntent(ctx context.Context, callID, toolName, inputJSON string) error
	ToolOutcome(ctx context.Context, callID, toolName, governedText string, isErr bool) error
}

// journalToolIntent is the shared pre-dispatch barrier. It returns the
// governed, bounded refusal response when the journal write fails; ok=true
// means dispatch may proceed (including when no journal is configured).
func journalToolIntent(ctx context.Context, journal TurnJournal, toolName, callID, input string) (refusal string, ok bool) {
	if journal == nil {
		return "", true
	}
	if err := journal.ToolIntent(ctx, callID, toolName, input); err != nil {
		return "tool not executed: the durable turn journal is unavailable, so a side effect could not be recorded first: " + err.Error(), false
	}
	return "", true
}

// journalToolOutcome persists the final model-visible bytes. Failures are
// deliberately not surfaced to the model: the side effect already happened
// and the result must not be rewritten; the driver's journal implementation
// records the degradation and fails the turn's terminal success instead.
func journalToolOutcome(ctx context.Context, journal TurnJournal, toolName, callID, governedText string, isErr bool) {
	if journal == nil {
		return
	}
	_ = journal.ToolOutcome(ctx, callID, toolName, governedText, isErr)
}
