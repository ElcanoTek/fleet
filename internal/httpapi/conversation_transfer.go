package httpapi

import (
	"context"
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
	// A running turn has the chat's workspace mid-flight: a participant's
	// turn swaps the owner's private subtrees (attachments, personal skills)
	// out and back (activateTurnPrivateWorkspace). Moving the chat under it
	// could hand those to the new owner when the turn puts them back, so the
	// transfer waits for the chat to be idle. A submission that was admitted
	// before the transfer and registers after it is refused by the ownership
	// epoch bumped below (registerTurnGated); one that registers in the
	// moment before the commit is stopped by the reconciliation.
	if s.turnRunning(convID) {
		http.Error(w, store.ErrTransferBusy.Error(), http.StatusConflict)
		return
	}
	// The previous owner's personal skills are theirs, not part of the chat,
	// and must be gone before the chat is anyone else's: they are removed
	// first, and a failure stops the transfer. Nothing is lost if the
	// transfer is then refused, because skills are written into the workspace
	// again at the start of each of their turns (materializeUserSkills).
	if err := s.purgeOwnerSkills(convID); err != nil {
		logging.From(ctx).Error("conversations: transfer could not clear user skills", "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
		http.Error(w, "transfer failed", http.StatusInternalServerError)
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
	// The transfer is committed. What follows must run even if the request
	// has gone away since, or the previous audience would keep a turn,
	// approvals or a live stream on a chat that is no longer theirs.
	ctx = context.WithoutCancel(ctx)
	// From here no submission admitted under the previous owner may register
	// a turn (registerTurnGated compares this under the same lock).
	s.inflightMu.Lock()
	if s.ownershipEpochs == nil {
		s.ownershipEpochs = make(map[string]uint64)
	}
	s.ownershipEpochs[convID]++
	s.inflightMu.Unlock()
	// The chat is now private to its new owner: stop a turn the previous
	// owner or a participant still has running, drop their queued inputs,
	// pending approvals, session approvals and patrols, and detach their live
	// streams — the same reconciliation a participant's removal gets.
	s.enforceMultiplayerRevocation(ctx, convID, res.Conversation, seeds)
	s.cancelConversationAutoSubmits(ctx, convID)
	// A turn that slipped in before the commit may have written the previous
	// owner's skills again; it has been stopped, so clear them once more.
	if err := s.purgeOwnerSkills(convID); err != nil {
		logging.From(ctx).Warn("conversations: transfer could not clear user skills after the commit", "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
	}
	// A new owner who took part in the chat as a participant has attachments
	// of their own in the participant stash; as the owner, theirs live in the
	// workspace.
	s.promoteParticipantAttachments(ctx, convID, res.ToEmail)
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

// turnRunning reports whether a turn is running in convID right now.
func (s *Server) turnRunning(convID string) bool {
	s.inflightMu.Lock()
	entry, ok := s.inflight[convID]
	s.inflightMu.Unlock()
	return ok && entry.IsRunning()
}

// currentOwnershipEpoch returns how many times this process has transferred
// convID.
func (s *Server) currentOwnershipEpoch(convID string) uint64 {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	return s.ownershipEpochs[convID]
}

// purgeOwnerSkills removes the owner's personal skills from both places a
// conversation keeps them: the workspace, and the stash a participant's turn
// moves the owner's copy to.
func (s *Server) purgeOwnerSkills(convID string) error {
	dirs := []string{filepath.Join(tools.WorkspaceDirForConversation(convID), userSkillsRoot)}
	if stash, err := s.privateWorkspaceStashDir(convID); err == nil {
		dirs = append(dirs, filepath.Join(stash, "owner", userSkillsRoot))
	}
	var firstErr error
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil && firstErr == nil { //nolint:gosec // G703: workspace or stash root + the id of a conversation the store found + constant directory names — no request-authored path segment
			firstErr = err
		}
	}
	return firstErr
}

// promoteParticipantAttachments moves the attachments newOwner uploaded while
// they were a participant of convID out of their participant stash and into
// the workspace, where an owner's attachments live, so links from their
// earlier turns keep resolving. A name the workspace already holds is left in
// the stash rather than overwritten. Best-effort: a failure is logged.
func (s *Server) promoteParticipantAttachments(ctx context.Context, convID, newOwner string) {
	stash, err := s.privateWorkspaceStashDir(convID)
	if err != nil {
		return
	}
	src := filepath.Join(stash, "participants", participantDigest(newOwner), uploadsDir)
	entries, err := os.ReadDir(src)
	if err != nil {
		return // nothing stashed for them
	}
	dst := filepath.Join(tools.WorkspaceDirForConversation(convID), uploadsDir)
	if err := os.MkdirAll(dst, 0o755); err != nil { //nolint:gosec // G301/G703: the conversation's own workspace attachments directory, same mode as the rest of the workspace
		logging.From(ctx).Warn("conversations: transfer could not move the new owner's attachments", "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
		return
	}
	for _, e := range entries {
		to := filepath.Join(dst, e.Name())
		if _, err := os.Lstat(to); err == nil { //nolint:gosec // G703: the conversation's workspace attachments directory + a directory entry name read from the server's own stash
			continue
		}
		if err := os.Rename(filepath.Join(src, e.Name()), to); err != nil { //nolint:gosec // G703: both paths are server-derived (stash and workspace roots, conversation id, directory entries of the stash)
			logging.From(ctx).Warn("conversations: transfer could not move an attachment of the new owner", "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
		}
	}
}
