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
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

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

	resp := storageResponse{DefaultDays: storageCleanupDefaultDays}
	resp.Uploads = duTree(filepath.Join(s.cfg.EmailAttachmentDir, "uploads"))
	resp.TempUploads = duTree(filepath.Join(s.cfg.DataDir, "temp_uploads"))

	workspaceRoot := tools.WorkspaceDirForConversation("")
	resp.Workspaces = duTree(workspaceRoot)
	resp.LargestWorkspaces = s.largestWorkspaces(r, workspaceRoot, 10)

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

	uploadsDir := filepath.Join(s.cfg.EmailAttachmentDir, "uploads")
	tempDir := filepath.Join(s.cfg.DataDir, "temp_uploads")
	workspaceRoot := tools.WorkspaceDirForConversation("")
	before := duTree(uploadsDir).Bytes + duTree(tempDir).Bytes + duTree(workspaceRoot).Bytes

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

	resp.RemainingUploadsBytes = duTree(uploadsDir).Bytes
	resp.RemainingTempBytes = duTree(tempDir).Bytes
	resp.RemainingWorkspaceByte = duTree(workspaceRoot).Bytes
	if freed := before - (resp.RemainingUploadsBytes + resp.RemainingTempBytes + resp.RemainingWorkspaceByte); freed > 0 {
		resp.BytesFreed = freed
	}

	log.Printf("admin storage cleanup: %d conversations, %d workspaces, %d upload files, %d temp files removed (%d bytes freed)",
		resp.DeletedConversations, resp.RemovedWorkspaces, resp.RemovedUploadFiles, resp.RemovedTempFiles, resp.BytesFreed)
	writeJSON(w, resp)
}

// largestWorkspaces sizes each per-conversation workspace dir and returns
// the top n by bytes, enriched with conversation title/owner/pinned so the
// operator knows what they're looking at before cleaning up.
func (s *Server) largestWorkspaces(r *http.Request, root string, n int) []storageWorkspaceRow {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	rows := make([]storageWorkspaceRow, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		size := duTree(filepath.Join(root, e.Name())).Bytes
		if size == 0 {
			continue
		}
		rows = append(rows, storageWorkspaceRow{ConversationID: e.Name(), Bytes: size})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Bytes > rows[j].Bytes })
	if len(rows) > n {
		rows = rows[:n]
	}

	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ConversationID
	}
	meta, err := s.store.ConversationStorageMetaByIDs(r.Context(), ids)
	if err != nil {
		log.Printf("admin storage: workspace meta: %v", err)
		return rows
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
	return rows
}

// diskUsage returns total/available bytes for the filesystem holding path.
// Mirrors hoststats.readDisk (unexported there) — a statfs on the data dir
// rather than "/" so the numbers reflect the mount the uploads actually
// fill.
func diskUsage(path string) (total, available int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	blockSize := uint64(st.Bsize)                                          // #nosec G115 -- kernel block sizes are non-negative and bounded.
	return int64(st.Blocks * blockSize), int64(st.Bavail * blockSize), nil //nolint:gosec // G115: single-box disk sizes fit int64
}

// duTree walks a directory tree summing regular-file sizes. Missing dirs
// are zero, not errors — fresh boxes haven't created these trees yet.
func duTree(dir string) storageTreeStats {
	out := storageTreeStats{Path: dir}
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
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
