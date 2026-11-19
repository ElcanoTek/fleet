package agentcore

import "strings"

// Prefixes of the result a staging gate returns when the call did exactly what
// it is for: it put a card in front of the user (a memory, note or skill
// proposal, or an email draft preview) instead of running a tool. The gate
// answers through the blocked path, so the result is marked as an error, but
// nothing failed. Their _FAILED and _UNAVAILABLE siblings are real failures
// and are deliberately not listed.
const (
	MemoryProposedPrefix   = "MEMORY_PROPOSED:"
	NoteProposedPrefix     = "NOTE_PROPOSED:"
	SkillProposedPrefix    = "SKILL_PROPOSED:"
	PreviewDisplayedPrefix = "PREVIEW_DISPLAYED:"
)

var stagedResultPrefixes = []string{MemoryProposedPrefix, NoteProposedPrefix, SkillProposedPrefix, PreviewDisplayedPrefix}

// IsStagedResult reports whether a tool result's text is a staging gate's
// success: a card shown to the user, not a failed call. Readers that judge a
// turn by its error results (the chat escalation signals) use it so a run of
// proposals is not read as a run of failures.
func IsStagedResult(text string) bool {
	text = strings.TrimSpace(text)
	for _, p := range stagedResultPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}
