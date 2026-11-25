package httpapi

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/pgtext"
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
	if conv == nil || (!pgtext.SameEmail(conv.UserEmail, user) && !s.callerIsAdmin(ctx)) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	res, err := s.transferConversation(ctx, conv, req.ToEmail)
	if err != nil {
		status := transferErrorStatus(err)
		http.Error(w, failureText(ctx, r, status, err), status)
		return
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
// moves the owner's copy to. The workspace is opened without following a
// symbolic link (the sandbox can replace the conversation directory with one):
// a conversation directory that is a link is refused, which stops the
// transfer, and nothing is removed wherever it points. A conversation that
// never wrote a file has no directory and nothing to remove there.
func (s *Server) purgeOwnerSkills(ctx context.Context, convID string) error {
	defer s.workspaceSwapLocks.lock(convID)()
	var firstErr error
	ws, err := tools.OpenConversationDir(ctx, convID)
	switch {
	case err == nil:
		firstErr = ws.RemoveAll(userSkillsRoot)
		_ = ws.Close()
	case errors.Is(err, fs.ErrNotExist):
	default:
		firstErr = err
	}
	if stash, err := s.privateWorkspaceStashDir(convID); err == nil {
		if err := os.RemoveAll(filepath.Join(stash, "owner", userSkillsRoot)); err != nil && firstErr == nil {
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
// (moveAcrossVolumes).
//
// The conversation directory is opened without following a symbolic link,
// and every move goes through the opened directories. A symbolic link at
// attachments/ holds no attachment data and is replaced with a real directory
// before anything moves, as a turn's private-directory swap does. If the
// conversation directory itself is a symbolic link, nothing moves: the files
// stay in the participant stash, which nothing reads for an owner, so the new
// owner does not see them; they are removed with the stash when the chat is
// deleted. Best-effort: a failure is logged (the refusal of a linked
// conversation directory by the opener, every other failure here).
func (s *Server) promoteParticipantAttachments(ctx context.Context, convID, newOwner string) {
	defer s.workspaceSwapLocks.lock(convID)()
	stash, err := s.privateWorkspaceStashDir(convID)
	if err != nil {
		return
	}
	src, err := openStashDirIfPresent(filepath.Join(stash, "participants", participantDigest(newOwner), uploadsDir))
	if err != nil || src == nil {
		return // nothing stashed for them
	}
	defer src.Close()
	listing, err := src.Open(".")
	if err != nil {
		return
	}
	entries, err := listing.ReadDir(-1)
	_ = listing.Close()
	if err != nil {
		return
	}
	warn := func(msg string, err error) {
		logging.From(ctx).Warn(msg, "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
	}
	ws, err := tools.EnsureConversationDir(ctx, convID)
	if err != nil {
		if !errors.Is(err, tools.ErrWorkspaceDirLink) { // a linked conversation directory is logged by the opener
			warn("conversations: transfer could not move the new owner's attachments", err)
		}
		return
	}
	defer ws.Close()
	dst, err := openRealSubdir(ws.Root, uploadsDir)
	if err != nil {
		warn("conversations: transfer could not move the new owner's attachments", err)
		return
	}
	defer dst.Close()
	for _, e := range entries {
		if _, err := dst.Lstat(e.Name()); err == nil {
			continue
		}
		if err := moveAcrossVolumes(src, e.Name(), dst, e.Name()); err != nil {
			warn("conversations: transfer could not move an attachment of the new owner", err)
		}
	}
}

// openRealSubdir opens the directory name inside parent, creating it (0o755,
// the mode of the rest of the workspace) if it is missing and replacing a
// symbolic link found there — a link holds no data — with a real directory.
// The directory is opened without following a link, so if one is put back in
// between, the open is refused rather than followed.
func openRealSubdir(parent *os.Root, name string) (*os.Root, error) {
	if info, err := parent.Lstat(name); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		if err := parent.Remove(name); err != nil {
			return nil, err
		}
	}
	if err := parent.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return tools.OpenSubdirNoFollow(parent, name)
}

// moveAcrossVolumes moves the file or directory srcName inside src to dstName
// inside dst, which must not exist; both are directories the caller opened,
// and both names are single path components. A rename is tried first; when it
// fails (EXDEV, when the two are on different filesystems) the entry is copied
// beside dstName (copyEntryBetween, no link followed), renamed into place
// there, and only then removed from src, so dstName never holds a partial
// copy.
func moveAcrossVolumes(src *os.Root, srcName string, dst *os.Root, dstName string) error {
	if err := moveRename(src, srcName, dst, dstName); err == nil {
		return nil
	}
	tmp := dstName + ".transfer-tmp"
	_ = dst.RemoveAll(tmp)
	if err := copyEntryBetween(src, srcName, dst, tmp); err != nil {
		_ = dst.RemoveAll(tmp)
		return err
	}
	if err := dst.Rename(tmp, dstName); err != nil {
		_ = dst.RemoveAll(tmp)
		return err
	}
	return src.RemoveAll(srcName)
}

// moveRename is the rename moveAcrossVolumes tries first (renameBetween); a
// test replaces it to run the copy that follows a rename across filesystems.
var moveRename = renameBetween

// recoverWorkspaceForTransfer finishes a participant workspace swap that a
// crashed process left behind (recoverInterruptedParticipantWorkspace), under
// the swap lock. The caller holds the conversation's workspace lock.
func (s *Server) recoverWorkspaceForTransfer(convID string) error {
	defer s.workspaceSwapLocks.lock(convID)()
	stash, err := s.privateWorkspaceStashDir(convID)
	if err != nil {
		return err
	}
	return recoverInterruptedParticipantWorkspace(convID, stash, s.stashOwner)
}

// transferConversation moves an idle chat to toEmail and reconciles everyone
// who could reach it before (the owner's transfer route, and an
// administrator's transfer request, ADR-0115). conv is the chat as the
// caller read it (GetConversationOwnerAndMultiplayer) when it decided the
// transfer may happen; the store re-checks its owner under the row lock, so
// a chat that changed hands since is not moved. It returns one of the
// errTransfer* refusals when the transfer did not happen.
func (s *Server) transferConversation(ctx context.Context, conv *store.Conversation, toEmail string) (*store.ConversationTransfer, error) {
	convID := conv.ID
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
		return nil, errTransferBusy
	}
	defer endTransfer()
	unlockWorkspace, ok := s.lockWorkspaceWithin(convID, transferWorkspaceWait)
	if !ok {
		return nil, errTransferBusy
	}
	defer unlockWorkspace()
	// A process that died during a participant's turn leaves the workspace
	// half-swapped, with nothing holding the lock after the restart. Put it
	// back the way every turn does before it starts, so what is purged and
	// moved below is the real layout; if that cannot be done, nothing moves.
	if err := s.recoverWorkspaceForTransfer(convID); err != nil {
		logging.From(ctx).Error("conversations: transfer could not recover the workspace", "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
		return nil, errTransferFailed
	}
	// The previous owner's personal skills are theirs, not part of the chat,
	// and must be gone before the chat is anyone else's: they are removed
	// first, and a failure stops the transfer. Nothing is lost if the
	// transfer is then refused, because skills are written into the workspace
	// again at the start of each of their turns (materializeUserSkills).
	if err := s.purgeOwnerSkills(ctx, convID); err != nil {
		logging.From(ctx).Error("conversations: transfer could not clear user skills", "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
		return nil, errTransferFailed
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
	res, err := s.store.TransferConversation(ctx, conv.UserEmail, convID, toEmail)
	switch {
	case errors.Is(err, store.ErrConversationNotFound):
		return nil, errTransferNotFound
	case errors.Is(err, store.ErrTransferTarget):
		return nil, errTransferTarget
	case errors.Is(err, store.ErrTransferNotAllowed):
		return nil, errTransferNotAllowed
	case err != nil:
		logging.From(ctx).Error("conversations: transfer failed", "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
		return nil, errTransferFailed
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
	return res, nil
}

// The transfer refusals, written out as errors.New so the browser's catalog
// of known server errors has them and shows them translated.
var (
	errTransferBusy       = errors.New("a turn is running in this chat; stop it or wait for it to finish, then transfer")
	errTransferFailed     = errors.New("transfer failed")
	errTransferNotFound   = errors.New("not found")
	errTransferTarget     = errors.New("that person can't take this chat: they need an active Fleet account that can chat")
	errTransferNotAllowed = errors.New("an archived or Lockdown chat can't be transferred")
)

// transferErrorStatus is the HTTP status of a transfer refusal.
func transferErrorStatus(err error) int {
	switch {
	case errors.Is(err, errTransferBusy), errors.Is(err, errTransferNotAllowed):
		return http.StatusConflict
	case errors.Is(err, errTransferNotFound):
		return http.StatusNotFound
	case errors.Is(err, errTransferTarget):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
