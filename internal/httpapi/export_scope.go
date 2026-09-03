package httpapi

import "strings"

// exportScope selects how much of a conversation a rendered transcript carries.
//
// A chat transcript has two audiences that want different documents. A person
// who wants to read (or forward) the conversation wants what was asked and what
// came back; the agent's 200-entry working trail buries that. Someone debugging
// a run wants every tool call and result. Rather than guess, the export takes
// an explicit scope and the download dialog offers it as one checkbox.
type exportScope int

const (
	// scopeConversation keeps only the human/assistant text turns — the
	// readable document. This is the default for the rendered formats
	// (Markdown, HTML): they exist to be read.
	scopeConversation exportScope = iota
	// scopeFull adds the agent's working trail: reasoning, tool calls, tool
	// results, and compaction summaries.
	scopeFull
)

// parseExportScope reads the ?include= query value. Anything unrecognized
// (including empty) means the readable default rather than an error: an export
// is a download, and failing a Save dialog over a typo'd query param helps
// nobody.
func parseExportScope(v string) exportScope {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "full", "all", "work":
		return scopeFull
	default:
		return scopeConversation
	}
}

// keeps reports whether an entry type belongs in a transcript at this scope.
func (s exportScope) keeps(entryType string) bool {
	if s == scopeFull {
		return true
	}
	return entryType == "text"
}
