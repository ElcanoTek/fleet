package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// handleSubagentLog serves GET /conversations/{id}/subagents/{childSessionID}
// (#1043 visibility): the transcript a chat-spawned sub-agent's governed run
// wrote — the sibling session-log file, redacted at write time like every
// session log. Mirrors the orchestrator's /logs/{task}/subagents/{child}
// endpoint with chat's own authorization layers:
//
//  1. OWNERSHIP — the conversation must load for THIS user (s.store.Get, the
//     exact check every other conversation sub-route uses);
//  2. LINKAGE — the child id must appear in the conversation's persisted
//     history (a spawn_subagent tool result names it), so one user's child
//     transcript can never be fetched through another conversation;
//  3. the id must match the strict subagent-UUID shape BEFORE any filesystem
//     path is derived from it.
//
// The transcript is a best-effort artifact by design: the file lives on the
// serving host, not in the DB, so after a wipe this returns 404 while the
// spawn's tool result (role, spend, answer) remains in the conversation.
func (s *Server) handleSubagentLog(w http.ResponseWriter, r *http.Request, convID, childID string) {
	user := userFromCtx(r.Context())
	conv, ok := s.conversationOr404(w, r, user, convID, true, "conversation not found")
	if !ok {
		return
	}
	if !agent.IsSubagentSessionID(childID) {
		http.Error(w, "invalid sub-agent session id", http.StatusBadRequest)
		return
	}

	history, err := s.store.LoadHistory(r.Context(), conv.ID)
	if err != nil {
		http.Error(w, "failed to load conversation history", http.StatusInternalServerError)
		return
	}
	linked := false
	initiatingSender := strings.TrimSpace(conv.UserEmail)
	currentTurnSender := initiatingSender
	needle := `\"child_session_id\":\"` + childID + `\"`
	for _, entry := range history {
		if entry.Role == "user" {
			var tc agent.TextContent
			if err := json.Unmarshal(entry.Content, &tc); err == nil && strings.TrimSpace(tc.SenderEmail) != "" {
				currentTurnSender = strings.TrimSpace(tc.SenderEmail)
			} else {
				currentTurnSender = strings.TrimSpace(conv.UserEmail)
			}
		}
		// The spawn tool result is a JSON string embedded in the tool_result
		// entry's content, so the child id appears with escaped quotes; check
		// the raw form too for robustness across encoders.
		if strings.Contains(string(entry.Content), needle) ||
			strings.Contains(string(entry.Content), `"child_session_id":"`+childID+`"`) {
			linked = true
			initiatingSender = currentTurnSender
			break
		}
	}
	if !linked || (initiatingSender != "" && !strings.EqualFold(initiatingSender, user)) {
		http.Error(w, "no sub-agent with this id is recorded in this conversation", http.StatusNotFound)
		return
	}

	path := agent.ChildLogFilePath(childID)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is derived from the process's own log-file config plus a strictly-validated subagent UUID — no request-controlled path components.
	if err != nil {
		http.Error(w, "sub-agent transcript file not available (it may have been cleaned up on this host)", http.StatusNotFound)
		return
	}
	// Round-trip through encoding/json rather than echoing the file bytes: it
	// validates the payload is JSON and re-encodes with HTML escaping (gosec
	// G705), so file content can never reach the response as markup.
	var child map[string]any
	if err := json.Unmarshal(data, &child); err != nil {
		http.Error(w, "sub-agent transcript file is unreadable", http.StatusInternalServerError)
		return
	}
	writeJSON(w, r, child)
}
