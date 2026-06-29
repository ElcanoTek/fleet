// Conversation folders & labels (#258).
//
// Folders and labels are per-conversation organization metadata stored on the
// conversations row (folder TEXT, labels TEXT[], added by #279). This file adds
// the read/management surface #258 calls for on top of that storage:
//
//   GET  /conversations?folder=&label=  — filter the list (server.go list handler)
//   GET  /folders                        — enumerate the user's folders + counts
//   POST /folders/rename                 — rename a folder (a bulk re-tag)
//
// plus the shared HTTP-layer validation for the folder/label fields a PATCH
// supplies. There is no separate folders table: a folder is just the set of
// conversations naming it, so "create" is implicit (assign a conversation to a
// name) and "rename" is a bulk UPDATE.

package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Folder/label bounds (#258), enforced at the HTTP layer before any store write.
const (
	maxLabelsPerConversation = 10
	maxLabelLen              = 32
	maxFolderLen             = 64
)

// normalizeAndValidateFolderLabels trims and bounds the optional folder + label
// mutations carried by a conversation PATCH (#258). folder is a pointer: nil =
// leave untouched; a pointer to "" = clear to the no-folder bucket (a valid way
// to remove a conversation from its folder). labels, when non-nil, is the full
// replacement set (a non-nil empty slice clears all labels). It mutates *folder
// and the label entries in place so the store persists trimmed values, and
// returns a user-facing error on violation.
func normalizeAndValidateFolderLabels(folder *string, labels []string) error {
	if folder != nil {
		*folder = strings.TrimSpace(*folder)
		if len(*folder) > maxFolderLen {
			return fmt.Errorf("folder must be at most %d characters", maxFolderLen)
		}
	}
	if len(labels) > maxLabelsPerConversation {
		return fmt.Errorf("at most %d labels per conversation", maxLabelsPerConversation)
	}
	for i, l := range labels {
		l = strings.TrimSpace(l)
		if l == "" {
			return fmt.Errorf("labels must be non-empty")
		}
		if len(l) > maxLabelLen {
			return fmt.Errorf("each label must be at most %d characters", maxLabelLen)
		}
		labels[i] = l
	}
	return nil
}

// listFolders handles GET /folders: the user's distinct non-empty folders, each
// with the count of its active conversations (#258).
func (s *Server) listFolders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	folders, err := s.store.ListFolders(r.Context(), userFromCtx(r.Context()))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"folders": folders})
}

// renameFolder handles POST /folders/rename: move every one of the caller's
// conversations from {from} to {to} (#258). Both names are validated/normalized;
// an empty {from} is rejected (there is no named "" folder to rename), while an
// empty {to} is allowed and means "move these conversations back to no folder".
func (s *Server) renameFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.From = strings.TrimSpace(req.From)
	req.To = strings.TrimSpace(req.To)
	if req.From == "" {
		http.Error(w, "from is required", http.StatusBadRequest)
		return
	}
	if len(req.To) > maxFolderLen {
		http.Error(w, fmt.Sprintf("to must be at most %d characters", maxFolderLen), http.StatusBadRequest)
		return
	}
	n, err := s.store.RenameFolder(r.Context(), userFromCtx(r.Context()), req.From, req.To)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"updated": n})
}
