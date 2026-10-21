package agentcore

import (
	"strings"

	"charm.land/fantasy"
)

// completedResponseText preserves every text block in the provider's completed
// response, including text on either side of non-text content.
func completedResponseText(content fantasy.ResponseContent) string {
	var text strings.Builder
	for _, part := range content {
		if block, ok := part.(fantasy.TextContent); ok {
			text.WriteString(block.Text)
		}
	}
	return strings.TrimSpace(text.String())
}

// StepTextSeparator returns what must go between text the run has already
// streamed (prev) and the first chunk of a NEW model step (next) so the two
// read as separate paragraphs. Each step's text was written as its own message
// — typically a one-line "Let me check the deal." before a tool call, then the
// next step's "Found it." — and a plain concatenation runs them together into
// "deal.Found it.", with no space after the period. That joined string is what
// live clients render from text.delta, what turn-journal recovery persists, and
// what a delta-only provider's fallback answer is built from, so the break is
// added once, here, where the step boundary is still known.
//
// Blocks WITHIN one step are deliberately not separated (completedResponseText
// joins them raw): a provider may split a single sentence across blocks.
// Newlines already present on either side count toward the break, so a model
// that ends its step with a blank line gets nothing extra.
func StepTextSeparator(prev, next string) string {
	if strings.TrimSpace(prev) == "" || next == "" {
		return ""
	}
	have := strings.Count(prev[len(strings.TrimRight(prev, " \t\r\n")):], "\n") +
		strings.Count(next[:len(next)-len(strings.TrimLeft(next, " \t\r\n"))], "\n")
	if have >= 2 {
		return ""
	}
	return strings.Repeat("\n", 2-have)
}
