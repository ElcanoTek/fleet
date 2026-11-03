// Per-conversation workspace file fetch:
// GET /conversations/{id}/workspace/<path>. Split out of server.go (#1127).

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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
// the conversation. relPath is interpreted relative to the conv's
// workspace dir; .. traversal and absolute paths are rejected. The
// resolved file must still live under the workspace dir after symlink
// resolution (filepath.EvalSymlinks) so a maliciously-placed symlink
// can't escape.
func (s *Server) handleWorkspaceFile(w http.ResponseWriter, r *http.Request, convID, relPath string) {
	user := userFromCtx(r.Context())
	conv, err := s.getOwnedOrMultiplayerConversation(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
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

	// Resolve the requested file against the conversation workspace root,
	// enforcing the shared path-traversal guard (tools.SafeWorkspaceJoin):
	// reject `..`/absolute/NUL paths up front, then EvalSymlinks + confirm the
	// result still lives under the workspace dir. Without the symlink check a
	// `ln -s /etc/passwd workspace/<conv>/p` written by the agent (or a
	// malicious upload) would let any user with the conversation read host
	// secrets via this endpoint.
	isOwner := strings.EqualFold(conv.UserEmail, user) || conv.UserEmail == ""
	if isPrivateWorkspacePath(filepath.ToSlash(filepath.Clean(relPath))) {
		if !isOwner || s.isParticipantWorkspaceActive(convID, conv.UserEmail) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
	}
	wsDir, err := tools.SafeWorkspaceJoin(tools.WorkspaceDirForConversation(convID), ".")
	if err != nil {
		http.Error(w, "workspace unavailable", http.StatusNotFound)
		return
	}
	resolvedAbs, err := tools.SafeWorkspaceJoin(wsDir, relPath)
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

	// EvalSymlinks provides useful validation errors, but the workspace is
	// still writable by the sandbox. Open through os.Root so a symlink swap
	// after validation cannot turn this host-side broker into an arbitrary
	// host read. Stat the opened descriptor rather than a second pathname.
	root, err := os.OpenRoot(wsDir)
	if err != nil {
		http.Error(w, "workspace unavailable", http.StatusNotFound)
		return
	}
	defer root.Close()
	rel, err := filepath.Rel(wsDir, resolvedAbs)
	if err != nil || !filepath.IsLocal(rel) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if isPrivateWorkspacePath(filepath.ToSlash(rel)) {
		if !isOwner || s.isParticipantWorkspaceActive(convID, conv.UserEmail) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
	}
	f, err := root.Open(rel)
	if err != nil {
		http.Error(w, "file unavailable", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "stat failed", http.StatusInternalServerError)
		return
	}
	if !info.Mode().IsRegular() {
		http.Error(w, "path is not a regular file", http.StatusBadRequest)
		return
	}

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

func participantDigest(userEmail string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(userEmail))))
	return hex.EncodeToString(sum[:])[:32]
}

func (s *Server) privateWorkspaceStashRoot() (string, error) {
	if s.cfg != nil {
		if base := strings.TrimSpace(s.cfg.EmailAttachmentDir); base != "" {
			abs, err := filepath.Abs(base)
			if err != nil {
				return "", err
			}
			return filepath.Join(filepath.Clean(abs), "multiplayer-private"), nil
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
		_ = recoverInterruptedParticipantWorkspace(tools.WorkspaceDirForConversation(convID), filepath.Join(root, convID))
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
	if _, err := os.Stat(filepath.Join(stashRoot, "active-participant")); err != nil {
		return false
	}
	// No participant turn is active in memory, so any active-participant
	// marker left on disk came from an interrupted earlier process. Recover
	// the owner's private workspace directories immediately before serving the
	// request; fail closed if recovery cannot complete.
	if err := recoverInterruptedParticipantWorkspace(tools.WorkspaceDirForConversation(convID), stashRoot); err != nil {
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
	stashRoot, err := s.privateWorkspaceStashDir(conv.ID)
	if err != nil {
		unlock()
		return err
	}
	wsDir := tools.WorkspaceDirForConversation(conv.ID)
	if err := recoverInterruptedParticipantWorkspace(wsDir, stashRoot); err != nil {
		unlock()
		return err
	}
	if conv.UserEmail == "" || strings.EqualFold(conv.UserEmail, user) {
		s.setParticipantWorkspaceActive(conv.ID, "", unlock)
		return nil
	}
	if _, err := tools.EnsureWorkspaceDir(conv.ID); err != nil {
		unlock()
		return err
	}
	ownerStash := filepath.Join(stashRoot, "owner")
	if err := os.MkdirAll(ownerStash, 0o750); err != nil {
		unlock()
		return err
	}
	marker := filepath.Join(stashRoot, "active-participant")
	if err := os.WriteFile(marker, []byte(strings.ToLower(strings.TrimSpace(user))), 0o600); err != nil { //nolint:gosec // host-side control-plane marker
		unlock()
		return err
	}
	for _, dirName := range privateWorkspaceDirs {
		wsSub := filepath.Join(wsDir, dirName)
		if err := movePrivateDirIfPresent(wsSub, filepath.Join(ownerStash, dirName)); err != nil {
			_ = restoreParticipantWorkspaceToOwner(wsDir, stashRoot, "")
			unlock()
			return fmt.Errorf("stash owner %s: %w", dirName, err)
		}
		if _, statErr := os.Lstat(wsSub); !os.IsNotExist(statErr) { //nolint:gosec // server-derived workspace path
			_ = restoreParticipantWorkspaceToOwner(wsDir, stashRoot, "")
			unlock()
			return fmt.Errorf("owner %s remained in workspace after stashing", dirName)
		}
	}
	partAtt := filepath.Join(stashRoot, "participants", participantDigest(user), uploadsDir)
	if err := movePrivateDirIfPresent(partAtt, filepath.Join(wsDir, uploadsDir)); err != nil {
		_ = restoreParticipantWorkspaceToOwner(wsDir, stashRoot, user)
		unlock()
		return fmt.Errorf("restore participant attachments: %w", err)
	}
	s.setParticipantWorkspaceActive(conv.ID, user, unlock)
	return nil
}

// restoreTurnPrivateWorkspace removes a non-owner participant's materialized
// user-skills/, stashes their staged attachments/ outside the mounted
// workspace, and restores the conversation owner's attachments/ and
// user-skills/ before the turn lock is released.
func (s *Server) restoreTurnPrivateWorkspace(conv *store.Conversation, user string) {
	if conv == nil || conv.ID == "" || !filepath.IsLocal(conv.ID) || strings.ContainsAny(conv.ID, `/\`) {
		return
	}
	if conv.UserEmail != "" && !strings.EqualFold(conv.UserEmail, user) {
		if stashRoot, err := s.privateWorkspaceStashDir(conv.ID); err == nil {
			wsDir := tools.WorkspaceDirForConversation(conv.ID)
			_ = restoreParticipantWorkspaceToOwner(wsDir, stashRoot, user)
		}
	}
	s.clearParticipantWorkspaceActive(conv.ID)
}

func recoverInterruptedParticipantWorkspace(wsDir, stashRoot string) error {
	marker := filepath.Join(stashRoot, "active-participant")
	raw, err := os.ReadFile(marker) //nolint:gosec // server-derived stashRoot
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return restoreParticipantWorkspaceToOwner(wsDir, stashRoot, strings.TrimSpace(string(raw)))
}

func restoreParticipantWorkspaceToOwner(wsDir, stashRoot, participantEmail string) error {
	if err := os.RemoveAll(filepath.Join(wsDir, userSkillsRoot)); err != nil { //nolint:gosec // server-derived conversation workspace
		return err
	}
	if participantEmail != "" {
		partAtt := filepath.Join(stashRoot, "participants", participantDigest(participantEmail), uploadsDir)
		if err := movePrivateDirIfPresent(filepath.Join(wsDir, uploadsDir), partAtt); err != nil {
			return err
		}
	} else if err := os.RemoveAll(filepath.Join(wsDir, uploadsDir)); err != nil { //nolint:gosec // server-derived conversation workspace
		return err
	}
	ownerStash := filepath.Join(stashRoot, "owner")
	for _, dirName := range privateWorkspaceDirs {
		if err := movePrivateDirIfPresent(filepath.Join(ownerStash, dirName), filepath.Join(wsDir, dirName)); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(stashRoot, "active-participant")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func movePrivateDirIfPresent(src, dst string) error {
	info, err := os.Lstat(src) //nolint:gosec // server-derived workspace/stash paths
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return os.RemoveAll(src) //nolint:gosec // server-derived source
	}
	if err := os.RemoveAll(dst); err != nil { //nolint:gosec // server-derived destination
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // workspace/stash parent dir
		return err
	}
	if err := os.Rename(src, dst); err == nil { //nolint:gosec // server-derived workspace/stash paths
		return nil
	}
	if err := copyDirRecursive(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src) //nolint:gosec // server-derived source
}

func copyDirRecursive(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error { //nolint:gosec // G703: src and dst are server-derived workspace/stash paths
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || !filepath.IsLocal(rel) {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755) //nolint:gosec // sandbox-readable workspace dir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(path) //nolint:gosec // server-derived source
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // sandbox-readable workspace file
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
}
