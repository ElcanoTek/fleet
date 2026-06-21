package agentcore

import (
	"context"

	"charm.land/fantasy"
)

// The four seams the unified run loop is parameterized over. Each interface
// documents the intended interactive vs scheduled implementation; the DRIVERS
// (P3) supply the real impls and test doubles live in the _test files here.
//
// The genuine Mode divergence is small: who supplies the prompt (InputSource),
// where output goes (Observer), what gates tool calls + finishing (Policy), and
// how code runs (Executor). The loop body is shared.

// Mode selects the run shape. Interactive collapses the enforcement loop to one
// round (the Policy's CanFinish returns true at round 1); Scheduled runs the
// full confirm_audit-driven enforcement loop.
type Mode int

const (
	// ModeInteractive is a live, user-driven chat turn: one model pass with an
	// interactive Policy, leaked-tool-call / forced-summary finalize, SSE output.
	ModeInteractive Mode = iota
	// ModeScheduled is a one-shot run-to-completion task with audit enforcement,
	// captain's-log output, and a verifier pass.
	ModeScheduled
)

func (m Mode) String() string {
	switch m {
	case ModeInteractive:
		return "interactive"
	case ModeScheduled:
		return "scheduled"
	default:
		return statusUnknown
	}
}

// InputSource supplies the initial prompt + persona for a run.
//
//   - Interactive: a live user turn — the latest user message plus replayed
//     conversation history. The driver wraps a TurnInput (user message, history,
//     attachments, opt-in MCP selection).
//   - Scheduled: a one-shot task+persona run-to-completion — the task text and
//     persona resolved from the task row, with no live history.
type InputSource interface {
	// Prompt returns the system prompt, the seed messages for the first round
	// (history + the new user/task message), and the human-readable task label.
	Prompt(ctx context.Context) (systemPrompt string, messages []fantasy.Message, label string, err error)
}

// Observer receives run events for rendering / persistence.
//
//   - Interactive: an SSE EventSink — emits turn.started / text.delta /
//     tool.call / tool.result / turn.retry so the browser renders live.
//   - Scheduled: the captain's-log JSON writer — appends structured LogMessages
//     and writes the session log file at run end.
type Observer interface {
	// Observe records a single run event. eventType mirrors the SSE event names
	// (text.delta, tool.call, …); payload carries the event-specific fields.
	Observe(eventType string, payload map[string]any)
}

// Policy gates tool calls and finishing.
//
//   - Interactive: approvals/memory staging + cost-ceiling guard via the
//     orchestration hooks; CanFinish returns true on round 1 so the loop runs a
//     single pass (the chat 1-round collapse).
//   - Scheduled: confirm_audit-driven checkFinishEnforcement; CanFinish returns
//     false until the audit + critical-action commitments + task tracker clear.
type Policy interface {
	// BeforeToolCall runs before a tool executes. Returning blocked=true
	// short-circuits the call with msg as the tool result (no execution).
	BeforeToolCall(toolName, toolCallID, rawInput string) (blocked bool, msg string)
	// RecordToolResult is called after a tool completes so the policy can update
	// enforcement state (email accounting, critical-action discharge).
	RecordToolResult(toolName, rawInput, resultText string, succeeded bool)
	// CanFinish reports whether the run may stop at the end of the given round
	// (0-based). When false, enforcementMsgs are injected as the next round's
	// nudges. Interactive returns (true, nil) at round 0.
	CanFinish(round int) (ok bool, enforcementMsgs []string)
}

// Executor runs sandboxed code. The real per-turn / per-exec-burst container
// backend is P3's sandbox.Pool; here the interface is defined and a test double
// lives in the _test files. Both modes use the SAME Executor behind this seam.
type Executor interface {
	// RunBash executes a bash command in the run's workspace and returns stdout
	// (combined output) or an error.
	RunBash(ctx context.Context, command string) (output string, err error)
	// RunPython executes a Python snippet in the run's workspace and returns
	// stdout (combined output) or an error.
	RunPython(ctx context.Context, code string) (output string, err error)
}
