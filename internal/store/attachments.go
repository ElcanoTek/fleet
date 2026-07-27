package store

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/lib/pq"
)

// SweepAttachments deletes regular files under dir whose mtime is older
// than ttl. Walks recursively to cover any future per-sender or
// per-date subfolders the email MCP might introduce, but does NOT
// remove empty directories — avoids racing with an in-flight download
// that just mkdir'd a timestamped folder.
//
// Missing dir is not an error: returns (0, nil), since the email MCP
// may not have run yet on a fresh box.
func SweepAttachments(dir string, ttl time.Duration) (int, error) {
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
		if err := os.Remove(path); err == nil { //nolint:gosec // sweep operates on our own attachment tree
			removed++
		}
		return nil
	})
	return removed, walkErr
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
// subdirectory. Non-UUID entries (e.g. stray files or unrelated dirs
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

	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("readdir %s: %w", root, err)
	}
	removed := 0
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
		if err := os.RemoveAll(filepath.Join(root, name)); err == nil {
			removed++
		}
	}
	return removed, nil
}

// liveConversationIDs returns the set of conversation ids currently in
// the DB. Used by SweepOrphanWorkspaces to decide what on-disk dirs to
// reap.
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
// how many are protected from cleanup (pinned/archived/shared/project),
// and how many an admin cleanup at the given idle cutoff would reclaim.
type StorageConversationStats struct {
	Total     int
	Pinned    int
	Protected int // pinned OR archived OR shared OR project-bound (cleanup-exempt)
	// ReclaimableAtCutoff counts live, unprotected conversations idle
	// since before the cutoff — exactly the set DeleteUnpinnedOlderThan
	// would remove.
	ReclaimableAtCutoff int
}

// StorageConversationStats returns the counts above. cutoff is an absolute
// time; conversations with updated_at older than it count as reclaimable.
func (s *Store) StorageConversationStats(ctx context.Context, cutoff time.Time) (StorageConversationStats, error) {
	var out StorageConversationStats
	err := s.db.QueryRowContext(ctx,
		`SELECT
		   COUNT(*) FILTER (WHERE deleted_at IS NULL),
		   COUNT(*) FILTER (WHERE deleted_at IS NULL AND pinned),
		   COUNT(*) FILTER (WHERE deleted_at IS NULL AND (pinned OR archived_at IS NOT NULL OR share_token IS NOT NULL OR project_id IS NOT NULL)),
		   COUNT(*) FILTER (WHERE deleted_at IS NULL AND NOT pinned AND archived_at IS NULL AND share_token IS NULL AND project_id IS NULL AND updated_at < $1)
		 FROM conversations`,
		cutoff.Unix(),
	).Scan(&out.Total, &out.Pinned, &out.Protected, &out.ReclaimableAtCutoff)
	if err != nil {
		return StorageConversationStats{}, fmt.Errorf("storage conversation stats: %w", err)
	}
	return out, nil
}

// DeleteUnpinnedOlderThan hard-deletes live conversations idle since before
// the cutoff, with the same protections as SweepExpired's TTL pass: pinned,
// archived, shared, and project-bound conversations are never touched. This
// is the admin "reclaim disk now" action — unlike the TTL sweep it always
// hard-deletes (the operator asked for space back), and the caller is
// expected to follow up with SweepOrphanWorkspaces to free the deleted
// conversations' workspace directories.
func (s *Store) DeleteUnpinnedOlderThan(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations
		 WHERE pinned = FALSE AND archived_at IS NULL AND deleted_at IS NULL AND share_token IS NULL AND project_id IS NULL AND updated_at < $1`,
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
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, user_email, pinned, updated_at FROM conversations WHERE deleted_at IS NULL AND id = ANY($1)`,
		pq.Array(ids),
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
