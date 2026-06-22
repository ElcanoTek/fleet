package store

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
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
