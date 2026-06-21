package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// summarizeRequest is the body of POST /conversations/{id}/summarize.
//
// Model is optional — the conversation's stored slug is used by
// default. Callers (the Next.js layer + e2e harness) usually echo the
// chat's currently-selected slug so the summary inherits the chat's
// quality profile.
type summarizeRequest struct {
	Model string `json:"model"`
}

// summarizeResponse mirrors the persisted SummaryContent so the
// frontend can append the new entry to its in-memory history without
// a separate refetch. id + created_at are populated by the
// conversation reload that the UI does after success — we don't ship
// them from this endpoint to keep it write-only.
type summarizeResponse struct {
	Type             string  `json:"type"`
	Role             string  `json:"role"`
	Text             string  `json:"text"`
	Model            string  `json:"model"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// handleSummarize runs the summarize-and-continue flow:
//
//  1. Refuse if a turn is in flight on this conversation. Compaction
//     races a live turn would corrupt the on-the-wire context for
//     that turn — better to make the user wait the few seconds it
//     takes to land than to clobber their reply.
//  2. Load the full history. Empty history short-circuits with 400.
//  3. Resolve the model: explicit body field wins, else the
//     conversation's stored slug, else 400 (server holds no default).
//  4. Stream agent.Manager.Summarize — text deltas go to the browser
//     as they generate so a 30-60s wait reads as a chat message
//     materializing instead of a frozen spinner.
//  5. Persist as a `summary` HistoryEntry via store.ReplaceSummary
//     (deletes any prior summary in the same tx).
//  6. Emit a final `summary.completed` event with usage + cost.
//
// Wire shape — text/event-stream with these events:
//
//	event: summary.delta
//	data: {"text": "<chunk>"}
//
//	event: summary.completed
//	data: {"text": "...", "model": "...", "prompt_tokens": N,
//	       "completion_tokens": N, "cost_usd": F}
//
//	event: summary.error
//	data: {"message": "..."}
//
// Pre-stream errors (validation, in-flight conflict, missing model)
// still come back as plain HTTP error codes so the frontend can
// distinguish them from mid-stream model failures.
func (s *Server) handleSummarize(w http.ResponseWriter, r *http.Request, user, convID string) {
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	// Compaction can't safely overlap a live turn. The agent built
	// for the in-flight turn already snapshotted the history; a
	// summary persisted mid-flight would not affect the in-flight
	// context, but a *user-visible* race ("I summarized but my reply
	// still shows the full scroll") is more confusing than just
	// asking them to wait. Refuse cleanly.
	if entry, ok := s.getInflight(convID); ok && entry.IsRunning() {
		http.Error(w, "a turn is currently running on this conversation; wait for it to finish before summarizing", http.StatusConflict)
		return
	}

	var req summarizeRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(conv.Model)
	}
	if model == "" {
		http.Error(w, "model required (no body field, conversation has no stored slug)", http.StatusBadRequest)
		return
	}
	if conv.Lockdown && !s.cfg.LockdownAllows(model) {
		http.Error(w, "model not allowed in lockdown mode", http.StatusBadRequest)
		return
	}

	history, err := s.store.LoadHistory(r.Context(), convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(history) == 0 {
		http.Error(w, "conversation has no history to summarize", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "response writer does not support flushing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	emit := func(event string, payload any) {
		body, mErr := json.Marshal(payload)
		if mErr != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
		flusher.Flush()
	}

	result, sumErr := s.agent.Summarize(r.Context(), SummarizeInput{
		History:  history,
		Model:    model,
		Lockdown: conv.Lockdown,
		OnTextDelta: func(text string) {
			emit("summary.delta", map[string]any{"text": text})
		},
	})
	if sumErr != nil {
		// Don't surface ctx cancellation as an error event — the client
		// has already disconnected and there's nothing useful to send.
		if errors.Is(sumErr, r.Context().Err()) {
			return
		}
		emit("summary.error", map[string]any{"message": sumErr.Error()})
		return
	}

	entry := mustSummaryEntry(agent.SummaryContent{
		Text:             result.Text,
		Model:            result.Model,
		PromptTokens:     result.PromptTokens,
		CompletionTokens: result.CompletionTokens,
		CostUSD:          result.CostUSD,
	})
	if err := s.store.ReplaceSummary(r.Context(), user, convID, entry); err != nil {
		emit("summary.error", map[string]any{"message": err.Error()})
		return
	}

	emit("summary.completed", summarizeResponse{
		Type:             "summary",
		Role:             "assistant",
		Text:             result.Text,
		Model:            result.Model,
		PromptTokens:     result.PromptTokens,
		CompletionTokens: result.CompletionTokens,
		CostUSD:          result.CostUSD,
	})
}

// mustSummaryEntry mirrors the agent package's mustEntry helper so
// the HTTP layer can build a HistoryEntry without exporting the
// internal one. Marshal failures here would be a bug — SummaryContent
// is a static struct.
func mustSummaryEntry(c agent.SummaryContent) agent.HistoryEntry {
	b, err := json.Marshal(c)
	if err != nil {
		panic("summary content marshal failed: " + err.Error())
	}
	return agent.HistoryEntry{
		Role:    "assistant",
		Type:    "summary",
		Content: b,
	}
}
