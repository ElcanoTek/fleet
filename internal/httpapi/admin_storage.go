// Admin storage management — operator-facing disk visibility + cleanup.
//
// GET  /admin/storage          — byte accounting for the chat-data trees
//                                (attachment uploads, orchestrator temp
//                                uploads, per-conversation workspaces),
//                                host-disk headroom, conversation counts,
//                                and the largest workspaces with owner/
//                                pinned context.
// POST /admin/storage/cleanup  — reclaim disk now: delete old unpinned
//                                conversations (same protections as the
//                                TTL sweep — pinned/archived/shared/
//                                project chats are never touched), sweep
//                                aged uploads + temp files, and remove
//                                orphaned workspace dirs.
//
// Both are admin-gated at route registration. The GET walks the data
// trees on every call; on the single-box deployments fleet targets these
// trees are tens of GB at most, so a request-time walk beats maintaining
// a byte ledger.

package httpapi

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ElcanoTek/fleet/internal/diskguard"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// storageCleanupDefaultDays is the idle cutoff the panel pre-fills: old
// enough that nobody is likely mid-thought, comfortably past the default
// 14-day TTL (which, being turn-triggered historically, may not have run).
const storageCleanupDefaultDays = 30

type storageTreeStats struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
}

type storageWorkspaceRow struct {
	ConversationID string `json:"conversation_id"`
	Bytes          int64  `json:"bytes"`
	// Title/UserEmail empty + Orphaned true when no live conversation row
	// matches the directory (it will be removed by the next orphan sweep).
	Title     string `json:"title,omitempty"`
	UserEmail string `json:"user_email,omitempty"`
	Pinned    bool   `json:"pinned"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
	Orphaned  bool   `json:"orphaned"`
}

type storageResponse struct {
	// Host-disk numbers for the filesystem holding DataDir. Zero values
	// when statfs fails (non-fatal — the tree numbers still render).
	DiskTotalBytes     int64 `json:"disk_total_bytes"`
	DiskAvailableBytes int64 `json:"disk_available_bytes"`

	Uploads     storageTreeStats `json:"uploads"`
	TempUploads storageTreeStats `json:"temp_uploads"`
	Workspaces  storageTreeStats `json:"workspaces"`

	ConversationsTotal     int `json:"conversations_total"`
	ConversationsPinned    int `json:"conversations_pinned"`
	ConversationsProtected int `json:"conversations_protected"`
	// ReclaimableConversations counts what a cleanup at DefaultDays would
	// delete, so the panel can say "cleanup would remove N chats" before
	// the operator commits.
	ReclaimableConversations int `json:"reclaimable_conversations"`
	DefaultDays              int `json:"default_days"`

	LargestWorkspaces []storageWorkspaceRow `json:"largest_workspaces"`
}

func (s *Server) handleAdminStorage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	resp := storageResponse{DefaultDays: storageCleanupDefaultDays}
	resp.Uploads = duTree(ctx, filepath.Join(s.cfg.EmailAttachmentDir, "uploads"))
	resp.TempUploads = duTree(ctx, filepath.Join(s.cfg.DataDir, "temp_uploads"))

	// One walk of the workspace tree, not two: the total and the per-conversation
	// rows come from the same pass, so opening the panel no longer sizes every
	// workspace twice.
	workspaceRoot := tools.WorkspaceDirForConversation("")
	resp.Workspaces, resp.LargestWorkspaces = s.workspaceUsage(r, workspaceRoot, 10)

	if total, avail, err := diskUsage(s.cfg.DataDir); err == nil {
		resp.DiskTotalBytes = total
		resp.DiskAvailableBytes = avail
	}

	cutoff := time.Now().AddDate(0, 0, -storageCleanupDefaultDays)
	if stats, err := s.store.StorageConversationStats(r.Context(), cutoff); err != nil {
		log.Printf("admin storage: conversation stats: %v", err)
	} else {
		resp.ConversationsTotal = stats.Total
		resp.ConversationsPinned = stats.Pinned
		resp.ConversationsProtected = stats.Protected
		resp.ReclaimableConversations = stats.ReclaimableAtCutoff
	}

	writeJSON(w, resp)
}

type storageCleanupRequest struct {
	// OlderThanDays is the idle cutoff for both the conversation delete and
	// the file sweeps. Required, minimum 1 — there is no "wipe everything
	// right now" setting; recent work is always spared.
	OlderThanDays int `json:"older_than_days"`
	// DeleteConversations removes unpinned/unarchived/unshared/non-project
	// conversations idle past the cutoff (and then their workspace dirs).
	DeleteConversations bool `json:"delete_conversations"`
	// SweepFiles removes attachment uploads + orchestrator temp files older
	// than the cutoff.
	SweepFiles bool `json:"sweep_files"`
}

type storageCleanupResponse struct {
	DeletedConversations   int   `json:"deleted_conversations"`
	RemovedUploadFiles     int   `json:"removed_upload_files"`
	RemovedTempFiles       int   `json:"removed_temp_files"`
	RemovedWorkspaces      int   `json:"removed_workspaces"`
	BytesFreed             int64 `json:"bytes_freed"`
	RemainingUploadsBytes  int64 `json:"remaining_uploads_bytes"`
	RemainingTempBytes     int64 `json:"remaining_temp_bytes"`
	RemainingWorkspaceByte int64 `json:"remaining_workspaces_bytes"`
}

func (s *Server) handleAdminStorageCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req storageCleanupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.OlderThanDays < 1 {
		http.Error(w, "older_than_days must be at least 1", http.StatusBadRequest)
		return
	}
	if !req.DeleteConversations && !req.SweepFiles {
		http.Error(w, "nothing to do: enable delete_conversations and/or sweep_files", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	uploadsDir := filepath.Join(s.cfg.EmailAttachmentDir, "uploads")
	tempDir := filepath.Join(s.cfg.DataDir, "temp_uploads")
	workspaceRoot := tools.WorkspaceDirForConversation("")
	before := duTree(ctx, uploadsDir).Bytes + duTree(ctx, tempDir).Bytes + duTree(ctx, workspaceRoot).Bytes

	var resp storageCleanupResponse
	age := time.Duration(req.OlderThanDays) * 24 * time.Hour

	if req.DeleteConversations {
		n, err := s.store.DeleteUnpinnedOlderThan(r.Context(), time.Now().Add(-age))
		if err != nil {
			http.Error(w, "delete conversations: "+err.Error(), http.StatusInternalServerError)
			return
		}
		resp.DeletedConversations = n
		// Their workspace dirs are now orphans — reap them in the same
		// action so the operator sees the space come back immediately.
		if removed, err := s.store.SweepOrphanWorkspaces(r.Context(), workspaceRoot); err != nil {
			log.Printf("admin storage cleanup: workspace sweep: %v", err)
		} else {
			resp.RemovedWorkspaces = removed
		}
	}

	if req.SweepFiles {
		if n, err := store.SweepAttachments(uploadsDir, age); err != nil {
			log.Printf("admin storage cleanup: attachment sweep: %v", err)
		} else {
			resp.RemovedUploadFiles = n
		}
		resp.RemovedTempFiles = sweepTempUploads(tempDir, age)
	}

	resp.RemainingUploadsBytes = duTree(ctx, uploadsDir).Bytes
	resp.RemainingTempBytes = duTree(ctx, tempDir).Bytes
	resp.RemainingWorkspaceByte = duTree(ctx, workspaceRoot).Bytes
	if freed := before - (resp.RemainingUploadsBytes + resp.RemainingTempBytes + resp.RemainingWorkspaceByte); freed > 0 {
		resp.BytesFreed = freed
	}

	log.Printf("admin storage cleanup: %d conversations, %d workspaces, %d upload files, %d temp files removed (%d bytes freed)",
		resp.DeletedConversations, resp.RemovedWorkspaces, resp.RemovedUploadFiles, resp.RemovedTempFiles, resp.BytesFreed)
	writeJSON(w, resp)
}

// workspaceUsage sizes the per-conversation workspace tree in ONE pass and
// returns both the whole-tree total and the top n directories by bytes,
// enriched with conversation title/owner/pinned so the operator knows what they
// are looking at before cleaning up.
//
// Returning both from one walk is the point: the panel previously walked the
// tree once for the total and then again, directory by directory, for the rows.
func (s *Server) workspaceUsage(r *http.Request, root string, n int) (storageTreeStats, []storageWorkspaceRow) {
	ctx := r.Context()
	total := storageTreeStats{Path: root}
	entries, err := os.ReadDir(root)
	if err != nil {
		return total, nil
	}
	rows := make([]storageWorkspaceRow, 0, len(entries))
	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		if !e.IsDir() {
			// Loose files at the root still count toward the tree total even
			// though they are not a conversation workspace.
			if info, statErr := e.Info(); statErr == nil && info.Mode().IsRegular() {
				total.Bytes += info.Size()
				total.Files++
			}
			continue
		}
		sub := duTree(ctx, filepath.Join(root, e.Name()))
		total.Bytes += sub.Bytes
		total.Files += sub.Files
		if sub.Bytes == 0 {
			continue
		}
		rows = append(rows, storageWorkspaceRow{ConversationID: e.Name(), Bytes: sub.Bytes})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Bytes > rows[j].Bytes })
	if len(rows) > n {
		rows = rows[:n]
	}

	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ConversationID
	}
	meta, err := s.store.ConversationStorageMetaByIDs(ctx, ids)
	if err != nil {
		log.Printf("admin storage: workspace meta: %v", err)
		return total, rows
	}
	for i := range rows {
		if m, ok := meta[rows[i].ConversationID]; ok {
			rows[i].Title = m.Title
			rows[i].UserEmail = m.UserEmail
			rows[i].Pinned = m.Pinned
			rows[i].UpdatedAt = m.UpdatedAt
		} else {
			rows[i].Orphaned = true
		}
	}
	return total, rows
}

// diskUsage returns total/available bytes for the filesystem holding path.
// Delegates to diskguard.Usage so the admin panel, the Prometheus gauges and
// the backpressure decision all read the same number the same way — three
// copies of this statfs used to live in three packages, free to drift.
//
// A statfs on the DATA DIR, not "/", so the numbers reflect the mount the
// uploads actually fill.
func diskUsage(path string) (total, available int64, err error) {
	t, a, err := diskguard.Usage(path)
	if err != nil {
		return 0, 0, err
	}
	return int64(t), int64(a), nil //nolint:gosec // G115: single-box disk sizes fit int64
}

// duTree walks a directory tree summing regular-file sizes. Missing dirs
// are zero, not errors — fresh boxes haven't created these trees yet.
//
// ctx-aware: these walks run inside an HTTP handler over trees that can hold
// tens of thousands of files, so a client that has already given up (or a
// shutdown) must stop the walk rather than pin a request goroutine to the end
// of it. A cancelled walk returns the partial total, which is the right answer
// for a display that is about to be discarded anyway.
func duTree(ctx context.Context, dir string) storageTreeStats {
	out := storageTreeStats{Path: dir}
	// Checking ctx on every entry would cost an atomic load per file; every
	// few hundred entries bounds the overrun to microseconds of work.
	const ctxCheckEvery = 256
	seen := 0
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		seen++
		if seen%ctxCheckEvery == 0 && ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil {
			return nil //nolint:nilerr // per-entry errors (vanished file, perms) must not abort the accounting walk
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			out.Bytes += info.Size()
			out.Files++
		}
		return nil
	})
	return out
}

// sweepTempUploads removes regular files under dir older than age. Same
// policy as handlers.CleanupTempFiles (which covers the scheduled path);
// duplicated here because the chat-plane admin server doesn't hold a
// reference to the sched handlers.
func sweepTempUploads(dir string, age time.Duration) int {
	removed := 0
	cutoff := time.Now().Add(-age)
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil //nolint:nilerr // per-entry errors must not abort the sweep
		}
		if info, err := d.Info(); err == nil && info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err == nil { //nolint:gosec // walks the server's own temp_uploads tree
				removed++
			}
		}
		return nil
	})
	return removed
}
