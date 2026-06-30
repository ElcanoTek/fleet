// Conversation branching (#454).
//
// An authenticated owner forks one of their conversations at a chosen message
// (POST /conversations/{id}/branch) into a NEW independent conversation that
// copies the parent's messages up to that point, then diverges. The branch is a
// normal conversation (it appears in the sidebar like any other) with lineage
// metadata recording where it came from; the parent is untouched.

package httpapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/store"
)

// handleConversationBranch forks the conversation the caller owns at
// branch_point_message_id into a new conversation. Body:
//
//	{ "branch_point_message_id": <messages.id>, "title": "optional name" }
//
// Returns 201 with the new conversation. 404 if the parent isn't owned/known,
// 400 for a missing/invalid branch point.
func (s *Server) handleConversationBranch(w http.ResponseWriter, r *http.Request, parentConvID, user string) {
	// Confirm the parent exists and belongs to the caller before forking (Get is
	// user-scoped, so a foreign/unknown id yields nil → 404). Also gives us the
	// parent title for the default branch name.
	parent, err := s.store.Get(r.Context(), user, parentConvID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if parent == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var body struct {
		BranchPointMessageID int64  `json:"branch_point_message_id"`
		Title                string `json:"title"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if body.BranchPointMessageID <= 0 {
		http.Error(w, "branch_point_message_id must be a positive message id", http.StatusBadRequest)
		return
	}

	title := strings.TrimSpace(body.Title)
	if title == "" {
		// A branch copies history, so the auto-titler (which only fires on an
		// empty first turn) won't rename it — give it a sensible default derived
		// from the parent.
		if base := strings.TrimSpace(parent.Title); base != "" {
			title = base + " (branch)"
		} else {
			title = "Branch"
		}
	}

	branch, err := s.store.BranchConversation(r.Context(), user, parentConvID, body.BranchPointMessageID, title)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrBranchPointNotFound):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, sql.ErrNoRows):
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSONStatus(w, http.StatusCreated, branch)
}
