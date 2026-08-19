package agent

import (
	"regexp"
	"strings"
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
// a written answer, without reaching for more tools. The conversation it is
// appended to comes from agentcore.FinalizeInput.Messages (the finishing
// round's input plus its tool transcript) — see streamForceFinalSummary. A
// HistoryEntry-replay builder used to live here, but its turn-history half was
// never populated in production, so the summary saw prior turns only (#1117).
const forceFinalSummaryNudge = "Write your complete response to my request now, using the results of the work you already did above. Do not call any tools — just give me the answer."
