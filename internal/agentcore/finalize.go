package agentcore

import (
	"context"

	"charm.land/fantasy"
)

// Interactive-only finalize hook (the chat leaked-tool-call retry +
// forced-final-summary recovery). Per the plan this is a Mode-keyed,
// interactive-only hook; P3's interactive driver supplies the real impl. Here we
// define the seam and lift the pure helpers (stripLeakedToolCalls + the nudge
// constants), and the loop calls the hook only when one is wired.

// FinalizeInput is what the loop hands the finalize hook when a run is about to
// finish. The hook may produce recovered final text (e.g. after forcing a
// summary out of a model that ended with tool calls and no prose).
type FinalizeInput struct {
	Mode         Mode
	FinalText    string
	Messages     []fantasy.Message
	Observer     Observer
	SystemPrompt string
}

// FinalizeHook is the interactive recovery hook. It returns recovered final text
// (empty to keep the loop's text) and an error. Scheduled mode passes nil.
type FinalizeHook func(ctx context.Context, in FinalizeInput) (recovered string, err error)

// leakedToolCallNudge tells the model it narrated a tool call as text and must
// invoke it for real. (Used by the P3 interactive finalize impl.)
const leakedToolCallNudge = "It looks like you wrote a tool call as plain text (e.g. `call:...{...}`) instead of invoking it, so nothing ran. Tools are called through the function-call mechanism, not by typing them in your message. Make the call you intended now, then finish the task."

// forceFinalSummaryNudge tells the model to turn the work it already did into a
// written answer, without reaching for more tools. (Used by the P3 impl.)
const forceFinalSummaryNudge = "Write your complete response to my request now, using the results of the work you already did above. Do not call any tools — just give me the answer."

// these constants are part of the seam contract for the P3 interactive driver;
// reference them so the package compiles cleanly until P3 wires the impl.
var _ = leakedToolCallNudge
var _ = forceFinalSummaryNudge
