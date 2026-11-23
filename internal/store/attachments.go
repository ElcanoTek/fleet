package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// SweepAttachments deletes regular files under dir whose mtime is older
// than ttl, unless keep says the file is still in use. Walks recursively to
// cover any future per-sender or per-date subfolders the email MCP might
// introduce, but does NOT remove empty directories — avoids racing with an
// in-flight download that just mkdir'd a timestamped folder.
//
// Age alone cannot tell an abandoned file from one something kept still reads:
// an image sent in a pinned chat is as old as the chat. So the walk asks keep
// about every aged file just before removing it; AttachmentKeeper builds the
// answer for the attachment tree. keep reporting an error keeps the file (a
// lookup that failed is not a "no"), and the failures come back as the error.
// A nil keep removes by age alone.
//
// Missing dir is not an error: returns (0, nil), since the email MCP
// may not have run yet on a fresh box.
func SweepAttachments(dir string, ttl time.Duration, keep func(path string) (bool, error)) (int, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", dir)
	}

	cutoff := time.Now().Add(-ttl)
	removed := 0
	unchecked := 0
	var firstKeepErr error
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		// Per-entry errors (a file disappeared mid-walk, perms, etc.)
		// shouldn't abort the whole sweep — keep going.
		if err != nil {
			return nil //nolint:nilerr // intentional: a per-entry walk error (file vanished mid-sweep, perms) must not abort the whole cleanup sweep.
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil || fi.ModTime().After(cutoff) {
			return nil //nolint:nilerr // intentional: an Info() error just means skip this entry; the sweep continues.
		}
		if keep != nil {
			inUse, kerr := keep(path)
			if kerr != nil {
				unchecked++
				if firstKeepErr == nil {
					firstKeepErr = kerr
				}
				return nil
			}
			if inUse {
				return nil
			}
		}
		if err := os.Remove(path); err == nil { //nolint:gosec // sweep operates on our own attachment tree
			removed++
		}
		return nil
	})
	if firstKeepErr != nil {
		walkErr = errors.Join(walkErr, fmt.Errorf("kept %d aged files whose use could not be checked: %w", unchecked, firstKeepErr))
	}
	return removed, walkErr
}

// The subdirectories of EMAIL_ATTACHMENT_DIR whose files can still be in use
// after they age past the conversation window. Everything that builds these
// paths uses these names: the chat plane's writers (httpapi.userUploadsRoot,
// httpapi.privateWorkspaceStashRoot) and the agent's image reader, through
// agent.UploadsDirName.
const (
	// AttachmentUploadsDir holds chat uploads, <user>/<token>/<name>. A sent
	// image is read again on every later turn of its conversation.
	AttachmentUploadsDir = agent.UploadsDirName
	// AttachmentPrivateStashDir holds the Multiplayer private stash, one
	// directory per conversation: participants' attachments between their
	// turns, and the owner's during a participant's turn. It goes with its
	// conversation (SweepOrphanWorkspaces), never by age.
	AttachmentPrivateStashDir = "multiplayer-private"
)

// UploadReferences answers whether a kept conversation still refers to an
// upload. *Store implements it.
type UploadReferences interface {
	// ReferencedUploads returns every referenced upload path in one query,
	// and whether the record is complete (false until the backfill of
	// messages older than it has finished).
	ReferencedUploads(ctx context.Context) (map[string]struct{}, bool, error)
	// UploadReferenced asks about one path.
	UploadReferenced(ctx context.Context, path string) (bool, error)
}

// AttachmentKeeper is the keep rule SweepAttachments applies to the
// attachment tree rooted at attachmentDir (EMAIL_ATTACHMENT_DIR), whichever
// part of it a caller walks:
//
//   - an upload is kept while a message of a conversation that still exists
//     refers to it. Conversation retention alone decides which conversations
//     exist, so an upload lives exactly as long as the chats that sent it,
//     with no second copy of the retention rules; an upload never sent in any
//     message is removed by age. Until the record of references is complete
//     (BackfillMessageUploads, after an upgrade), every upload is kept;
//   - the Multiplayer private stash is always kept: it is removed with its
//     conversation;
//   - anything else (outbound email copies left by a crash, files an email
//     connector wrote) is removed by age, as before.
//
// The cost does not grow with the files it keeps: the first aged upload loads
// every referenced path in one query, and only a file missing from that set —
// one about to be removed — is asked about again on its own, which also
// covers a message written after the set was loaded. The returned function
// serves one walk; it is not safe for concurrent use. A nil refs cannot tell
// whether an upload is in use, so it keeps them all.
func AttachmentKeeper(ctx context.Context, refs UploadReferences, attachmentDir string) func(path string) (bool, error) {
	root := normalizeUploadPath(attachmentDir)
	uploads := filepath.Join(root, AttachmentUploadsDir)
	stash := filepath.Join(root, AttachmentPrivateStashDir)
	var (
		loaded     bool
		complete   bool
		referenced map[string]struct{}
		loadErr    error
	)
	return func(path string) (bool, error) {
		p := normalizeUploadPath(path)
		switch {
		case isWithin(stash, p):
			return true, nil
		case isWithin(uploads, p):
			if refs == nil {
				return true, nil
			}
			if !loaded {
				loaded = true
				referenced, complete, loadErr = refs.ReferencedUploads(ctx)
			}
			if loadErr != nil {
				return true, loadErr
			}
			if !complete {
				return true, nil
			}
			if _, ok := referenced[p]; ok {
				return true, nil
			}
			return refs.UploadReferenced(ctx, p)
		default:
			return false, nil
		}
	}
}

// isWithin reports whether path lies strictly below dir.
func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && filepath.IsLocal(rel)
}

// SweepOrphanWorkspaces removes per-conversation workspace directories
// under root whose name is NOT the id of a live conversation.
//
// The email download_attachment tool writes into
// `<workspaceRoot>/<conversation_id>/`, and native bash/run_python cwd
// into the same dir. Once the conversation is gone (TTL, per-user cap,
// or a user-account delete that cascaded into conversations), nothing
// on disk notices — the dir lingers until an operator scrubs it. This
// sweep closes that loop.
//
// Live-id lookup happens once up front so we don't query the DB per
// subdirectory; only a directory missing from that set — a removal candidate
// — is looked up again on its own just before it is removed, so a
// conversation created after the set was read keeps its directory. Non-UUID
// entries (e.g. stray files or unrelated dirs
// created by hand) are ignored, not deleted — be conservative; the
// workspace root may be under an operator-managed mount.
func (s *Store) SweepOrphanWorkspaces(ctx context.Context, root string) (int, error) {
	if root == "" {
		return 0, nil
	}
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", root)
	}

	live, err := s.liveConversationIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("load live ids: %w", err)
	}
	if afterLiveSnapshot != nil {
		afterLiveSnapshot()
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("readdir %s: %w", root, err)
	}
	removed := 0
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Only touch UUID-shaped names — matches what
		// WorkspaceDirForConversation writes. Spares anything an
		// operator dropped alongside by hand.
		if !looksLikeConversationID(name) {
			continue
		}
		if _, alive := live[name]; alive {
			continue
		}
		// live was read before the directory listing, so a conversation
		// created since then is missing from it while its directory already
		// exists. Ask again for this one id right before removing it.
		if alive, err := s.conversationExists(ctx, name); err != nil || alive {
			if err != nil {
				errs = append(errs, fmt.Errorf("recheck workspace %s: %w", name, err))
			}
			continue
		}
		if err := removeWorkspaceTree(root, name); err != nil {
			// Reported, not swallowed: a dir that cannot be removed is
			// retried every pass, so a silent failure pins its bytes
			// forever (a read-only subdir did exactly that to 4.8 GB on a
			// production box). Keep sweeping the rest.
			errs = append(errs, fmt.Errorf("remove orphan workspace %s: %w", name, err))
			continue
		}
		removed++
	}
	// Also sweep the sibling .fleet-private-<base> stash root if present.
	if absRoot, absErr := filepath.Abs(root); absErr == nil {
		absRoot = filepath.Clean(absRoot)
		siblingStash := filepath.Join(filepath.Dir(absRoot), ".fleet-private-"+filepath.Base(absRoot))
		if stEntries, rerr := os.ReadDir(siblingStash); rerr == nil {
			for _, e := range stEntries {
				if !e.IsDir() || !looksLikeConversationID(e.Name()) {
					continue
				}
				if _, alive := live[e.Name()]; alive {
					continue
				}
				if alive, err := s.conversationExists(ctx, e.Name()); err != nil || alive {
					continue
				}
				_ = removeWorkspaceTree(siblingStash, e.Name())
			}
		}
	}
	return removed, errors.Join(errs...)
}

// removeWorkspaceTree deletes root/name. A plain RemoveAll fails on any
// directory the agent left without owner rwx — `chmod -R a-w`, `chmod 000 .`,
// an archive that unpacked read-only modes — because unlinking an entry needs
// write permission on its PARENT and listing one needs read. On that failure
// the orphan dir itself and every directory under it are made owner-rwx and
// the removal retried.
//
// Every operation goes through an os.Root opened at the WORKSPACE ROOT, never
// a host path joined from it. The sandbox can write the workspace, so it could
// swap an orphan dir for a symlink between the two attempts; Root refuses to
// resolve any name that escapes it, so a planted link can redirect neither the
// chmods nor the removal outside the tree. WalkDir does not follow symlinks
// either, so only real directories are visited.
// RemoveWorkspaceTree deletes root/name, repairing directory permissions inside
// root/name first if a sandboxed process left read-only subdirectories behind.
func RemoveWorkspaceTree(root, name string) error {
	return removeWorkspaceTree(root, name)
}

func removeWorkspaceTree(root, name string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := r.RemoveAll(name); err == nil {
		return nil
	}
	// The orphan dir first, through the parent root: an unreadable (0111,
	// 0000) dir cannot be opened to walk until its own mode is repaired. A
	// symbolic link at name is never repaired or walked; the walk only runs
	// in the very directory that was Lstat'ed (os.SameFile), so a link swapped
	// in between cannot point it at another directory.
	info, lerr := r.Lstat(name)
	if lerr != nil || !info.IsDir() {
		return r.RemoveAll(name)
	}
	_ = r.Chmod(name, info.Mode().Perm()|0o700)
	if removeTreeAfterLstat != nil {
		removeTreeAfterLstat(name)
	}
	// "<name>/." opens name with O_DIRECTORY (and "." relative to it), so a
	// FIFO swapped in for the directory fails at once instead of blocking
	// the sweep forever with no writer.
	if sub, oerr := r.OpenRoot(name + string(filepath.Separator) + "."); oerr == nil {
		if got, serr := sub.Stat("."); serr != nil || !os.SameFile(info, got) {
			_ = sub.Close()
			return r.RemoveAll(name)
		}
		_ = fs.WalkDir(sub.FS(), ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil //nolint:nilerr // best effort: an unreadable entry just stays unreadable; the RemoveAll below reports what is left.
			}
			if info, ierr := d.Info(); ierr == nil && info.Mode().Perm()&0o700 != 0o700 {
				_ = sub.Chmod(path, info.Mode().Perm()|0o700)
			}
			return nil
		})
		_ = sub.Close()
	}
	return r.RemoveAll(name)
}

// removeTreeAfterLstat is a test seam run between the orphan directory's
// Lstat and its open in removeWorkspaceTree; nil in production.
var removeTreeAfterLstat func(name string)

// liveConversationIDs returns the set of conversation ids currently in the DB
// (including soft-deleted tombstones within their restore window, until
// PurgeSoftDeletedConversations removes them). Used by SweepOrphanWorkspaces
// to decide what on-disk dirs to reap.
func (s *Store) liveConversationIDs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM conversations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// conversationExists reports whether a conversation row with this id exists
// (a soft-deleted tombstone counts, as in liveConversationIDs).
func (s *Store) conversationExists(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM conversations WHERE id = $1)`, id).Scan(&exists)
	return exists, err
}

// afterLiveSnapshot is a test seam run in SweepOrphanWorkspaces right after the
// live-id set is read; nil in production.
var afterLiveSnapshot func()

// LooksLikeConversationID reports whether name has the shape of a UUID in
// 8-4-4-4-12 hex form.
func LooksLikeConversationID(name string) bool {
	return looksLikeConversationID(name)
}

// looksLikeConversationID reports whether name has the shape of a v4
// UUID in 8-4-4-4-12 hex form. Intentionally lax on the version nibble
// so we don't accidentally skip older ids.
func looksLikeConversationID(name string) bool {
	if len(name) != 36 {
		return false
	}
	for i, c := range name {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

// ── admin storage management ─────────────────────────────────────────────

// StorageConversationStats is the conversation-side picture the admin
// Storage panel renders next to the on-disk numbers: how many chats exist,
// how many are protected from cleanup (anything that is not temporary),
// and how many an admin cleanup at the given idle cutoff would reclaim.
type StorageConversationStats struct {
	Total     int
	Pinned    int
	Protected int // live and not temporary (temporaryConversationClause): cleanup-exempt
	// ReclaimableAtCutoff counts live, unprotected conversations idle
	// since before the cutoff — exactly the set DeleteUnpinnedOlderThan
	// would remove.
	ReclaimableAtCutoff int
}

// StorageConversationStats returns the counts above. cutoff is an absolute
// time; conversations with updated_at older than it count as reclaimable.
func (s *Store) StorageConversationStats(ctx context.Context, cutoff time.Time) (StorageConversationStats, error) {
	var out StorageConversationStats
	// Protected and reclaimable are read from temporaryConversationClause, the
	// one definition the retention sweep and DeleteUnpinnedOlderThan use, so
	// the preview counts exactly what a cleanup would remove.
	err := s.db.QueryRowContext(ctx,
		`SELECT
		   COUNT(*) FILTER (WHERE deleted_at IS NULL),
		   COUNT(*) FILTER (WHERE deleted_at IS NULL AND pinned),
		   COUNT(*) FILTER (WHERE deleted_at IS NULL AND NOT (`+temporaryConversationClause+`)),
		   COUNT(*) FILTER (WHERE `+temporaryConversationClause+` AND updated_at < $1)
		 FROM conversations`,
		cutoff.Unix(),
	).Scan(&out.Total, &out.Pinned, &out.Protected, &out.ReclaimableAtCutoff)
	if err != nil {
		return StorageConversationStats{}, fmt.Errorf("storage conversation stats: %w", err)
	}
	return out, nil
}

// DeleteUnpinnedOlderThan hard-deletes live conversations idle since before
// the cutoff, with the same protections as SweepExpired's TTL pass — the same
// temporaryConversationClause, not a copy of it: pinned, archived, shared,
// project-bound, Multiplayer and team-visible chats, and chats with an armed
// or paused patrol or timer, are never touched. (A hand-copied list here once
// lacked the patrol/timer rule, so this cleanup deleted chats the retention
// sweep keeps.) This is the admin "reclaim disk now" action — unlike the TTL sweep it always
// hard-deletes (the operator asked for space back), and the caller is
// expected to follow up with SweepOrphanWorkspaces to free the deleted
// conversations' workspace directories.
func (s *Store) DeleteUnpinnedOlderThan(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations WHERE `+temporaryConversationClause+` AND updated_at < $1`,
		cutoff.Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("delete unpinned older than: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ConversationStorageMeta is the per-conversation context the admin Storage
// panel shows next to a large workspace directory: whose chat it is, what
// it's called, and whether cleanup would spare it.
type ConversationStorageMeta struct {
	Title     string
	UserEmail string
	Pinned    bool
	UpdatedAt int64 // unix seconds
}

// ConversationStorageMetaByIDs fetches title/owner/pinned for the given
// conversation ids (workspace dir names). Missing ids are simply absent
// from the map — the caller labels those workspaces as orphaned.
func (s *Store) ConversationStorageMetaByIDs(ctx context.Context, ids []string) (map[string]ConversationStorageMeta, error) {
	out := make(map[string]ConversationStorageMeta, len(ids))
	if len(ids) == 0 || s == nil || s.db == nil {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, user_email, pinned, updated_at FROM conversations WHERE deleted_at IS NULL AND id = ANY($1)`,
		ids,
	)
	if err != nil {
		return nil, fmt.Errorf("conversation storage meta: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var m ConversationStorageMeta
		if err := rows.Scan(&id, &m.Title, &m.UserEmail, &m.Pinned, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out[id] = m
	}
	return out, rows.Err()
}

// downloadPartialRE matches the hidden partial files download_url streams
// into (sandbox.StreamPartialName; fileops.py enforces the same pattern).
var downloadPartialRE = regexp.MustCompile(`^\.fleet-download-[0-9a-f]{24}\.partial$`)

// DownloadPartialMaxAge is how long a download partial file may sit untouched
// before the sweep removes it. A running download rewrites its partial every
// chunk and times out within 30 minutes, so an hour-old one is abandoned.
const DownloadPartialMaxAge = time.Hour

// SweepStaleDownloadPartials removes download_url partial files left under
// root by a download that never finished (a cancelled turn can retire the
// sandbox before the download aborts its own partial). Only regular files
// with the exact reserved name, untouched for olderThan, are removed; symlinks
// are never followed. The in-sandbox sweep on a later download's first chunk
// covers the same case per directory; this one needs no later download.
// Returns the count and bytes removed; a missing root is (0, 0, nil).
func SweepStaleDownloadPartials(root string, olderThan time.Duration) (int, int64, error) {
	if root == "" {
		return 0, 0, nil
	}
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	// Removal goes through a root-scoped handle, so a directory swapped for a
	// symlink mid-walk cannot redirect it outside root.
	scoped, err := os.OpenRoot(root)
	if err != nil {
		return 0, 0, err
	}
	defer scoped.Close()
	cutoff := time.Now().Add(-olderThan)
	removed := 0
	var freed int64
	var errs []error
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is skipped, not fatal: keep sweeping.
			if d != nil && d.IsDir() && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !downloadPartialRE.MatchString(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			return nil //nolint:nilerr // a file that vanished or is fresh is simply not swept
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil //nolint:nilerr // a path the walk produced is always under root; skip if not
		}
		if err := scoped.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
			return nil
		}
		removed++
		freed += info.Size()
		return nil
	})
	if walkErr != nil {
		errs = append(errs, walkErr)
	}
	return removed, freed, errors.Join(errs...)
}
