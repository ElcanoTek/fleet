package httpapi

import (
	"encoding/json"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/tools"
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

// isPrivateTurnTraceEntry reports whether e is a working-trace entry
// (tool_call, tool_result, reasoning, …) rather than a rendered message:
// anything but text, a compaction summary or its content-free boundary, or a
// conversation-wide model upgrade notice. Working-trace entries are private to
// the turn's sender and dropped for every other participant.
func isPrivateTurnTraceEntry(e agent.HistoryEntry) bool {
	if e.Type == agent.EntryTypeNotice {
		var c agent.NoticeContent
		if json.Unmarshal(e.Content, &c) == nil && c.Kind == agent.NoticeKindModelUpgraded {
			return false
		}
	}
	return e.Type != "text" && e.Type != "summary" && e.Type != agent.EntryTypeSummaryBoundary
}

// sharedCardEntries returns the indexes of the entries that draw a card the
// assistant showed the conversation (docs/GENERATIVE-UI.md): a show_ui call
// together with the UI_DISPLAYED result that answers it. Unlike the rest of
// the working trace a shown card is rendered output, like the reply text, and
// dashboard view builds its panels from it (docs/DASHBOARD-VIEW.md) — so every
// participant's transcript carries it, whoever sent the turn. A call the
// server refused (UI_INVALID) or never answered was not shown and stays with
// its sender, raw input included. The model replay (redactHistoryForSender)
// omits cards with the rest of the trace.
func sharedCardEntries(entries []agent.HistoryEntry) map[int]bool {
	var shared map[int]bool
	// call id → indexes of the show_ui calls still waiting for a result, oldest
	// first. A provider can reuse an id, even for two calls in one step: a
	// result answers the first call still waiting, as the web pairs them
	// (applyToolResult).
	open := make(map[string][]int)
	for i, e := range entries {
		// A call left unanswered when its turn ended (cancelled, or the server
		// stopped) is never answered later: a result in a later turn that
		// reuses the id answers that turn's call. So nothing waits across a
		// turn boundary — a new user message that is not a mid-turn steer, or
		// a compaction summary.
		if startsNewTurn(e) {
			clear(open)
			continue
		}
		switch e.Type {
		case "tool_call":
			var c agent.ToolCallContent
			if json.Unmarshal(e.Content, &c) == nil && c.Name == tools.ShowUIToolName {
				open[c.ID] = append(open[c.ID], i)
			}
		case "tool_result":
			var c agent.ToolResultContent
			if json.Unmarshal(e.Content, &c) != nil || c.Name != tools.ShowUIToolName {
				continue
			}
			waiting := open[c.ID]
			if len(waiting) == 0 {
				continue
			}
			call := waiting[0]
			open[c.ID] = waiting[1:]
			if !isShownCardResult(c.Text, c.IsErr) {
				continue
			}
			if shared == nil {
				shared = make(map[int]bool)
			}
			shared[call], shared[i] = true, true
		}
	}
	return shared
}

// startsNewTurn reports whether e opens a new turn in the stored history: a
// user message that was not steered into a running turn, or a compaction
// summary (or its content-free boundary).
func startsNewTurn(e agent.HistoryEntry) bool {
	switch e.Type {
	case "summary", agent.EntryTypeSummaryBoundary:
		return true
	case "text":
		if e.Role != "user" {
			return false
		}
		var c agent.TextContent
		return json.Unmarshal(e.Content, &c) != nil || !c.Steered
	}
	return false
}

// isShownCardResult reports whether a show_ui result says the card was drawn:
// the same test the web applies before rendering one (isRenderableCardCall).
func isShownCardResult(text string, isErr bool) bool {
	return !isErr && strings.HasPrefix(text, "UI_DISPLAYED")
}

// redactHistoryForSender returns a shallow copy of entries with
// InjectedContext cleared and working-trace entries (tool_call, tool_result,
// reasoning) omitted on any turn whose sender is not callerEmail, so replaying
// conversation history into a multiplayer participant's turn never feeds
// another user's private attachment paths, workspace inventory, or connector
// API responses to the model.
func redactHistoryForSender(entries []agent.HistoryEntry, callerEmail, convOwnerEmail string) []agent.HistoryEntry {
	if callerEmail == "" || len(entries) == 0 {
		return entries
	}
	out := make([]agent.HistoryEntry, 0, len(entries))
	currentTurnSender := strings.TrimSpace(convOwnerEmail)
	toolCallSenders := make(map[string]string)
	for _, e := range entries {
		if e.Type == "summary" && convOwnerEmail != "" && !strings.EqualFold(callerEmail, convOwnerEmail) {
			out = append(out, agent.HistoryEntry{
				ID:      e.ID,
				Role:    e.Role,
				Type:    agent.EntryTypeSummaryBoundary,
				Content: json.RawMessage(`{}`),
			})
			continue
		}
		sender := currentTurnSender
		var (
			tc    agent.TextContent
			hasTC bool
		)
		switch {
		case e.Role == "user" && e.Type == "text":
			sender = strings.TrimSpace(convOwnerEmail)
			if json.Unmarshal(e.Content, &tc) == nil {
				hasTC = true
				if strings.TrimSpace(tc.SenderEmail) != "" {
					sender = strings.TrimSpace(tc.SenderEmail)
				}
			}
			if !tc.Steered {
				currentTurnSender = sender
			}
		case e.Type == "tool_call":
			var tcc agent.ToolCallContent
			if json.Unmarshal(e.Content, &tcc) == nil && strings.TrimSpace(tcc.ID) != "" {
				toolCallSenders[strings.TrimSpace(tcc.ID)] = currentTurnSender
			}
		case e.Type == "tool_result":
			var trc agent.ToolResultContent
			if json.Unmarshal(e.Content, &trc) == nil {
				if strings.TrimSpace(trc.SenderEmail) != "" {
					sender = strings.TrimSpace(trc.SenderEmail)
				} else if tcSender := toolCallSenders[strings.TrimSpace(trc.ID)]; tcSender != "" {
					sender = tcSender
				}
				if trc.SenderEmail != "" {
					trc.SenderEmail = ""
					if b, err := json.Marshal(trc); err == nil {
						e.Content = b
					}
				}
			}
		}
		if sender != "" && !strings.EqualFold(callerEmail, sender) {
			if e.Type == "turn_summary" {
				if redacted, ok := redactTurnSummaryForOtherParticipant(e.Content); ok {
					e.Content = redacted
					e.InjectedContext = ""
					out = append(out, e)
				}
				continue
			}
			if isPrivateTurnTraceEntry(e) {
				continue
			}
			e.InjectedContext = ""
			if hasTC && len(tc.Images) > 0 {
				tc.Images = nil
				if b, err := json.Marshal(tc); err == nil {
					e.Content = b
				}
			}
		}
		out = append(out, e)
	}
	return out
}

func redactTurnSummaryForOtherParticipant(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var ts agent.TurnSummaryContent
	if err := json.Unmarshal(raw, &ts); err != nil {
		return nil, false
	}
	if !ts.Cancelled && ts.Failure == nil {
		return nil, false
	}
	redacted := agent.TurnSummaryContent{
		Cancelled: ts.Cancelled,
		Model:     ts.Model,
	}
	if ts.Failure != nil {
		redacted.Failure = &agent.TurnFailure{
			Reason:  ts.Failure.Reason,
			Message: "Turn failed.",
		}
	}
	b, err := json.Marshal(redacted)
	if err != nil {
		return nil, false
	}
	return b, true
}

// historyForParticipant projects loaded history into the wire shape above for
// viewerEmail, retaining each entry's InjectedContext only when viewerEmail is
// the sender of that entry (or the conversation owner for unattributed
// entries) so another user's private attachment paths or library listings are
// never exposed. Another sender's working trace is dropped, except the cards
// the assistant showed (sharedCardEntries). For the owner's own single-user chat every entry is theirs,
// so the projection carries every field. Always returns a non-nil slice so the
// JSON carries `[]`, not `null`, for an empty conversation.
func historyForParticipant(entries []agent.HistoryEntry, viewerEmail, convOwnerEmail string) []historyEntryResponse {
	out := make([]historyEntryResponse, 0, len(entries))
	currentTurnSender := strings.TrimSpace(convOwnerEmail)
	toolCallSenders := make(map[string]string)
	var sharedCards map[int]bool
	if viewerEmail != "" {
		sharedCards = sharedCardEntries(entries)
	}
	for i, e := range entries {
		if viewerEmail != "" && e.Type == "summary" && convOwnerEmail != "" && !strings.EqualFold(viewerEmail, convOwnerEmail) {
			out = append(out, historyEntryResponse{
				ID:      e.ID,
				Role:    e.Role,
				Type:    agent.EntryTypeSummaryBoundary,
				Content: json.RawMessage(`{}`),
			})
			continue
		}
		injected := e.InjectedContext
		content := e.Content
		if viewerEmail != "" {
			sender := currentTurnSender
			var (
				tc    agent.TextContent
				hasTC bool
			)
			switch {
			case e.Role == "user" && e.Type == "text":
				sender = strings.TrimSpace(convOwnerEmail)
				if json.Unmarshal(e.Content, &tc) == nil {
					hasTC = true
					if strings.TrimSpace(tc.SenderEmail) != "" {
						sender = strings.TrimSpace(tc.SenderEmail)
					}
				}
				if !tc.Steered {
					currentTurnSender = sender
				}
			case e.Type == "tool_call":
				var tcc agent.ToolCallContent
				if json.Unmarshal(e.Content, &tcc) == nil && strings.TrimSpace(tcc.ID) != "" {
					toolCallSenders[strings.TrimSpace(tcc.ID)] = currentTurnSender
				}
			case e.Type == "tool_result":
				var trc agent.ToolResultContent
				if json.Unmarshal(e.Content, &trc) == nil {
					if strings.TrimSpace(trc.SenderEmail) != "" {
						sender = strings.TrimSpace(trc.SenderEmail)
					} else if tcSender := toolCallSenders[strings.TrimSpace(trc.ID)]; tcSender != "" {
						sender = tcSender
					}
					if trc.SenderEmail != "" {
						trc.SenderEmail = ""
						if b, err := json.Marshal(trc); err == nil {
							content = b
						}
					}
				}
			}
			if sender != "" && !strings.EqualFold(viewerEmail, sender) {
				switch {
				case e.Type == "turn_summary":
					redacted, ok := redactTurnSummaryForOtherParticipant(e.Content)
					if !ok {
						continue
					}
					content = redacted
					injected = ""
				case sharedCards[i]:
					injected = ""
				case isPrivateTurnTraceEntry(e):
					continue
				default:
					injected = ""
					if hasTC && len(tc.Images) > 0 {
						tc.Images = nil
						if b, err := json.Marshal(tc); err == nil {
							content = b
						}
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
