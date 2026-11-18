package httpapi

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// handleConversationTransfer serves POST /conversations/{id}/transfer: the
// chat's owner — or an admin, for any chat — hands it to another person
// (docs/CHAT-TRANSFER.md). The transfer is direct: there is no offer for the
// new owner to accept. What keeps that safe is in the store
// (TransferConversation): the chat arrives private, on Manual approval, with
// its patrols paused, so nothing runs on the new owner's connectors until
// they start it.
//
// A caller who is neither the owner nor an admin gets the same 404 as for a
// chat that does not exist.
func (s *Server) handleConversationTransfer(w http.ResponseWriter, r *http.Request, convID, user string) {
	var req struct {
		ToEmail string `json:"to_email"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	ctx := r.Context()
	conv, err := s.store.GetConversationOwnerAndMultiplayer(ctx, convID)
	if err != nil {
		http.Error(w, "transfer failed", http.StatusInternalServerError)
		return
	}
	if conv == nil || (!strings.EqualFold(conv.UserEmail, user) && !s.callerIsAdmin(ctx)) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Everyone who could reach the chat until now, so each of them is
	// reconciled against its new audience below.
	seeds := append([]string{conv.UserEmail}, conv.MultiplayerParticipants...)
	if conv.Multiplayer {
		if roster, rerr := s.store.ListMultiplayerRoster(ctx, conv.UserEmail, convID); rerr == nil {
			seeds = append(seeds, roster...)
		}
	}
	// The store re-checks the owner under the row lock, so a request admitted
	// here cannot move a chat that changed hands in between.
	res, err := s.store.TransferConversation(ctx, conv.UserEmail, convID, req.ToEmail)
	switch {
	case errors.Is(err, store.ErrConversationNotFound):
		http.Error(w, "not found", http.StatusNotFound)
		return
	case errors.Is(err, store.ErrTransferTarget):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case errors.Is(err, store.ErrTransferNotAllowed):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		logging.From(ctx).Error("conversations: transfer failed", "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
		http.Error(w, "transfer failed", http.StatusInternalServerError)
		return
	}
	// The chat is now private to its new owner: stop a turn the previous
	// owner or a participant still has running, drop their queued inputs,
	// pending approvals, session approvals and patrols, and detach their live
	// streams — the same reconciliation a participant's removal gets.
	s.enforceMultiplayerRevocation(ctx, convID, res.Conversation, seeds)
	s.cancelConversationAutoSubmits(ctx, convID)
	// The previous owner's own skills were written into the workspace for
	// their turns; they are theirs, not part of the chat.
	skillsDir := filepath.Join(tools.WorkspaceDirForConversation(convID), userSkillsRoot)
	if err := os.RemoveAll(skillsDir); err != nil { //nolint:gosec // G703: workspace root + the id of a conversation the store just found and moved + a constant directory name — no request-authored path segment
		logging.From(ctx).Warn("conversations: transfer could not clear user skills", "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
	}
	logging.From(ctx).Info("conversations: chat transferred",
		"user", safe.LogSafe(user), "conv_id", safe.LogSafe(convID),
		"from", safe.LogSafe(res.FromEmail), "to", safe.LogSafe(res.ToEmail),
		"patrols_moved", res.PatrolsMoved, "patrols_paused", res.PatrolsPaused)
	writeJSON(w, r, map[string]any{
		"conversation_id": convID,
		"owner_email":     res.ToEmail,
		"previous_owner":  res.FromEmail,
		"patrols_moved":   res.PatrolsMoved,
		"patrols_paused":  res.PatrolsPaused,
	})
}
