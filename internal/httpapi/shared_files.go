package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/sharedfiles"
	"github.com/ElcanoTek/fleet/internal/store"
)

// The cross-chat shared file library (docs/SHARED-FILES.md): admins publish
// files once and every conversation can read them, on both sandbox backends.
//
// Listing and downloading are member-level — the files are already readable
// from every chat, so hiding the catalog from members would be security
// theater. Mutations are admin-only, gated in-handler (the collection route
// mixes a member GET with an admin POST, so a route-level adminMiddleware
// can't express it; requireAdmin applies the identical rule).
//
// Mutations write the DB row first (the manifest is the source of truth),
// then update the staged tree; sharedFilesMu serializes handler staging
// against the maintenance reconciler so a sync pass can never see — and
// "repair" — a mutation's intermediate state. Any residual drift (a crash
// between row and tree) self-heals on the next Sync.

// sharedFilesLibrary derives the library's tree locations from config, the
// same way the sandbox pool derives its workspace mount — falling back to
// ./workspace when FLEET_WORKSPACE_ROOT is unset so both resolve identically.
func (s *Server) sharedFilesLibrary() sharedfiles.Library {
	root := s.cfg.WorkspaceRoot
	if root == "" {
		root = "workspace"
	}
	return sharedfiles.New(s.cfg.DataDir, root)
}

// requireAdmin applies adminMiddleware's exact rule (ADMIN_EMAILS allowlist OR
// users.role = 'admin') inside a mixed-method handler. Reports success; on
// failure the 403 has already been written.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	ctx := r.Context()
	if !s.isAdmin(userFromCtx(ctx)) && roleFromCtx(ctx) != store.RoleAdmin {
		http.Error(w, "forbidden — not an admin", http.StatusForbidden)
		return false
	}
	return true
}

// sharedFilesResponse is the collection GET payload: the manifest plus the
// numbers the admin UI's usage meter needs.
type sharedFilesResponse struct {
	Files      []store.SharedFile `json:"files"`
	TotalBytes int64              `json:"total_bytes"`
	// MaxTotalBytes is the live cap (0 = unlimited).
	MaxTotalBytes int64 `json:"max_total_bytes"`
}

// handleSharedFiles is the collection route: GET lists (member), POST uploads
// (admin, multipart like /attachments).
func (s *Server) handleSharedFiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		files, err := s.store.ListSharedFiles(r.Context())
		if err != nil {
			http.Error(w, "list shared files: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if files == nil {
			files = []store.SharedFile{}
		}
		writeJSON(w, sharedFilesResponse{
			Files:         files,
			TotalBytes:    sharedfiles.TotalBytes(files),
			MaxTotalBytes: s.sharedFilesMaxTotalBytes(),
		})
	case http.MethodPost:
		if !s.requireAdmin(w, r) {
			return
		}
		s.postSharedFiles(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// sharedFilesMaxTotalBytes converts the live MB knob to bytes (0 = unlimited).
func (s *Server) sharedFilesMaxTotalBytes() int64 {
	mb := s.cfg.LiveSharedFilesMaxTotalMB()
	if mb <= 0 {
		return 0
	}
	return int64(mb) << 20
}

// postSharedFiles accepts one-or-more files via multipart/form-data under the
// "files" field (the /attachments shape), plus optional "folder" and
// "description" fields applied to each file in the request.
func (s *Server) postSharedFiles(w http.ResponseWriter, r *http.Request) {
	maxBytes := s.cfg.UploadMaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxUploadBytes
	}
	// Same request-cap shape as /attachments: a couple of big files per
	// request, aligned with the Next.js proxy's 2 GB body cap.
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes*2)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, fmt.Sprintf("upload is over this server's %s combined request limit — upload fewer files at once", humanSize(mbe.Limit)), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "parse multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		http.Error(w, "no files provided (use field name 'files')", http.StatusBadRequest)
		return
	}
	folder, err := sharedfiles.SanitizeFolder(r.FormValue("folder"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	description := strings.TrimSpace(r.FormValue("description"))

	// Validate every size up front (per-file cap AND the library total cap)
	// so an oversize file mid-batch doesn't leave earlier files half-added.
	var incoming int64
	for _, fh := range files {
		if fh.Size > maxBytes {
			http.Error(w, fmt.Sprintf("%q is %s — over this server's %s per-file upload limit", fh.Filename, humanSize(fh.Size), humanSize(maxBytes)), http.StatusRequestEntityTooLarge)
			return
		}
		incoming += fh.Size
	}
	// Serialize admission with manifest writes: two uploads must not both
	// reserve the same remaining space before either has saved its rows.
	s.sharedFilesMu.Lock()
	defer s.sharedFilesMu.Unlock()
	if limit := s.sharedFilesMaxTotalBytes(); limit > 0 {
		existing, err := s.store.TotalSharedFileBytes(r.Context())
		if err != nil {
			http.Error(w, "check library size: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if existing+incoming > limit {
			http.Error(w, fmt.Sprintf("upload would put the shared library at %s, over its %s cap — remove files or raise shared_files_max_total_mb in Settings → Admin → Features", humanSize(existing+incoming), humanSize(limit)), http.StatusRequestEntityTooLarge)
			return
		}
	}

	// Resolve and admit every NAME before writing anything, the same way the
	// sizes were. Checking per file inside the write loop meant a collision
	// on file N came back as a 409 naming only file N — while files 1..N-1
	// were already durably created, unreported in the error response, and
	// invisible to the admin until a refetch. Under sharedFilesMu the answer
	// holds until our own inserts land. Duplicates WITHIN the batch are
	// caught here too; the DB would only have rejected the second one.
	names := make([]string, len(files))
	seen := make(map[string]struct{}, len(files))
	for i, fh := range files {
		name, err := sharedfiles.SanitizeName(fh.Filename)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, dup := seen[name]; dup {
			http.Error(w, fmt.Sprintf("%s: appears twice in this upload", name), http.StatusConflict)
			return
		}
		seen[name] = struct{}{}
		if err := s.store.SharedFilePathAvailable(r.Context(), folder, name); err != nil {
			if errors.Is(err, store.ErrSharedFileExists) || errors.Is(err, store.ErrSharedFileNameIsFolder) {
				http.Error(w, fmt.Sprintf("%s: %v (nothing from this upload was saved)", name, err), http.StatusConflict)
				return
			}
			http.Error(w, "check shared file name: "+err.Error(), http.StatusInternalServerError)
			return
		}
		names[i] = name
	}

	lib := s.sharedFilesLibrary()
	out := make([]store.SharedFile, 0, len(files))
	// The batch is all-or-nothing. The admission checks above catch what can
	// be known up front; a failure INSIDE the loop (a token, an unreadable
	// part, a full disk, a store error) rolls back the rows this request
	// already created before the error is written, so the "nothing from this
	// upload was saved" contract holds for every exit — not only the 409s.
	// Under sharedFilesMu nothing else can have referenced those rows yet.
	fail := func(status int, msg string) {
		s.rollbackSharedFileBatch(r.Context(), lib, out)
		http.Error(w, msg+" (nothing from this upload was saved)", status)
	}
	for i, fh := range files {
		name := names[i]
		id, err := randomToken()
		if err != nil {
			fail(http.StatusInternalServerError, "token: "+err.Error())
			return
		}
		src, err := fh.Open()
		if err != nil {
			fail(http.StatusInternalServerError, "open upload: "+err.Error())
			return
		}
		size, sha, err := lib.SaveCanonical(id, src)
		_ = src.Close()
		if err != nil {
			log.Printf("shared files: save canonical %q: %v", name, err)
			fail(http.StatusInternalServerError, "save upload: "+err.Error())
			return
		}
		row, err := s.store.CreateSharedFile(r.Context(), store.SharedFile{
			ID:          id,
			Name:        name,
			Folder:      folder,
			Description: description,
			SizeBytes:   size,
			ContentType: fh.Header.Get("Content-Type"),
			SHA256:      sha,
			UploadedBy:  userFromCtx(r.Context()),
		})
		if err != nil {
			_ = lib.RemoveCanonical(id)
			if errors.Is(err, store.ErrSharedFileExists) || errors.Is(err, store.ErrSharedFileNameIsFolder) {
				fail(http.StatusConflict, fmt.Sprintf("%s: %v", name, err))
				return
			}
			fail(http.StatusInternalServerError, "save shared file: "+err.Error())
			return
		}
		if err := lib.Stage(row); err != nil {
			// The row exists and the canonical bytes are safe; the staged copy
			// self-heals on the next Sync. Log and keep going rather than
			// failing an upload that IS durably recorded.
			log.Printf("shared files: stage %q: %v (will self-heal on the next maintenance pass)", name, err)
		}
		out = append(out, row)
	}
	writeJSON(w, map[string]any{"files": out})
}

// rollbackSharedFileBatch undoes the rows one upload request created before
// it failed part-way: the manifest row (the source of truth) goes first, then
// the staged copy and the canonical bytes — the same order deleteSharedFile
// uses, so a crash mid-rollback leaves at worst orphaned files the next Sync
// pass reclaims, never a row without bytes. Best-effort per row: a failure is
// logged and the remaining rows are still attempted, because a half-undone
// batch is better than one that stopped undoing at the first hiccup. Caller
// holds sharedFilesMu.
func (s *Server) rollbackSharedFileBatch(ctx context.Context, lib sharedfiles.Library, created []store.SharedFile) {
	for _, row := range created {
		if _, err := s.store.DeleteSharedFile(ctx, row.ID); err != nil {
			log.Printf("shared files: roll back row %q: %v", row.ID, err)
			continue
		}
		if err := lib.Unstage(row); err != nil {
			log.Printf("shared files: roll back staged %q: %v (will self-heal on the next maintenance pass)", row.ID, err)
		}
		if err := lib.RemoveCanonical(row.ID); err != nil {
			log.Printf("shared files: roll back canonical %q: %v", row.ID, err)
		}
	}
}

// sharedFilePatch is the PATCH body: nil = leave alone. Folder and description
// distinguish "absent" from "set to empty" (move to root / clear description);
// an empty name is meaningless so its zero value needs no pointer... but the
// pointer keeps all three symmetric for the client.
type sharedFilePatch struct {
	Name        *string `json:"name"`
	Folder      *string `json:"folder"`
	Description *string `json:"description"`
}

// handleSharedFileItem is the item route: GET …/{id}/download (member),
// PATCH …/{id} rename/move/describe (admin), DELETE …/{id} (admin).
func (s *Server) handleSharedFileItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/shared-files/")
	id, sub, _ := strings.Cut(rest, "/")
	if id == "" {
		http.Error(w, "shared file id required", http.StatusBadRequest)
		return
	}
	switch {
	case r.Method == http.MethodGet && sub == "download":
		s.downloadSharedFile(w, r, id)
	case sub != "":
		http.Error(w, "not found", http.StatusNotFound)
	case r.Method == http.MethodPatch:
		if !s.requireAdmin(w, r) {
			return
		}
		s.patchSharedFile(w, r, id)
	case r.Method == http.MethodDelete:
		if !s.requireAdmin(w, r) {
			return
		}
		s.deleteSharedFile(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// downloadSharedFile streams the CANONICAL bytes (never the staged copy — the
// download must stay correct even mid-restage). ServeContent handles ranges
// and conditional requests off the row's update time.
func (s *Server) downloadSharedFile(w http.ResponseWriter, r *http.Request, id string) {
	f, err := s.store.GetSharedFile(r.Context(), id)
	if err != nil {
		httpErrorForSharedFile(w, err)
		return
	}
	lib := s.sharedFilesLibrary()
	src, err := os.Open(filepath.Join(lib.CanonicalDir, f.ID))
	if err != nil {
		http.Error(w, "shared file bytes are missing on disk", http.StatusNotFound)
		return
	}
	defer src.Close()
	if f.ContentType != "" {
		w.Header().Set("Content-Type", f.ContentType)
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", f.Name))
	http.ServeContent(w, r, f.Name, time.Unix(f.UpdatedAt, 0), src)
}

func (s *Server) patchSharedFile(w http.ResponseWriter, r *http.Request, id string) {
	var req sharedFilePatch
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	old, err := s.store.GetSharedFile(r.Context(), id)
	if err != nil {
		httpErrorForSharedFile(w, err)
		return
	}
	name, folder, description := old.Name, old.Folder, old.Description
	if req.Name != nil {
		name, err = sharedfiles.SanitizeName(*req.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.Folder != nil {
		folder, err = sharedfiles.SanitizeFolder(*req.Folder)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.Description != nil {
		description = strings.TrimSpace(*req.Description)
	}

	s.sharedFilesMu.Lock()
	defer s.sharedFilesMu.Unlock()
	updated, err := s.store.UpdateSharedFileMeta(r.Context(), id, name, folder, description)
	if err != nil {
		httpErrorForSharedFile(w, err)
		return
	}
	if updated.Name != old.Name || updated.Folder != old.Folder {
		// Re-materialize under the new path. Failures self-heal on Sync.
		lib := s.sharedFilesLibrary()
		if err := lib.Unstage(old); err != nil {
			log.Printf("shared files: unstage old path for %q: %v", id, err) //nolint:gosec // G706: id is the URL path segment, %q-quoted so CR/LF cannot forge a log entry
		}
		if err := lib.Stage(updated); err != nil {
			log.Printf("shared files: restage %q: %v (will self-heal on the next maintenance pass)", id, err) //nolint:gosec // G706: id is the URL path segment, %q-quoted so CR/LF cannot forge a log entry
		}
	}
	writeJSON(w, updated)
}

func (s *Server) deleteSharedFile(w http.ResponseWriter, r *http.Request, id string) {
	s.sharedFilesMu.Lock()
	defer s.sharedFilesMu.Unlock()
	f, err := s.store.DeleteSharedFile(r.Context(), id)
	if err != nil {
		httpErrorForSharedFile(w, err)
		return
	}
	lib := s.sharedFilesLibrary()
	if err := lib.Unstage(f); err != nil {
		log.Printf("shared files: unstage %q: %v (will self-heal on the next maintenance pass)", id, err) //nolint:gosec // G706: id names a row the DELETE just removed, %q-quoted so CR/LF cannot forge a log entry
	}
	if err := lib.RemoveCanonical(f.ID); err != nil {
		log.Printf("shared files: remove canonical %q: %v", id, err) //nolint:gosec // G706: id names a row the DELETE just removed, %q-quoted so CR/LF cannot forge a log entry
	}
	w.WriteHeader(http.StatusNoContent)
}

// httpErrorForSharedFile maps the store's shared-file sentinels onto statuses,
// the same per-feature taxonomy shape as httpErrorForSetting.
func httpErrorForSharedFile(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrSharedFileNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, store.ErrSharedFileExists), errors.Is(err, store.ErrSharedFileNameIsFolder):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// SyncSharedFiles reconciles the staged tree against the manifest. cmd/fleet
// calls it once at boot (before the listeners come up, so the first turn
// already sees the library — and before the sandbox pool spawns, so the
// staged root exists when kubernetes pods mount its subPath) and the hourly
// maintenance pass keeps calling it. Exported for exactly those two drivers.
func (s *Server) SyncSharedFiles(ctx context.Context) error {
	files, err := s.store.ListSharedFiles(ctx)
	if err != nil {
		return fmt.Errorf("list shared files: %w", err)
	}
	s.sharedFilesMu.Lock()
	defer s.sharedFilesMu.Unlock()
	return s.sharedFilesLibrary().Sync(files)
}

// appendSharedFilesBlock announces the library at the top of a turn, mirroring
// appendWorkspaceInventoryBlock: paths the agent can read RIGHT NOW, relative
// to its cwd (through the per-conversation "shared" symlink), instead of state
// it must remember or rediscover. Empty library (or a read error — the turn
// must proceed) appends nothing. The block text itself lives in
// sharedfiles.PromptBlock, the one renderer both the chat and scheduled
// drivers announce the library through (#1301).
func (s *Server) appendSharedFilesBlock(ctx context.Context, message string) string {
	files, err := s.store.ListSharedFiles(ctx)
	if err != nil {
		log.Printf("shared files: list for prompt block: %v", err)
		return message
	}
	block := sharedfiles.PromptBlock(files)
	if block == "" {
		return message
	}
	return strings.TrimRight(message, "\n") + "\n\n" + block
}
