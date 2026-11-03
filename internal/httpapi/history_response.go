package httpapi

import (
	"encoding/json"
	"strings"

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

// historyForParticipant projects loaded history for viewerEmail, retaining each
// entry's InjectedContext only when viewerEmail is the sender of that entry
// (or the conversation owner for unattributed entries) so another user's
// private attachment paths or library listings are never exposed.
// redactHistoryForSender returns a shallow copy of entries with
// InjectedContext cleared on any entry whose sender is not callerEmail, so
// replaying conversation history into a multiplayer participant's turn never
// feeds another user's private attachment paths, workspace inventory, or
// expanded context handles to the model.
func redactHistoryForSender(entries []agent.HistoryEntry, callerEmail, convOwnerEmail string) []agent.HistoryEntry {
	if callerEmail == "" || len(entries) == 0 {
		return entries
	}
	out := make([]agent.HistoryEntry, len(entries))
	copy(out, entries)
	for i := range out {
		sender := strings.TrimSpace(convOwnerEmail)
		var (
			tc    agent.TextContent
			hasTC bool
		)
		if out[i].Role == "user" && out[i].Type == "text" {
			if json.Unmarshal(out[i].Content, &tc) == nil {
				hasTC = true
				if strings.TrimSpace(tc.SenderEmail) != "" {
					sender = strings.TrimSpace(tc.SenderEmail)
				}
			}
		}
		if sender != "" && !strings.EqualFold(callerEmail, sender) {
			out[i].InjectedContext = ""
			if hasTC && len(tc.Images) > 0 {
				tc.Images = nil
				if b, err := json.Marshal(tc); err == nil {
					out[i].Content = b
				}
			}
		}
	}
	return out
}

func historyForParticipant(entries []agent.HistoryEntry, viewerEmail, convOwnerEmail string) []historyEntryResponse {
	out := make([]historyEntryResponse, 0, len(entries))
	for _, e := range entries {
		injected := e.InjectedContext
		content := e.Content
		if viewerEmail != "" {
			sender := strings.TrimSpace(convOwnerEmail)
			var (
				tc    agent.TextContent
				hasTC bool
			)
			if e.Role == "user" && e.Type == "text" {
				if json.Unmarshal(e.Content, &tc) == nil {
					hasTC = true
					if strings.TrimSpace(tc.SenderEmail) != "" {
						sender = strings.TrimSpace(tc.SenderEmail)
					}
				}
			}
			if sender != "" && !strings.EqualFold(viewerEmail, sender) {
				injected = ""
				if hasTC && len(tc.Images) > 0 {
					tc.Images = nil
					if b, err := json.Marshal(tc); err == nil {
						content = b
					}
				}
			}
		}
		out = append(out, historyEntryResponse{
			ID:              e.ID,
			Role:            e.Role,
			Type:            e.Type,
			Content:         content,
			InjectedContext: injected,
		})
	}
	return out
}
