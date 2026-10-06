// Conversation branching (#454).
//
// An authenticated owner forks one of their conversations at a chosen message
// (POST /conversations/{id}/branch) into a NEW independent conversation that
// copies the parent's messages up to that point, then diverges. The branch is a
// normal conversation (it appears in the sidebar like any other) with lineage
// metadata recording where it came from; the parent is untouched.

package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/store"
)

// handleConversationBranch forks a conversation the caller can READ at
// branch_point_message_id into a new conversation they own. Body:
//
//	{ "branch_point_message_id": <messages.id>, "title": "optional name" }
//
// Returns 201 with the new conversation. 404 if the parent isn't readable/known,
// 400 for a missing/invalid branch point.
//
// Readable is the caller's own conversation, or a teammate's chat its owner
// shared with the team (ADR-0057) — branching is how a member builds on a
// colleague's thread, and it needs no write access to the original: the fork is
// a copy the brancher owns outright.
func (s *Server) handleConversationBranch(w http.ResponseWriter, r *http.Request, parentConvID, user string) {
	// Confirm the parent exists and is readable before forking (Get is
	// user-scoped, so a foreign/unknown id yields nil). Also gives us the
	// parent title for the default branch name.
	parentTitle := ""
	// shared is set when the parent is a TEAMMATE's chat: the branch then
	// carries the parent's shared outputs as the brancher's own copies.
	var shared *store.TeamSharedConversation
	parent, err := s.store.Get(r.Context(), user, parentConvID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	switch {
	case parent != nil:
		parentTitle = parent.Title
	default:
		var serr error
		shared, serr = s.store.GetTeamVisibleConversation(r.Context(), user, parentConvID)
		if serr != nil {
			http.Error(w, serr.Error(), http.StatusInternalServerError)
			return
		}
		if shared == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		parentTitle = shared.Title
	}

	var body struct {
		BranchPointMessageID int64  `json:"branch_point_message_id"`
		Title                string `json:"title"`
	}
	// Refuse a malformed body outright rather than letting it decode to the
	// zero value and surface as the misleading "must be a positive message id".
	if !decodeOptionalJSONBody(w, r, &body) {
		return
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
		if base := strings.TrimSpace(parentTitle); base != "" {
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
	if shared != nil {
		branch.BranchOrigin = s.carrySharedFilesIntoBranch(r.Context(), shared, branch)
	}
	writeJSONStatus(w, http.StatusCreated, branch)
}

// carrySharedFilesIntoBranch is the file half of a teammate's branch
// (ADR-0079): every output the owner has SHARED at this moment is copied into
// the new branch's workspace at the same relative path — so the links in the
// copied transcript resolve against the brancher's own workspace — and the
// origin is recorded so the branch can say where it came from and the agent
// in it can be told which files it actually has.
//
// The copies are the brancher's from the first byte: later unshares, edits or
// deletions by the owner never reach them. Outputs the owner unchecked are
// recorded as withheld; their names stay in the transcript, as locked names.
// The owner's OWN branch is untouched by this — it already reads their
// workspace through their own history and needs no copy.
//
// Best-effort past the branch itself: the conversation already exists, so a
// failure here is logged and the branch is returned without (some of) its
// files rather than turned into an error the user cannot act on.
func (s *Server) carrySharedFilesIntoBranch(ctx context.Context, src *store.TeamSharedConversation, branch *store.Conversation) *store.BranchOrigin {
	origin := store.BranchOrigin{
		SourceConversationID: src.ID,
		SourceOwnerEmail:     src.OwnerEmail,
		SourceTitle:          src.Title,
		BranchedAt:           branch.CreatedAt,
		CopiedFiles:          []store.BranchFile{},
		WithheldFiles:        []string{},
		SourceStillShared:    true,
	}
	outs, err := s.outputsFromHistory(ctx, src.ID, src.Messages)
	if err != nil {
		log.Printf("branch files: outputs of %s: %v", logSafeSlug(src.ID), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF from the id and the error text.
	} else {
		origin.CopiedFiles, origin.WithheldFiles = copySharedOutputsIntoBranch(src.ID, branch.ID, outs)
	}
	if err := s.store.RecordBranchOrigin(ctx, branch.ID, origin); err != nil {
		log.Printf("branch files: record origin of %s: %v", logSafeSlug(branch.ID), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF from the id and the error text.
	}
	return &origin
}
