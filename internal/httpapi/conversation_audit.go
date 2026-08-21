// Per-conversation tool-call audit read path (#224):
// GET /conversations/{id}/audit. Split out of server.go (#1127); the write
// side (deriving audit rows from turn history) lives in tool_audit.go.

package httpapi

import (
	"net/http"
	"strconv"
	"time"
)

// auditDefaultLimit / auditMaxLimit bound the per-conversation audit page size.
const (
	auditDefaultLimit = 50
	auditMaxLimit     = 200
)

// handleConversationAudit serves GET /conversations/{id}/audit — the persistent,
// queryable tool-call audit log for one conversation (#224).
//
// Membership scope: it 404s a conversation the caller doesn't own (store.Get is
// scoped by user_email), so one user can never read another's tool history. This
// reuses the exact ownership check every other conversation sub-route uses
// (handleStream, the GET conversationByID body) — there is no separate, weaker
// authorization path.
//
// Query params (all optional): tool (filter to one tool name), from (RFC3339 or
// YYYY-MM-DD lower bound on started_at), limit (default 50, max 200). The
// response shape mirrors the stored row, with redacted args/result summaries —
// raw secret values never reach this endpoint (see deriveToolCallEntries).
func (s *Server) handleConversationAudit(w http.ResponseWriter, r *http.Request, convID string) {
	user := userFromCtx(r.Context())
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	toolFilter := r.URL.Query().Get("tool")
	fromUnix := parseAuditFrom(r.URL.Query().Get("from"))

	limit := auditDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > auditMaxLimit {
		limit = auditMaxLimit
	}

	entries, err := s.store.ListToolCalls(r.Context(), convID, toolFilter, fromUnix, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Named "rows" (not "tools") to avoid shadowing the package-level `tools`
	// import used elsewhere in this file.
	rows := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		row := map[string]any{
			"id":             e.ID,
			"turn_id":        e.TurnID,
			"tool_name":      e.ToolName,
			"args_summary":   e.ArgsSummary,
			"result_summary": e.ResultSummary,
			"is_error":       e.IsError,
			"started_at":     e.StartedAt,
		}
		if e.DurationMS != nil {
			row["duration_ms"] = *e.DurationMS
		}
		rows = append(rows, row)
	}
	writeJSON(w, map[string]any{"tool_calls": rows})
}

// parseAuditFrom parses the `from` audit query param, accepting either a full
// RFC3339 timestamp or a bare YYYY-MM-DD date. Returns the unix-second lower
// bound, or 0 (no floor) when the value is empty or unparseable — a malformed
// filter degrades to "no lower bound" rather than erroring the request.
func parseAuditFrom(raw string) int64 {
	if raw == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.Unix()
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.Unix()
	}
	return 0
}
