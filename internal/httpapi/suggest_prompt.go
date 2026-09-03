// Save a chat to the prompt library — the synthesis half.
//
// A user who refined a useful ask in chat can save it as a reusable
// prompt-library entry. This endpoint distills the conversation into a clean,
// self-contained draft (name + description + content — a host-side model
// call, same pattern as promote-to-task/#455) and RETURNS it; nothing is
// persisted here. The client opens the draft in an editable review dialog and
// saves the user-approved version through the orchestrator's existing
// POST /prompts, which owns the prompt_library table and its permissions.

package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// suggestPromptRequest is the optional body of POST .../suggest-prompt.
//
// UpToMessageID names the assistant reply the user is saving from. The moment
// someone decides an interaction is worth keeping is the moment they finish
// reading a good answer — so the "Save as prompt" action lives on that reply,
// and the distillation is cut off there. Without it, a chat that carried on
// into an unrelated tangent would fold the tangent into the saved recipe.
// Omitted (or zero) means the whole conversation, which is what the
// conversation-level menu item sends.
type suggestPromptRequest struct {
	UpToMessageID int64 `json:"up_to_message_id"`
}

// handleSuggestPrompt backs POST /conversations/{id}/suggest-prompt. It loads
// the conversation the caller owns, synthesizes a prompt-library draft from
// its transcript, and returns it as JSON. 404 if the conversation isn't
// owned/known, 422 if it has no distillable content, 502 if synthesis fails.
func (s *Server) handleSuggestPrompt(w http.ResponseWriter, r *http.Request, convID, user string) {
	ctx := r.Context()

	// Ownership: Get is user-scoped, so a foreign/unknown id yields nil → 404.
	conv, err := s.store.Get(ctx, user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// The body is optional and advisory: a malformed or absent one means "the
	// whole conversation", never a 400. The dialog that posts it is a
	// convenience affordance, not a contract the user should see fail.
	var req suggestPromptRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	history, err := s.store.LoadHistory(ctx, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	transcript := transcriptFromHistory(historyUpTo(history, req.UpToMessageID))
	if strings.TrimSpace(transcript) == "" {
		http.Error(w, "conversation has no content to save as a prompt", http.StatusUnprocessableEntity)
		return
	}

	draft, err := s.agent.SuggestLibraryPrompt(ctx, transcript)
	if err != nil || draft == nil {
		http.Error(w, "could not synthesize a prompt from this conversation; try again", http.StatusBadGateway)
		return
	}

	writeJSON(w, map[string]any{
		"name":        draft.Name,
		"description": draft.Description,
		"content":     draft.Content,
	})
}

// historyUpTo truncates history after the entry with the given persisted id,
// inclusive. A zero id (no cut requested) or an id that isn't in this history
// (a client-side optimistic id, an entry since compacted away) returns the
// history unchanged — a mis-aimed cut degrades to "distill everything", which
// is the behavior the user had before the per-message action existed.
func historyUpTo(history []agent.HistoryEntry, id int64) []agent.HistoryEntry {
	if id <= 0 {
		return history
	}
	for i, e := range history {
		if e.ID == id {
			return history[:i+1]
		}
	}
	return history
}
