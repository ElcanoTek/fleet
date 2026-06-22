package agent

import (
	"regexp"
	"strings"

	"charm.land/fantasy"
)

// leakedToolCallRe matches a Gemini "function call narrated as plain text"
// leak, e.g. `call:default_api:download_url{output_dir:...,url:...}`. Some
// Gemini Flash turns emit a tool call as prose instead of a structured call;
// it never executes and the raw syntax lands in the user-visible reply. We
// strip these so the user never sees the gibberish — and so a reply that was
// ONLY a leaked call collapses to empty and triggers the forced-summary
// fallback below. Observed in the wild as call:default_api:download_url{...}.
//
// Intentionally narrow: namespace:name{...} with no nested braces. Real prose
// virtually never matches, and a false positive only costs us one stripped
// fragment, so erring toward matching the known leak shape is safe.
var leakedToolCallRe = regexp.MustCompile(`call:[A-Za-z0-9_.]+:[A-Za-z0-9_]+\{[^{}]*\}`)

// stripLeakedToolCalls removes leaked tool-call-as-text fragments from a
// model reply and trims the result. Text that isn't a leaked call is
// preserved untouched. Cheap no-op when the reply has no "call:" marker.
func stripLeakedToolCalls(text string) string {
	if text == "" || !strings.Contains(text, "call:") {
		return text
	}
	return strings.TrimSpace(leakedToolCallRe.ReplaceAllString(text, ""))
}

// leakedToolCallNudge tells the model it narrated a tool call as text and must
// invoke it for real.
const leakedToolCallNudge = "It looks like you wrote a tool call as plain text (e.g. `call:...{...}`) instead of invoking it, so nothing ran. Tools are called through the function-call mechanism, not by typing them in your message. Make the call you intended now, then finish the task."

// forceFinalSummaryNudge tells the model to turn the work it already did into
// a written answer, without reaching for more tools.
const forceFinalSummaryNudge = "Write your complete response to my request now, using the results of the work you already did above. Do not call any tools — just give me the answer."

// buildForceSummaryMessages reconstructs the conversation (prior history plus
// this turn's tool work) and appends the forcing nudge as a final user turn.
// Pulled out of forceFinalSummary so the prompt shape is unit-testable
// without a live model: the result must replay all the work the model already
// did and end with the nudge, so the follow-up call has the context it needs
// to summarize.
func buildForceSummaryMessages(priorHistory, turnHistory []HistoryEntry) ([]fantasy.Message, error) {
	allEntries := make([]HistoryEntry, 0, len(priorHistory)+len(turnHistory))
	allEntries = append(allEntries, priorHistory...)
	allEntries = append(allEntries, turnHistory...)
	convo, err := replayHistory(allEntries)
	if err != nil {
		return nil, err
	}
	return append(convo, fantasy.NewUserMessage(forceFinalSummaryNudge)), nil
}

// stepCap turns the configured per-turn iteration limit into a fantasy stop
// condition. Zero (or negative) means "no cap" — preserves the prior
// behavior of looping until the model itself stops. Wiring this closes a
// latent gap where CHAT_MAX_ITERATIONS was read into config but never applied,
// so a model that never stopped calling tools was bounded only by the
// per-turn wall-clock timeout (CHAT_TURN_TIMEOUT_SECONDS) and the cost ceiling.
func stepCap(maxIterations int) []fantasy.StopCondition {
	if maxIterations <= 0 {
		return nil
	}
	return []fantasy.StopCondition{fantasy.StepCountIs(maxIterations)}
}
