// Per-conversation workspace file fetch:
// GET /conversations/{id}/workspace/<path>. Split out of server.go (#1127).

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ElcanoTek/fleet/internal/accountdelete"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// handleWorkspaceFile streams a single file from the per-conversation
// workspace dir so the chat UI can render images / files the agent
// produced via run_python or wrote with write_file. Used by the
// markdown img interceptor in chat-experience.tsx — when the agent
// writes `![chart](spend_chart.png)` and saves spend_chart.png to its
// workspace, the UI rewrites the relative src to
// `/api/conversations/<convID>/workspace/spend_chart.png` and the
// browser fetches it from this handler.
//
// Auth: same as every other conversation route — the caller must own
// the conversation. The conversation directory is opened without
// following a symbolic link (a link there gets the same 404 as a missing
// file), relPath is resolved inside it — .. traversal and absolute paths
// are rejected, and a symlink is followed only while it stays inside the
// directory — and the file is opened with no link followed at any
// component, so nothing outside the workspace can be served.
func (s *Server) handleWorkspaceFile(w http.ResponseWriter, r *http.Request, convID, relPath string) {
	user := userFromCtx(r.Context())
	conv, ok := s.conversationOr404(w, r, user, convID, true, "conversation not found")
	if !ok {
		return
	}

	if relPath == "" {
		http.Error(w, "file path required", http.StatusBadRequest)
		return
	}
	// relPath comes from r.URL.Path, which net/http has ALREADY
	// percent-decoded once (spaces, parens, unicode in pandas/matplotlib
	// filenames arrive as themselves). It must not be decoded a second time:
	// that turned a literal `%` in an agent-written filename into a "bad
	// path encoding" 400 (`100%.png`) or a lookup of the wrong name
	// (`rate%20final.csv` → `rate final.csv` → 404).

	// Resolve the requested file inside the conversation workspace with the
	// shared guard (tools.OpenConversationDir + WorkspaceDir.Resolve): a
	// conversation directory that is a symbolic link is refused, `..`,
	// absolute and NUL paths are rejected up front, and a symlink is followed
	// only while it stays inside the directory, so a `ln -s /etc/passwd p`
	// the agent wrote answers 400 rather than serving a host file.
	isOwner := strings.EqualFold(conv.UserEmail, user) || conv.UserEmail == ""
	swapUnlock := s.workspaceSwapLocks.lock(convID)
	swapUnlocked := false
	releaseSwap := func() {
		if !swapUnlocked {
			swapUnlocked = true
			swapUnlock()
		}
	}
	defer releaseSwap()
	// A chat that has changed owner in this process: the access check above
	// may have been made just before the transfer, and the transfer moves
	// private files around under this same lock (docs/CHAT-TRANSFER.md). So
	// the check is made again now that the lock is held, and what is served
	// is decided by who owns the chat now.
	if s.currentOwnershipEpoch(convID) > 0 {
		if conv, ok = s.conversationOr404(w, r, user, convID, true, "conversation not found"); !ok {
			return
		}
		isOwner = strings.EqualFold(conv.UserEmail, user) || conv.UserEmail == ""
	}
	rawWsDir := tools.WorkspaceDirForConversation(convID)
	baseRawDir := rawWsDir
	cleanRel := filepath.ToSlash(filepath.Clean(relPath))
	participantActive := s.isParticipantWorkspaceActive(convID, conv.UserEmail)
	callerIsActiveParticipant := !isOwner && s.isActiveParticipantUser(convID, user)
	if isPrivateWorkspacePath(cleanRel) {
		if isOwner {
			if participantActive {
				stashRoot, serr := s.privateWorkspaceStashDir(convID)
				if serr != nil {
					http.Error(w, "file not found", http.StatusNotFound)
					return
				}
				baseRawDir = filepath.Join(stashRoot, "owner")
			}
		} else if !callerIsActiveParticipant {
			if cleanRel != uploadsDir && !strings.HasPrefix(cleanRel, uploadsDir+"/") {
				http.Error(w, "file not found", http.StatusNotFound)
				return
			}
			stashRoot, serr := s.privateWorkspaceStashDir(convID)
			if serr != nil {
				http.Error(w, "file not found", http.StatusNotFound)
				return
			}
			baseRawDir = filepath.Join(stashRoot, "participants", participantDigest(user))
		}
	}
	// The conversation directory is opened without following a symbolic link
	// (the sandbox can rename it and leave one in its place), and every later
	// step goes through that open directory. A link there gets the same 404 as
	// a missing file. The private stash is host-only, so it opens as given.
	var wsDir *tools.WorkspaceDir
	var err error
	if baseRawDir == rawWsDir {
		wsDir, err = tools.OpenConversationDir(r.Context(), convID)
	} else {
		wsDir, err = tools.OpenHostDir(baseRawDir)
	}
	if err != nil {
		switch {
		case errors.Is(err, tools.ErrWorkspaceDirLink):
			// Logged once, rate-limited, by the opener.
			http.Error(w, "file not found", http.StatusNotFound)
		case baseRawDir != rawWsDir:
			http.Error(w, "file not found", http.StatusNotFound)
		default:
			http.Error(w, "workspace unavailable", http.StatusNotFound)
		}
		return
	}
	defer wsDir.Close()
	// Resolve the request inside the opened directory: `..`/absolute/NUL paths
	// are rejected up front, links are followed only while they stay inside it
	// (so a `ln -s /etc/passwd p` the agent wrote is refused), and the result is
	// the canonical name the owner-private check below judges.
	rel, err := wsDir.Resolve(relPath)
	switch {
	case errors.Is(err, tools.ErrUnsafePath):
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	case errors.Is(err, tools.ErrPathEscapesWorkspace):
		http.Error(w, "path escapes workspace", http.StatusBadRequest)
		return
	case os.IsNotExist(err):
		http.Error(w, "file not found", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "resolve path: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if baseRawDir == rawWsDir && isPrivateWorkspacePath(filepath.ToSlash(rel)) {
		allowedInWs := (isOwner && !participantActive) || callerIsActiveParticipant
		if !allowedInWs {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
	}
	// The canonical name has no link in it; open it refusing a link at any
	// component, so nothing the sandbox swaps in after the resolution is
	// followed (and a FIFO cannot hang the request). The type is judged on
	// the opened descriptor.
	if rel == "." {
		releaseSwap()
		http.Error(w, "path is not a regular file", http.StatusBadRequest)
		return
	}
	f, info, err := tools.OpenNoFollow(wsDir.Root, rel)
	releaseSwap()
	switch {
	case errors.Is(err, tools.ErrNotRegularFile):
		http.Error(w, "path is not a regular file", http.StatusBadRequest)
		return
	case err != nil:
		http.Error(w, "file unavailable", http.StatusNotFound)
		return
	}
	defer f.Close()
	resolvedAbs := filepath.Join(wsDir.Path(), rel)

	// Content-Type from extension; mime.TypeByExtension handles the
	// common suffixes (.png/.jpg/.jpeg/.svg/.pdf/.csv/.json/.txt) and
	// returns "" for unknown — http.ServeContent's sniffing then takes
	// over. Either way the browser gets something it can render or
	// download.
	ctype := mime.TypeByExtension(filepath.Ext(resolvedAbs))
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	// Agents can overwrite a filename on later turns. Keep private browser
	// caching, but revalidate before reuse so a corrected chart or download
	// cannot remain stale for a day at the same URL.
	w.Header().Set("Cache-Control", "private, no-cache")

	http.ServeContent(w, r, filepath.Base(resolvedAbs), info.ModTime(), f)
}

// errWorkspaceUnavailable is the fixed text a browser gets when the
// conversation workspace cannot be prepared; the detail, which can name host
// paths, goes to the log.
var errWorkspaceUnavailable = errors.New("workspace unavailable")

// participantDigest names a participant's private workspace tree: the one
// digest account deletion and its maintenance sweep remove by
// (accountdelete.Digest).
func participantDigest(userEmail string) string {
	return accountdelete.Digest(userEmail)
}

func (s *Server) privateWorkspaceStashRoot() (string, error) {
	if s.cfg != nil {
		if base := strings.TrimSpace(s.cfg.EmailAttachmentDir); base != "" {
			abs, err := filepath.Abs(base)
			if err != nil {
				return "", err
			}
			return accountdelete.PrivateWorkspaceRoot(filepath.Clean(abs)), nil
		}
	}
	wsRoot, err := filepath.Abs(tools.WorkspaceDirForConversation(""))
	if err != nil {
		return "", err
	}
	wsRoot = filepath.Clean(wsRoot)
	return filepath.Join(filepath.Dir(wsRoot), ".fleet-private-"+filepath.Base(wsRoot)), nil
}

// privateWorkspaceStashDir returns the host-only control-plane directory used
// to isolate per-sender private workspace trees (attachments/ and user-skills/)
// outside the conversation workspace mounted into sandboxes.
func (s *Server) privateWorkspaceStashDir(convID string) (string, error) {
	if convID == "" || !filepath.IsLocal(convID) || strings.ContainsAny(convID, `/\`) {
		return "", errors.New("invalid conversation id")
	}
	root, err := s.privateWorkspaceStashRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, convID), nil
}

func (s *Server) removeConversationPrivateStash(convID string) {
	stashDir, err := s.privateWorkspaceStashDir(convID)
	if err != nil {
		return
	}
	_ = os.RemoveAll(stashDir)
}

func (s *Server) purgeParticipantPrivateStashes(userEmail string) {
	userEmail = strings.ToLower(strings.TrimSpace(userEmail))
	if userEmail == "" {
		return
	}
	s.inflightMu.Lock()
	for convID, activeUser := range s.activeParticipantWorkspaces {
		if strings.EqualFold(activeUser, userEmail) {
			if s.purgedParticipantWorkspaces == nil {
				s.purgedParticipantWorkspaces = make(map[string]bool)
			}
			s.purgedParticipantWorkspaces[convID+"\x00"+userEmail] = true
		}
	}
	s.inflightMu.Unlock()
	root, err := s.privateWorkspaceStashRoot()
	if err != nil || root == "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	digest := participantDigest(userEmail)
	for _, d := range entries {
		if !d.IsDir() {
			continue
		}
		convID := d.Name()
		if !filepath.IsLocal(convID) || strings.ContainsAny(convID, `/\`) {
			continue
		}
		unlock := s.workspaceSwapLocks.lock(convID)
		if s.isActiveParticipantUser(convID, userEmail) {
			s.markParticipantWorkspacePurged(convID, userEmail)
		}
		_ = os.RemoveAll(filepath.Join(root, convID, "participants", digest)) //nolint:gosec // server-derived stash path
		unlock()
	}
}

func (s *Server) markParticipantWorkspacePurged(convID, userEmail string) {
	userKey := strings.ToLower(strings.TrimSpace(userEmail))
	if convID == "" || userKey == "" {
		return
	}
	s.inflightMu.Lock()
	if s.purgedParticipantWorkspaces == nil {
		s.purgedParticipantWorkspaces = make(map[string]bool)
	}
	s.purgedParticipantWorkspaces[convID+"\x00"+userKey] = true
	s.inflightMu.Unlock()
}

func (s *Server) takeParticipantWorkspacePurged(convID, userEmail string) bool {
	userKey := strings.ToLower(strings.TrimSpace(userEmail))
	if convID == "" || userKey == "" {
		return false
	}
	k := convID + "\x00" + userKey
	s.inflightMu.Lock()
	purged := s.purgedParticipantWorkspaces[k]
	delete(s.purgedParticipantWorkspaces, k)
	s.inflightMu.Unlock()
	return purged
}

func (s *Server) isActiveParticipantUser(convID, userEmail string) bool {
	userKey := strings.ToLower(strings.TrimSpace(userEmail))
	if convID == "" || userKey == "" {
		return false
	}
	s.inflightMu.Lock()
	active := s.activeParticipantWorkspaces[convID]
	s.inflightMu.Unlock()
	return active != "" && strings.EqualFold(active, userKey)
}

func (s *Server) setParticipantWorkspaceActive(convID, user string, unlock func()) {
	s.inflightMu.Lock()
	if user != "" {
		if s.activeParticipantWorkspaces == nil {
			s.activeParticipantWorkspaces = make(map[string]string)
		}
		s.activeParticipantWorkspaces[convID] = user
	}
	if unlock != nil {
		if s.activeWorkspaceUnlocks == nil {
			s.activeWorkspaceUnlocks = make(map[string]func())
		}
		s.activeWorkspaceUnlocks[convID] = unlock
	}
	s.inflightMu.Unlock()
}

func (s *Server) clearParticipantWorkspaceActive(convID string) {
	s.inflightMu.Lock()
	delete(s.activeParticipantWorkspaces, convID)
	unlock := s.activeWorkspaceUnlocks[convID]
	delete(s.activeWorkspaceUnlocks, convID)
	s.inflightMu.Unlock()
	if unlock != nil {
		unlock()
	}
}

func (s *Server) isParticipantWorkspaceActiveInMemory(convID, ownerEmail string) bool {
	s.inflightMu.Lock()
	user := s.activeParticipantWorkspaces[convID]
	s.inflightMu.Unlock()
	if user != "" && (ownerEmail == "" || !strings.EqualFold(user, ownerEmail)) {
		return true
	}
	if entry, ok := s.getInflight(convID); ok && entry.IsRunning() && entry.steer != nil && entry.steer.user != "" && !strings.EqualFold(entry.steer.user, ownerEmail) {
		return true
	}
	return false
}

func (s *Server) sweepOrphanPrivateWorkspaceStashes(ctx context.Context) {
	root, err := s.privateWorkspaceStashRoot()
	if err != nil || root == "" {
		return
	}
	if s.store != nil {
		_, _ = s.store.SweepOrphanWorkspaces(ctx, root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, d := range entries {
		if !d.IsDir() {
			continue
		}
		convID := d.Name()
		if !filepath.IsLocal(convID) || strings.ContainsAny(convID, `/\`) {
			continue
		}
		if s.isParticipantWorkspaceActiveInMemory(convID, "") {
			continue
		}
		_ = recoverInterruptedParticipantWorkspace(convID, filepath.Join(root, convID))
	}
}

func (s *Server) isParticipantWorkspaceActive(convID, ownerEmail string) bool {
	if ownerEmail == "" {
		return false
	}
	if s.isParticipantWorkspaceActiveInMemory(convID, ownerEmail) {
		return true
	}
	stashRoot, err := s.privateWorkspaceStashDir(convID)
	if err != nil {
		return false
	}
	_, activeErr := os.Stat(filepath.Join(stashRoot, "active-participant"))
	_, phaseErr := os.Stat(filepath.Join(stashRoot, "swap-phase"))
	if activeErr != nil && phaseErr != nil {
		return false
	}
	// No participant turn is active in memory, so any active-participant or
	// swap-phase marker left on disk came from an interrupted earlier process.
	// Recover the owner's private workspace directories immediately before
	// serving the request; fail closed if recovery cannot complete.
	if err := recoverInterruptedParticipantWorkspace(convID, stashRoot); err != nil {
		return true
	}
	return false
}

// activateTurnPrivateWorkspace isolates privateWorkspaceDirs (attachments/ and
// user-skills/) for the active turn's sender before attachments or user skills
// are staged into the conversation workspace. When a non-owner participant
// runs a turn, the owner's attachments/ and user-skills/ are moved out of the
// mounted workspace into the host-only control-plane stash and replaced with
// the participant's own staged attachments (if any from an earlier turn).
// Returns a non-nil error if isolation cannot be confirmed so the turn aborts
// rather than launching with another user's private workspace files present.
func (s *Server) activateTurnPrivateWorkspace(conv *store.Conversation, user string) error {
	if conv == nil || conv.ID == "" || !filepath.IsLocal(conv.ID) || strings.ContainsAny(conv.ID, `/\`) {
		return errors.New("invalid conversation id")
	}
	unlock := s.workspaceLocks.lock(conv.ID)
	swapUnlock := s.workspaceSwapLocks.lock(conv.ID)
	defer swapUnlock()
	stashRoot, err := s.privateWorkspaceStashDir(conv.ID)
	if err != nil {
		unlock()
		return err
	}
	if err := recoverInterruptedParticipantWorkspace(conv.ID, stashRoot); err != nil {
		unlock()
		return err
	}
	if conv.UserEmail == "" || strings.EqualFold(conv.UserEmail, user) {
		s.setParticipantWorkspaceActive(conv.ID, "", unlock)
		return nil
	}
	_ = s.takeParticipantWorkspacePurged(conv.ID, user)
	// The conversation directory is opened without following a symbolic link
	// and every move below goes through that open directory: the sandbox can
	// rename the directory and leave a link in its place, and the owner's
	// private files must neither be moved into wherever it points nor be
	// replaced by what is there.
	ws, err := tools.EnsureConversationDir(context.Background(), conv.ID)
	if err != nil {
		unlock()
		return err
	}
	defer ws.Close()
	ownerStash, err := openStashDir(filepath.Join(stashRoot, "owner"))
	if err != nil {
		unlock()
		return err
	}
	defer ownerStash.Close()
	phaseFile := filepath.Join(stashRoot, "swap-phase")
	if err := os.WriteFile(phaseFile, []byte("stashing-owner"), 0o600); err != nil {
		unlock()
		return err
	}
	s.setParticipantWorkspaceActive(conv.ID, user, unlock)
	for _, dirName := range privateWorkspaceDirs {
		if err := movePrivateDirIfPresent(ws.Root, dirName, ownerStash, dirName); err != nil {
			_ = rollbackOwnerStash(ws.Root, stashRoot)
			s.clearParticipantWorkspaceActive(conv.ID)
			return fmt.Errorf("stash owner %s: %w", dirName, err)
		}
	}
	if err := writeStashMarker(stashRoot, "active-participant", strings.ToLower(strings.TrimSpace(user))); err != nil {
		_ = rollbackOwnerStash(ws.Root, stashRoot)
		s.clearParticipantWorkspaceActive(conv.ID)
		return err
	}
	_ = os.Remove(phaseFile)
	if err := restoreParticipantAttachments(ws.Root, stashRoot, user); err != nil {
		_ = restoreParticipantWorkspaceToOwner(ws.Root, stashRoot, user)
		s.clearParticipantWorkspaceActive(conv.ID)
		return fmt.Errorf("restore participant attachments: %w", err)
	}
	return nil
}

// restoreTurnPrivateWorkspace removes a non-owner participant's materialized
// user-skills/, stashes their staged attachments/ outside the mounted
// workspace, and restores the conversation owner's attachments/ and
// user-skills/ before the turn lock is released. A conversation directory that
// has become a symbolic link is not followed: the owner's files then stay in
// the host-only stash, and the next activation retries the restore.
func (s *Server) restoreTurnPrivateWorkspace(conv *store.Conversation, user string) {
	if conv == nil || conv.ID == "" || !filepath.IsLocal(conv.ID) || strings.ContainsAny(conv.ID, `/\`) {
		return
	}
	swapUnlock := s.workspaceSwapLocks.lock(conv.ID)
	defer swapUnlock()
	if conv.UserEmail != "" && !strings.EqualFold(conv.UserEmail, user) {
		if stashRoot, err := s.privateWorkspaceStashDir(conv.ID); err == nil {
			if ws, werr := tools.EnsureConversationDir(context.Background(), conv.ID); werr == nil {
				_ = restoreParticipantWorkspaceToOwner(ws.Root, stashRoot, user)
				_ = ws.Close()
			}
			if s.takeParticipantWorkspacePurged(conv.ID, user) {
				_ = os.RemoveAll(filepath.Join(stashRoot, "participants", participantDigest(user))) //nolint:gosec,nolintlint // server-derived stash path; gosec flags this on some analyses and not others (see PR #1917), so the unused-directive check is waived here
			}
		}
	}
	s.clearParticipantWorkspaceActive(conv.ID)
}

func writeStashMarker(stashRoot, name, val string) error {
	root, err := os.OpenRoot(stashRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.WriteFile(name, []byte(val), 0o600)
}

// recoverInterruptedParticipantWorkspace finishes a swap an earlier process
// left half done. The conversation directory is opened (created if missing,
// never followed if it is a symbolic link) only when there is something to
// move back into it.
func recoverInterruptedParticipantWorkspace(convID, stashRoot string) error {
	marker := filepath.Join(stashRoot, "active-participant")
	raw, err := os.ReadFile(marker) //nolint:gosec // server-derived stashRoot
	if err == nil {
		ws, werr := tools.EnsureConversationDir(context.Background(), convID)
		if werr != nil {
			return werr
		}
		defer ws.Close()
		return restoreParticipantWorkspaceToOwner(ws.Root, stashRoot, strings.TrimSpace(string(raw)))
	}
	if !os.IsNotExist(err) {
		return err
	}
	// If active-participant is absent, any swap interrupted during
	// "stashing-owner" (before participant files entered the workspace) or
	// during "restoring-owner" (after participant files were already
	// cleared/stashed) only has owner files in the workspace and/or ownerStash.
	// Nothing to recover — no phase marker and no owner files waiting in the
	// stash — means the conversation directory is not opened at all (an
	// empty owner/ left by an earlier version is removed).
	_, phaseErr := os.Stat(filepath.Join(stashRoot, "swap-phase"))
	ownerDir := filepath.Join(stashRoot, "owner")
	ownerEntries, ownerErr := os.ReadDir(ownerDir)
	if phaseErr != nil && (errors.Is(ownerErr, fs.ErrNotExist) || (ownerErr == nil && len(ownerEntries) == 0)) {
		if ownerErr == nil {
			_ = os.Remove(ownerDir) // only if empty
		}
		return nil
	}
	// owner/ has entries, or cannot be read: the rollback below moves them
	// back, or fails and the caller is told.
	ws, err := tools.EnsureConversationDir(context.Background(), convID)
	if err != nil {
		return err
	}
	defer ws.Close()
	return rollbackOwnerStash(ws.Root, stashRoot)
}

// restoreParticipantAttachments moves a participant's earlier staged
// attachments from the stash into the opened conversation directory.
func restoreParticipantAttachments(ws *os.Root, stashRoot, participantEmail string) error {
	part, err := openStashDirIfPresent(filepath.Join(stashRoot, "participants", participantDigest(participantEmail)))
	if err != nil || part == nil {
		return err
	}
	defer part.Close()
	return movePrivateDirIfPresent(part, uploadsDir, ws, uploadsDir)
}

func restoreParticipantWorkspaceToOwner(ws *os.Root, stashRoot, participantEmail string) error {
	if err := ws.RemoveAll(userSkillsRoot); err != nil {
		return err
	}
	if participantEmail != "" {
		part, err := openStashDir(filepath.Join(stashRoot, "participants", participantDigest(participantEmail)))
		if err != nil {
			return err
		}
		err = movePrivateDirIfPresent(ws, uploadsDir, part, uploadsDir)
		_ = part.Close()
		if err != nil {
			return err
		}
	} else if err := ws.RemoveAll(uploadsDir); err != nil {
		return err
	}
	phaseFile := filepath.Join(stashRoot, "swap-phase")
	_ = os.WriteFile(phaseFile, []byte("restoring-owner"), 0o600)
	if err := os.Remove(filepath.Join(stashRoot, "active-participant")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return rollbackOwnerStash(ws, stashRoot)
}

func rollbackOwnerStash(ws *os.Root, stashRoot string) error {
	ownerStash, err := openStashDirIfPresent(filepath.Join(stashRoot, "owner"))
	if err != nil {
		return err
	}
	if ownerStash != nil {
		defer ownerStash.Close()
		for _, dirName := range privateWorkspaceDirs {
			if _, err := ownerStash.Lstat(dirName); err != nil {
				continue
			}
			if _, err := ws.Lstat(dirName); !os.IsNotExist(err) {
				continue
			}
			if err := movePrivateDirIfPresent(ownerStash, dirName, ws, dirName); err != nil {
				return err
			}
		}
	}
	if err := os.Remove(filepath.Join(stashRoot, "active-participant")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(filepath.Join(stashRoot, "swap-phase")); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Leave no empty owner/ behind: an empty one would send every later
	// recovery check into opening the conversation directory for nothing.
	_ = os.Remove(filepath.Join(stashRoot, "owner")) // only if empty
	return nil
}

// openStashDir creates (if missing) and opens a directory of the host-only
// private stash. No sandbox mounts the stash, so its path is opened as given.
func openStashDir(dir string) (*os.Root, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // server-derived stash path
		return nil, err
	}
	return os.OpenRoot(dir)
}

// openStashDirIfPresent opens a stash directory, or returns nil when it does
// not exist.
func openStashDirIfPresent(dir string) (*os.Root, error) {
	r, err := os.OpenRoot(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return r, err
}

// movePrivateDirIfPresent moves the directory srcName inside src to dstName
// inside dst, replacing whatever dst held there. Both names are single path
// components, and each side is a directory already opened by its caller —
// the conversation side without following a symbolic link — so the move can
// land nowhere else. A non-directory at srcName (a file, or a symbolic link
// the sandbox planted) is removed, never followed or moved. Across
// filesystems the tree is copied (no link followed) and then renamed into
// place.
func movePrivateDirIfPresent(src *os.Root, srcName string, dst *os.Root, dstName string) error {
	info, err := src.Lstat(srcName)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return src.RemoveAll(srcName)
	}
	if err := dst.RemoveAll(dstName); err != nil {
		return err
	}
	if err := renameBetween(src, srcName, dst, dstName); err == nil {
		return nil
	}
	tmpDst := dstName + ".tmp"
	_ = dst.RemoveAll(tmpDst)
	if err := copyDirBetween(src, srcName, dst, tmpDst); err != nil {
		_ = dst.RemoveAll(tmpDst) // clean up partial copy without touching dst or src
		return err
	}
	if err := dst.Rename(tmpDst, dstName); err != nil {
		_ = dst.RemoveAll(tmpDst)
		return err
	}
	return src.RemoveAll(srcName)
}

// renameBetween renames srcName in src to dstName in dst with renameat(2) on
// the two open directories. Neither name's last component is followed.
func renameBetween(src *os.Root, srcName string, dst *os.Root, dstName string) error {
	sf, err := src.Open(".")
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := dst.Open(".")
	if err != nil {
		return err
	}
	defer df.Close()
	return unix.Renameat(int(sf.Fd()), srcName, int(df.Fd()), dstName)
}

// copyEntryBetween copies srcName inside src — a regular file or a directory
// tree — to dstName inside dst (copyFileBetween, copyDirBetween). Anything
// else, a symbolic link included, is refused rather than followed.
func copyEntryBetween(src *os.Root, srcName string, dst *os.Root, dstName string) error {
	info, err := src.Lstat(srcName)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		return copyDirBetween(src, srcName, dst, dstName)
	case info.Mode().IsRegular():
		return copyFileBetween(src, srcName, dst, dstName)
	default:
		return &fs.PathError{Op: "copy", Path: srcName, Err: tools.ErrNotRegularFile}
	}
}

// copyFileBetween copies the regular file srcName inside src to a new file
// dstName inside dst (0o644, the sandbox uid must read it), opening the
// source with no link followed. dstName must not exist: it is created with
// O_EXCL, which refuses any entry already there, a symbolic link included.
func copyFileBetween(src *os.Root, srcName string, dst *os.Root, dstName string) error {
	in, _, err := tools.OpenNoFollow(src, srcName)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := dst.OpenFile(dstName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// copyDirBetween copies the tree srcName inside src to a new directory
// dstName inside dst: directories 0o755 and regular files 0o644 (the sandbox
// uid must read them). dstName must not exist: it is created with Mkdir,
// which refuses any entry already there, a symbolic link included, and the
// copy is written through that new directory, opened without following a
// link. Symbolic links and special files in the source are skipped, each
// source directory is opened through its parent with no link followed
// (tools.WalkNoFollow, which also walks names that are not UTF-8), and each
// file is opened with no link followed.
func copyDirBetween(src *os.Root, srcName string, dst *os.Root, dstName string) error {
	from, err := tools.OpenSubdirNoFollow(src, srcName)
	if err != nil {
		return err
	}
	defer from.Close()
	if err := dst.Mkdir(dstName, 0o755); err != nil {
		return err
	}
	to, err := tools.OpenSubdirNoFollow(dst, dstName)
	if err != nil {
		return err
	}
	defer to.Close()
	return tools.WalkNoFollow(from, func(rel string, d fs.DirEntry, parent *os.Root, err error) error {
		if err != nil {
			return err // a directory that cannot be copied fails the move; nothing is dropped
		}
		if d.IsDir() {
			return to.Mkdir(rel, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFileBetween(parent, d.Name(), to, rel)
	})
}
