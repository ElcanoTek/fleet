package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

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
	// The chat must be idle, and stay idle until the transfer is done. A
	// participant's turn swaps the owner's private subtrees (attachments,
	// personal skills) out of the workspace and back
	// (activateTurnPrivateWorkspace); moving the chat under one could hand
	// those to the new owner when the turn puts them back. So:
	//
	//   - beginTransfer refuses while a turn is running and, under the same
	//     lock registration takes, marks the chat as changing hands: no turn
	//     can register until the deferred end (registerTurnGated).
	//   - the workspace lock is the one a turn, or an approval being
	//     executed, holds from the moment it prepares the workspace until it
	//     has put it back. Holding it here means the last turn's restore has
	//     finished, and nothing else touches the workspace meanwhile.
	endTransfer, idle := s.beginTransfer(convID)
	if !idle {
		http.Error(w, "a turn is running in this chat; stop it or wait for it to finish, then transfer", http.StatusConflict)
		return
	}
	defer endTransfer()
	unlockWorkspace, ok := s.lockWorkspaceWithin(convID, transferWorkspaceWait)
	if !ok {
		http.Error(w, "a turn is running in this chat; stop it or wait for it to finish, then transfer", http.StatusConflict)
		return
	}
	defer unlockWorkspace()
	// A process that died during a participant's turn leaves the workspace
	// half-swapped, with nothing holding the lock after the restart. Put it
	// back the way every turn does before it starts, so what is purged and
	// moved below is the real layout; if that cannot be done, nothing moves.
	if err := s.recoverWorkspaceForTransfer(convID); err != nil {
		logging.From(ctx).Error("conversations: transfer could not recover the workspace", "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
		http.Error(w, "transfer failed", http.StatusInternalServerError)
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
		// Written out, like the other refusals here, so the browser's
		// catalog of known errors has them and shows them translated.
		http.Error(w, "that person can't take this chat: they need an active Fleet account that can chat", http.StatusBadRequest)
		return
	case errors.Is(err, store.ErrTransferNotAllowed):
		http.Error(w, "an archived or Lockdown chat can't be transferred", http.StatusConflict)
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
	// The chat's persistent sandbox, where the deployment keeps one, holds
	// the previous owner's live Python state: variables, imports and threads
	// that were never part of the transcript or the workspace. The new
	// owner's first turn starts a fresh one.
	s.releasePersistentSandbox(convID)
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

// transferWorkspaceWait bounds how long a transfer waits for a turn that has
// just ended, or an approval being executed, to put the workspace back.
var transferWorkspaceWait = 10 * time.Second

// beginTransfer marks convID as changing hands, unless a turn is running in
// it, an approved action is being executed in it, or another transfer of it
// is under way (idle=false). While the mark is
// set, registerTurnGated refuses every turn for the conversation; both read
// and write it under inflightMu, so "no turn is running" and "none can start"
// are one decision. The returned func clears the mark.
func (s *Server) beginTransfer(convID string) (end func(), idle bool) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if entry, ok := s.inflight[convID]; ok && entry.IsRunning() {
		return nil, false
	}
	if s.transferring[convID] {
		return nil, false
	}
	if s.transferring == nil {
		s.transferring = make(map[string]bool)
	}
	s.transferring[convID] = true
	// An approval being decided or an approved action being executed in the
	// chat is as much "not idle" as a turn. Decisions are admitted under
	// this same lock (beginApprovalDecision), so none starts once the mark
	// is set.
	if s.decidingApprovals[convID] > 0 || slices.Contains(s.executingApprovals.convIDs(), convID) {
		delete(s.transferring, convID)
		return nil, false
	}
	return func() {
		s.inflightMu.Lock()
		delete(s.transferring, convID)
		s.inflightMu.Unlock()
	}, true
}

// beginApprovalDecision admits one approval decision for convID (approve,
// reject, expire or dismiss) unless the chat is changing owner, and counts
// it until end is called so a transfer does not begin under it.
func (s *Server) beginApprovalDecision(convID string) (end func(), admitted bool) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.transferring[convID] {
		return nil, false
	}
	if s.decidingApprovals == nil {
		s.decidingApprovals = make(map[string]int)
	}
	s.decidingApprovals[convID]++
	return func() {
		s.inflightMu.Lock()
		defer s.inflightMu.Unlock()
		if s.decidingApprovals[convID]--; s.decidingApprovals[convID] <= 0 {
			delete(s.decidingApprovals, convID)
		}
	}, true
}

// transferInProgress reports whether convID is changing owner right now.
func (s *Server) transferInProgress(convID string) bool {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	return s.transferring[convID]
}

// lockWorkspaceWithin takes the conversation's workspace lock, giving up
// after wait. When it gives up, the lock is released as soon as it is
// eventually acquired.
func (s *Server) lockWorkspaceWithin(convID string, wait time.Duration) (unlock func(), ok bool) {
	got := make(chan func(), 1)
	go func() { got <- s.workspaceLocks.lock(convID) }()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case unlock := <-got:
		return unlock, true
	case <-timer.C:
		go func() { (<-got)() }()
		return nil, false
	}
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
	defer s.workspaceSwapLocks.lock(convID)()
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
// the stash rather than overwritten. The stash and the workspace can be on
// different volumes (the Helm chart puts them on separate claims), so an
// entry that cannot be renamed across is copied and then removed
// (moveAcrossVolumes). Best-effort: a failure is logged.
func (s *Server) promoteParticipantAttachments(ctx context.Context, convID, newOwner string) {
	defer s.workspaceSwapLocks.lock(convID)()
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
		if err := moveAcrossVolumes(filepath.Join(src, e.Name()), to); err != nil {
			logging.From(ctx).Warn("conversations: transfer could not move an attachment of the new owner", "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
		}
	}
}

// moveAcrossVolumes moves the file or directory src to dst, which must not
// exist. A rename is tried first; when it fails (EXDEV, when the two are on
// different filesystems) src is copied beside dst, renamed into place there,
// and only then removed, so dst never holds a partial copy.
func moveAcrossVolumes(src, dst string) error {
	if err := os.Rename(src, dst); err == nil { //nolint:gosec // G703: both paths are server-derived (stash and workspace roots, conversation id, directory entries of the stash)
		return nil
	}
	tmp := dst + ".transfer-tmp"
	_ = os.RemoveAll(tmp) //nolint:gosec // G703: server-derived staging path
	if err := copyDirRecursive(src, tmp); err != nil {
		_ = os.RemoveAll(tmp) //nolint:gosec // G703: server-derived staging path
		return err
	}
	if err := os.Rename(tmp, dst); err != nil { //nolint:gosec // G703: same directory, server-derived
		_ = os.RemoveAll(tmp) //nolint:gosec // G703: server-derived staging path
		return err
	}
	return os.RemoveAll(src) //nolint:gosec // G703: server-derived source in the stash
}

// recoverWorkspaceForTransfer finishes a participant workspace swap that a
// crashed process left behind (recoverInterruptedParticipantWorkspace), under
// the swap lock. The caller holds the conversation's workspace lock.
func (s *Server) recoverWorkspaceForTransfer(convID string) error {
	defer s.workspaceSwapLocks.lock(convID)()
	stash, err := s.privateWorkspaceStashDir(convID)
	if err != nil {
		return err
	}
	return recoverInterruptedParticipantWorkspace(tools.WorkspaceDirForConversation(convID), stash)
}
