// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/accountdelete"
	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
)

// defaultAccountDrainTimeout bounds how long deleting an account waits for
// the account's requests and turns to stop once cancelled. Past it the deletion
// goes ahead: a write that still lands afterwards is refused for want of an
// account (store.withAccountRow) or removed by the maintenance pass
// (sweepDeletedAccounts).
const defaultAccountDrainTimeout = 15 * time.Second

// defaultAccountDeleteTimeout bounds an account deletion once the account's
// work is stopped (the database deletion, keys, files, the server's cleanup).
const defaultAccountDeleteTimeout = 2 * time.Minute

// accountDeleteTimeout is defaultAccountDeleteTimeout, or accountDeleteLimit
// (a test seam).
func (s *Server) accountDeleteTimeout() time.Duration {
	if s.accountDeleteLimit > 0 {
		return s.accountDeleteLimit
	}
	return defaultAccountDeleteTimeout
}

// accountDrainTimeout is how long this server waits for an account's work
// to stop (accountDrain, a test seam, or defaultAccountDrainTimeout).
func (s *Server) accountDrainTimeout() time.Duration {
	if s.accountDrain > 0 {
		return s.accountDrain
	}
	return defaultAccountDrainTimeout
}

// stopAccountWork stops what the account has running in this process before
// it is deleted: its requests in flight (s.accounts, already closed by the
// caller, which cancelled them) and the turns it runs — in its own chats, and
// as a participant in other people's. In that order: it waits for the
// requests to end first, so none is still between its access check and its
// turn's registration, then cancels the account's running turns and waits for
// them, then looks once more. No turn can start for the account meanwhile
// (registerTurnChecked refuses one while the gate is closed, under the lock
// the turns are read under), so the second look is a backstop. Everything
// shares one deadline, accountDrainTimeout; it reports whether everything
// stopped in time. It returns early with ctx's error when ctx ends — the
// deleting request was cancelled (its client went away, or the account
// deleting is itself being deleted) — and the caller then deletes nothing,
// though the requests and turns it already cancelled stay cancelled.
func (s *Server) stopAccountWork(ctx context.Context, email string) (stopped bool, err error) {
	email = identity.NormalizeEmail(email)
	deadline := time.Now().Add(s.accountDrainTimeout())
	drained, err := s.accounts.wait(ctx, email, time.Until(deadline))
	if err != nil {
		return false, err
	}
	stopped = drained
	for range 2 {
		ok, err := s.stopAccountTurns(ctx, email, deadline)
		if err != nil {
			return false, err
		}
		stopped = stopped && ok
	}
	return stopped, nil
}

// stopAccountTurns cancels the running turns that are email's (turnBelongsTo)
// and waits for them until deadline, reporting whether they all ended.
func (s *Server) stopAccountTurns(ctx context.Context, email string, deadline time.Time) (bool, error) {
	var turns []string
	for _, convID := range s.runningTurnConvIDs() {
		if s.turnBelongsTo(ctx, convID, email) && s.cancelInflight(convID) {
			turns = append(turns, convID)
		}
	}
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for _, convID := range turns {
		for {
			entry, ok := s.getInflight(convID)
			if !ok || !entry.IsRunning() {
				break
			}
			if !time.Now().Before(deadline) {
				return false, nil
			}
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-tick.C:
			}
		}
	}
	return true, nil
}

// runningTurnConvIDs lists the conversations with a turn running now.
func (s *Server) runningTurnConvIDs() []string {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	out := make([]string, 0, len(s.inflight))
	for id, e := range s.inflight {
		if e.IsRunning() {
			out = append(out, id)
		}
	}
	return out
}

// turnBelongsTo reports whether convID's running turn is email's (already
// normalized): it runs it (as owner or as a multiplayer participant), or the
// chat is email's own and goes with the account.
func (s *Server) turnBelongsTo(ctx context.Context, convID, email string) bool {
	if entry, ok := s.getInflight(convID); ok && entry.steer != nil && identity.NormalizeEmail(entry.steer.user) == email {
		return true
	}
	s.inflightMu.Lock()
	participant := s.activeParticipantWorkspaces[convID]
	s.inflightMu.Unlock()
	if participant != "" && identity.NormalizeEmail(participant) == email {
		return true
	}
	if s.store == nil {
		return false
	}
	conv, err := s.store.GetConversationOwnerAndMultiplayer(ctx, convID)
	return err == nil && conv != nil && identity.NormalizeEmail(conv.UserEmail) == email
}

// accountEmails lists every account's email, whatever its state.
func (s *Server) accountEmails(ctx context.Context) ([]string, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, identity.NormalizeEmail(u.Email))
	}
	return out, nil
}

// sweepDeletedAccounts is the maintenance pass's account step: it removes
// what names an address that no longer has an account — rows in tables only
// a signed-in account writes (store.SweepDeletedAccountRows), upload and
// private workspace trees under the attachment directory, and this process's
// in-memory approvals. It covers accounts deleted before deletion reached
// these, a deletion from the CLI while this server runs (a turn in flight can
// write a private workspace tree back as it ends, and the CLI cannot reach
// this process's memory), and a CLI that resolved another attachment
// directory.
//
// The directories are read first and the accounts listed afterwards (once),
// so an account created while the pass runs is either listed or had no
// directory yet. A tree that looks orphaned is checked once more, for its
// own digest alone, right before it goes (accountdelete.RemoveOrphanTree,
// digestLive), so an account created since under an address whose old tree
// is still there keeps what it wrote; the approvals are judged against a
// listing taken after the files. Upload trees an interrupted removal set
// aside are removed on every pass.
func (s *Server) sweepDeletedAccounts(ctx context.Context) {
	if s.store == nil {
		return
	}
	if sw, ok := s.store.(interface {
		SweepDeletedAccountRows(context.Context) (int64, error)
	}); ok {
		if n, err := sw.SweepDeletedAccountRows(ctx); err != nil {
			logging.From(ctx).Error("maintenance: deleted-account row sweep failed", "err", err)
		} else if n > 0 {
			logging.From(ctx).Info("maintenance: removed rows of deleted accounts", "removed", n)
		}
	}
	var uploads []string
	if s.cfg != nil && s.cfg.EmailAttachmentDir != "" {
		if err := accountdelete.RemoveSetAsideUploads(s.cfg.EmailAttachmentDir); err != nil {
			logging.From(ctx).Error("maintenance: removing set-aside upload trees failed", "err", err)
		}
		var err error
		if uploads, err = accountdelete.UploadDigests(s.cfg.EmailAttachmentDir); err != nil {
			logging.From(ctx).Error("maintenance: reading the uploads directory failed", "err", err)
		}
	}
	stashes := s.participantStashes()
	emails, err := s.accountEmails(ctx)
	if err != nil {
		logging.From(ctx).Error("maintenance: listing accounts failed", "err", err)
		return
	}
	liveDigests := make(map[string]bool, len(emails))
	for _, e := range emails {
		liveDigests[accountdelete.Digest(e)] = true
	}
	if s.afterAccountSnapshot != nil {
		s.afterAccountSnapshot()
	}
	live := s.digestLive(ctx)
	var orphans []string
	for _, d := range uploads {
		if !liveDigests[d] {
			orphans = append(orphans, d)
		}
	}
	if len(orphans) > 0 {
		if n, err := accountdelete.RemoveUploads(s.cfg.EmailAttachmentDir, orphans, live); err != nil {
			logging.From(ctx).Error("maintenance: deleted-account upload sweep failed", "err", err)
		} else if n > 0 {
			logging.From(ctx).Info("maintenance: removed uploads of deleted accounts", "removed", n)
		}
	}
	s.removeOrphanParticipantStashes(ctx, stashes, liveDigests, live)
	emails, err = s.accountEmails(ctx)
	if err != nil {
		logging.From(ctx).Error("maintenance: listing accounts failed", "err", err)
		return
	}
	liveEmails := make(map[string]bool, len(emails))
	for _, e := range emails {
		liveEmails[e] = true
	}
	s.sessionApprovals.ForgetUsersExcept(liveEmails)
}

// digestLive is the sweep's last look before it removes a tree: whether an
// account's email has that one digest now. The store answers it with one
// query that reads no row back (store.AccountDigestLive); a store without
// that query is answered by listing the accounts.
func (s *Server) digestLive(ctx context.Context) func(digest string) (bool, error) {
	if q, ok := s.store.(interface {
		AccountDigestLive(context.Context, string) (bool, error)
	}); ok {
		return func(digest string) (bool, error) { return q.AccountDigestLive(ctx, digest) }
	}
	return func(digest string) (bool, error) {
		emails, err := s.accountEmails(ctx)
		if err != nil {
			return false, err
		}
		for _, e := range emails {
			if accountdelete.Digest(e) == digest {
				return true, nil
			}
		}
		return false, nil
	}
}

// participantStash is one participants/<digest> tree in a conversation's
// private workspace stash; aside marks a tree an interrupted removal left set
// aside (accountdelete.RemoveAside), which is removed whatever its digest.
type participantStash struct {
	convID, name string
	aside        bool
}

// participantStashes lists every participants/<digest> tree in the private
// workspace stashes, and the trees a removal left set aside there.
func (s *Server) participantStashes() []participantStash {
	root, err := s.privateWorkspaceStashRoot()
	if err != nil || root == "" {
		return nil
	}
	convs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []participantStash
	for _, d := range convs {
		convID := d.Name()
		if !d.IsDir() || !filepath.IsLocal(convID) || strings.ContainsAny(convID, `/\`) {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, convID, "participants"))
		if err != nil {
			continue
		}
		for _, e := range entries {
			switch {
			case e.IsDir() && accountdelete.IsDigest(e.Name()):
				out = append(out, participantStash{convID: convID, name: e.Name()})
			case accountdelete.IsAside(e.Name()):
				out = append(out, participantStash{convID: convID, name: e.Name(), aside: true})
			}
		}
	}
	return out
}

// removeOrphanParticipantStashes removes the trees among stashes whose digest
// is no account's: not in liveDigests, and still not live right before the
// removal (accountdelete.RemoveOrphanTree). Each removal holds the
// conversation's swap lock and marks a participant turn still running for
// that digest, so it does not move the tree back as it ends.
func (s *Server) removeOrphanParticipantStashes(ctx context.Context, stashes []participantStash, liveDigests map[string]bool, live func(string) (bool, error)) {
	root, err := s.privateWorkspaceStashRoot()
	if err != nil || root == "" {
		return
	}
	removed := 0
	for _, c := range stashes {
		dir := filepath.Join(root, c.convID, "participants")
		if c.aside {
			if err := os.RemoveAll(filepath.Join(dir, c.name)); err != nil {
				logging.From(ctx).Error("maintenance: removing a set-aside private workspace tree failed", "err", err)
			}
			continue
		}
		if liveDigests[c.name] {
			continue
		}
		unlock := s.workspaceSwapLocks.lock(c.convID)
		s.inflightMu.Lock()
		active := s.activeParticipantWorkspaces[c.convID]
		s.inflightMu.Unlock()
		gone, err := accountdelete.RemoveOrphanTree(filepath.Join(dir, c.name), c.name, live)
		if gone && active != "" && accountdelete.Digest(active) == c.name {
			s.markParticipantWorkspacePurged(c.convID, active)
		}
		if err != nil {
			logging.From(ctx).Error("maintenance: removing private workspace files of a deleted account failed", "err", err)
		} else if gone {
			removed++
		}
		unlock()
	}
	if removed > 0 {
		logging.From(ctx).Info("maintenance: removed private workspace files of deleted accounts", "removed", removed)
	}
}

// logAccountDeleteProblems logs each step of a deletion that did not complete.
func logAccountDeleteProblems(ctx context.Context, email string, problems []accountdelete.Problem) {
	for _, problem := range problems {
		logging.From(ctx).Warn("admin users: deleted, with a step left", "email", safe.LogSafe(email), "problem", safe.LogSafe(problem.String()))
	}
}
