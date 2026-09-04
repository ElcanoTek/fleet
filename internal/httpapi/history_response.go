package httpapi

import (
	"encoding/json"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// The OWNER-facing history wire shape.
//
// agent.HistoryEntry deliberately does not marshal its InjectedContext field
// (`json:"-"`): the same struct is projected straight into the public share
// snapshot and the team-shared read view, and a field that serialized by
// default would publish one user's absolute attachment paths and the admin's
// shared-library listing to every reader of those two surfaces. So a response
// that wants it says so, and only the owner's own conversation read does.
//
// Wire contract (docs/ATTACHMENT-SCOPING.md) — one entry:
//
//	{"id": 1234, "role": "user", "type": "text",
//	 "content": {"text": "what is the CPM?"},
//	 "injected_context": "\n\n---\n**User attached files:** …"}
//
// `content.text` is what the user typed, nothing appended.
// `injected_context` is the server-derived suffix for that turn (attachment
// manifest, workspace inventory, shared file library announcement, expanded
// `@file`/`@url` handles, skill note, connector hints): a string, absent when
// empty, present only on user text entries. Render it outside the user's
// bubble (a collapsed system note) or not at all — it is context the server
// added, not words the user wrote.
//
// Rows written before migration 056 have it empty with the blocks still inside
// `content.text`; a client must therefore tolerate both and must not assume a
// user bubble is free of injected markup on old conversations.
type historyEntryResponse struct {
	ID              int64           `json:"id,omitempty"`
	Role            string          `json:"role"`
	Type            string          `json:"type"`
	Content         json.RawMessage `json:"content"`
	InjectedContext string          `json:"injected_context,omitempty"`
}

// historyForClient projects loaded history into the owner-facing shape above.
// Always returns a non-nil slice so the JSON carries `[]`, not `null`, for an
// empty conversation — the shape LoadHistory's direct marshal used to produce
// for a fresh chat.
func historyForClient(entries []agent.HistoryEntry) []historyEntryResponse {
	out := make([]historyEntryResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, historyEntryResponse{
			ID:              e.ID,
			Role:            e.Role,
			Type:            e.Type,
			Content:         e.Content,
			InjectedContext: e.InjectedContext,
		})
	}
	return out
}
