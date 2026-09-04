package agent

import "strings"

// Server-injected turn context: how it is composed for the model, and how a
// legacy row that embedded it in the message text is un-composed again.
//
// A chat turn's user message is TWO things glued together: what the user
// actually typed, and a set of server-derived blocks the HTTP layer appends
// before the model sees it (the attachment manifest with its absolute paths,
// the workspace inventory, the shared file library announcement, expanded
// `@file`/`@url` context handles, the skill-invocation note, connector
// recommendations). The model needs both; everything else needs them apart:
//
//   - The branch copy (store.BranchConversation) copies the PARENT's user
//     messages. Carrying the parent's injected suffix into a teammate's fork
//     handed the brancher the owner's attachment paths — which their sandbox
//     then read. See docs/ATTACHMENT-SCOPING.md and ADR-0058.
//   - The transcript renders the user bubble from the message text, so an
//     admin-published library listing read as if the user had typed it.
//
// So the suffix is stored in its own column (migration 056) and carried on
// HistoryEntry.InjectedContext; ComposeUserMessage is the ONE place the two
// halves are joined for a provider call.

// ComposeUserMessage returns the exact bytes a model sees for a turn whose
// user text and injected context are stored separately.
//
// The TrimRight matches what every block appender did when it concatenated
// onto the message directly (each starts by trimming the trailing newlines of
// what it appends to), so a stored (text, injected) pair recomposes
// byte-for-byte into the string that used to be persisted as one blob. That
// byte-stability is what keeps replayed history stable for the provider
// prompt cache (docs/PROMPT-CACHE-CONTRACT.md): a recomposed old turn is not
// a new prefix.
func ComposeUserMessage(text, injected string) string {
	if injected == "" {
		return text
	}
	return strings.TrimRight(text, "\n") + injected
}

// injectedBlockMarkers are the exact opening sequences of every block the chat
// server appends to a user message. Each carries its own separator ("\n\n---\n"
// or, for the skill note, "\n\n[") so a user who literally types
// "**User attached files:**" is not mistaken for an injected block.
//
// Order does not matter: StripLegacyInjectedContext cuts at the EARLIEST match,
// and the blocks are always appended contiguously at the end of the message, so
// cutting at the first one removes every later block too — including any block
// added after this list was written.
//
// Keep in sync with the appenders: httpapi.appendAttachmentsBlock,
// httpapi.appendWorkspaceInventoryBlock, httpapi.appendSharedFilesBlock (via
// sharedfiles.PromptBlock), httpapi.appendContextHandleBlocks,
// httpapi.matchSkillInvocation, httpapi.appendConnectorRecommendationBlock.
var injectedBlockMarkers = []string{
	"\n\n---\n**User attached images**",
	"\n\n---\n**User attached files:**",
	"\n\n---\n**Workspace files persisted from earlier turns**",
	"\n\n---\n**Shared file library**",
	"\n\n---\n**Contents of @file:",
	"\n\n---\n**Fetched @url:",
	"\n\n---\n**Context handle notices:**",
	"\n\n---\n**Possibly-relevant connectors (NOT currently connected):**",
	"\n\n[Skill invoked: ",
}

// StripLegacyInjectedContext splits a message written BEFORE migration 056 —
// when the injected blocks lived inside content.text — into the user's own
// text and the injected suffix. It reports whether it cut anything.
//
// This is belt-and-suspenders, not the mechanism: rows written after 056 carry
// the suffix in its own column and reach here with nothing to strip. It exists
// because a box that has been running for months has thousands of rows whose
// text still embeds an absolute attachment path, and the branch path must not
// copy those into another user's conversation just because the row predates
// the fix.
//
// Marker-based by necessity (the split was never recorded for those rows), so
// it is deliberately conservative: it matches the full separator + bold header
// sequence, and only ever cuts a suffix — it never rewrites the interior of a
// message.
func StripLegacyInjectedContext(text string) (userText, injected string, stripped bool) {
	cut := -1
	for _, marker := range injectedBlockMarkers {
		if i := strings.Index(text, marker); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut < 0 {
		return text, "", false
	}
	return text[:cut], text[cut:], true
}
