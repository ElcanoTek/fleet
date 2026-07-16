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
	"net/http"
	"strings"
)

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

	history, err := s.store.LoadHistory(ctx, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	transcript := transcriptFromHistory(history)
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
