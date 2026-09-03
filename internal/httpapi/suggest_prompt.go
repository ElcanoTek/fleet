// Save a chat to the prompt library — the synthesis half.
//
// A chat that solved something well is worth keeping as a procedure, not as a
// question. This endpoint turns the whole conversation into a reusable
// WORKFLOW TEMPLATE draft (name + description + content — a host-side model
// call, same pattern as promote-to-task/#455) and RETURNS it; nothing is
// persisted here. The client opens the draft in an editable review dialog and
// saves the user-approved version through the orchestrator's existing
// POST /prompts, which owns the prompt_library table and its permissions.
//
// The transcript handed to the synthesizer is deliberately NOT the one
// promote-to-task uses. That one keeps the text turns, because a recurring
// task needs the ask. A workflow needs the method: which tools ran, in what
// order, and where the run hit trouble. See workflowTranscriptFromHistory.

package httpapi

import (
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// handleSuggestPrompt backs POST /conversations/{id}/suggest-prompt. It loads
// the conversation the caller owns, synthesizes a workflow-template draft from
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
	transcript := workflowTranscriptFromHistory(history)
	if strings.TrimSpace(transcript) == "" {
		http.Error(w, "conversation has no content to save as a prompt", http.StatusUnprocessableEntity)
		return
	}

	draft, err := s.agent.SuggestLibraryPrompt(ctx, agent.LibraryPromptInput{
		Transcript: transcript,
		Title:      conv.Title,
		Persona:    conv.Persona,
		Connectors: conv.OptionalMCPServersEnabled,
	})
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
