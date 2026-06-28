package handlers

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Workspace file browser (#287): list + download the artifacts a scheduled
// task's agent wrote into its per-run workspace directory (reports, data, code,
// charts). The workspace path is recorded on the task row by the runner when the
// run begins (Task.WorkspacePath). These endpoints expose it after the fact so
// an operator never has to SSH into the box to retrieve a generated file.
//
// SECURITY (the whole point of this feature being careful):
//
//   - Ownership. Unlike GetTask's permissive task-visibility check, workspace
//     files can contain anything the agent produced — so access is restricted to
//     the admin key OR the principal that created the task (the creating user, or
//     the creating API key). See taskWorkspaceOwned. A shared task-result link
//     grants NO access here; these endpoints require the creator's own credential.
//   - Path traversal. Every per-file access goes through tools.SafeWorkspaceJoin,
//     the single shared guard: it rejects "..", absolute paths, and NUL bytes up
//     front, then EvalSymlinks + confirms the resolved path is still under the
//     workspace root. Symlinks the agent planted that point outside the workspace
//     (including the structural protocols/personas/system_prompts/skills links in
//     a chat-style workspace) are therefore NOT followable through this endpoint.
//   - Lifecycle. The workspace is only browsable for COMPLETED tasks (success or
//     error). A running task's workspace is actively written; a pending/scheduled
//     task has none.

const (
	// defaultWorkspaceDownloadMaxBytes caps a single browser download / ZIP export
	// at 50 MiB by default. A larger artifact must be retrieved with the CLI. The
	// cap is overridable via FLEET_WORKSPACE_DOWNLOAD_MAX_BYTES.
	defaultWorkspaceDownloadMaxBytes int64 = 50 << 20

	// maxWorkspaceListEntries bounds the directory walk so a pathological
	// workspace (an agent that wrote a million files) can't make the listing
	// endpoint allocate without limit. The listing is truncated past this.
	maxWorkspaceListEntries = 10000
)

// workspaceEntry is one row in the workspace listing. Name is the slash-joined
// path relative to the workspace root (directories carry a trailing slash, like
// the issue's response shape).
type workspaceEntry struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
	IsDir      bool      `json:"is_dir"`
}

// workspaceListResponse is the GET /tasks/{id}/workspace body.
type workspaceListResponse struct {
	Files         []workspaceEntry `json:"files"`
	WorkspacePath string           `json:"workspace_path"`
	Truncated     bool             `json:"truncated,omitempty"`
}

// workspaceDownloadMaxBytes resolves the configured per-download size cap,
// honouring FLEET_WORKSPACE_DOWNLOAD_MAX_BYTES (a positive integer byte count).
// A missing / invalid / non-positive value falls back to the 50 MiB default.
func workspaceDownloadMaxBytes() int64 {
	if v := strings.TrimSpace(os.Getenv("FLEET_WORKSPACE_DOWNLOAD_MAX_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultWorkspaceDownloadMaxBytes
}

// taskWorkspaceOwned reports whether the request principal may read the given
// task's workspace files. This is STRICTER than taskVisibleToScopes: a workspace
// is private to its creator. Admin always wins; a user principal must be the
// creating user; an API-key principal must be the creating key.
func taskWorkspaceOwned(p principal, task *models.Task) bool {
	if p.isAdmin {
		return true
	}
	if p.user != nil && task.CreatedBy != nil && *task.CreatedBy == p.user.ID {
		return true
	}
	if p.apiKey != nil && task.CreatedByKeyID != nil && p.apiKey.KeyID == *task.CreatedByKeyID {
		return true
	}
	return false
}

// loadWorkspaceTask resolves + authorizes the task for a workspace request and
// returns its resolved, symlink-evaluated workspace ROOT. It writes the HTTP
// error and returns ok=false on any failure (bad ID, not found, forbidden, no
// workspace recorded, task not yet complete, root missing). On success root is
// safe to use as the base for tools.SafeWorkspaceJoin.
func (h *Handlers) loadWorkspaceTask(w http.ResponseWriter, r *http.Request) (task *models.Task, root string, ok bool) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewLogs) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return nil, "", false
	}

	taskIDStr := chi.URLParam(r, "task_id")
	taskID, err := uuid.Parse(taskIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return nil, "", false
	}

	task, err = h.storage.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return nil, "", false
	}

	// Ownership gate — stricter than ordinary task visibility (see doc above).
	if !taskWorkspaceOwned(p, task) {
		writeError(w, http.StatusForbidden, "Workspace files are private to the task creator")
		return nil, "", false
	}

	if task.WorkspacePath == nil || strings.TrimSpace(*task.WorkspacePath) == "" {
		writeError(w, http.StatusNotFound, "No workspace recorded for this task")
		return nil, "", false
	}

	// Only completed tasks expose their workspace: a running task's workspace is
	// mid-write, and pending/scheduled tasks have none yet.
	if task.Status != models.TaskStatusSuccess && task.Status != models.TaskStatusError {
		writeError(w, http.StatusConflict, "Workspace is only available for completed tasks")
		return nil, "", false
	}

	// Resolve the root's own symlinks once; EvalSymlinks failing here means the
	// directory is gone (cleaned up / never created) — surface as 404 rather than
	// leaking a filesystem error.
	rootAbs, err := filepath.Abs(*task.WorkspacePath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to resolve workspace path")
		return nil, "", false
	}
	root, err = filepath.EvalSymlinks(rootAbs)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "Workspace directory no longer exists")
			return nil, "", false
		}
		writeError(w, http.StatusInternalServerError, "Failed to resolve workspace path")
		return nil, "", false
	}
	return task, root, true
}

// TaskWorkspace handles GET /tasks/{task_id}/workspace. With ?format=zip it
// streams the whole workspace as a ZIP archive; otherwise it returns the JSON
// directory listing.
func (h *Handlers) TaskWorkspace(w http.ResponseWriter, r *http.Request) {
	task, root, ok := h.loadWorkspaceTask(w, r)
	if !ok {
		return
	}
	if strings.EqualFold(r.URL.Query().Get("format"), "zip") {
		h.streamWorkspaceZip(w, r, task, root)
		return
	}
	h.listWorkspace(w, r, root)
}

// listWorkspace walks the workspace root and returns the file/dir inventory. The
// walk skips entries that resolve (via symlink) outside the root — defence in
// depth so a planted symlink dir can't make the listing recurse off into the
// host filesystem.
func (h *Handlers) listWorkspace(w http.ResponseWriter, _ *http.Request, root string) {
	files := make([]workspaceEntry, 0, 16)
	truncated := false
	//nolint:gosec // G703: root is the symlink-resolved workspace dir from loadWorkspaceTask (a DB-stored path), not raw request input; each entry is re-validated by SafeWorkspaceJoin below.
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries; a single bad file must not abort the listing
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil || rel == "." {
			return nil //nolint:nilerr // an un-relativizable/self path is skipped, not fatal to the listing
		}
		if len(files) >= maxWorkspaceListEntries {
			truncated = true
			return fs.SkipAll
		}
		// Defence in depth: never descend through a symlink that escapes the root.
		// SafeWorkspaceJoin re-validates the entry against the root; an escaping or
		// unreadable entry is skipped (and, if a dir, not descended into).
		if _, jerr := tools.SafeWorkspaceJoin(root, rel); jerr != nil {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil //nolint:nilerr // a single stat failure is skipped, not fatal to the listing
		}
		name := filepath.ToSlash(rel)
		size := info.Size()
		if d.IsDir() {
			name += "/"
			size = 0
		}
		files = append(files, workspaceEntry{
			Name:       name,
			SizeBytes:  size,
			ModifiedAt: info.ModTime().UTC(),
			IsDir:      d.IsDir(),
		})
		return nil
	})

	writeJSON(w, http.StatusOK, workspaceListResponse{
		Files:         files,
		WorkspacePath: root,
		Truncated:     truncated,
	})
}

// TaskWorkspaceFile handles GET /tasks/{task_id}/workspace/{path...}. It streams
// a single file from the task's workspace, enforcing the shared path-traversal
// guard and a size cap.
func (h *Handlers) TaskWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	_, root, ok := h.loadWorkspaceTask(w, r)
	if !ok {
		return
	}

	// The trailing path arrives as a chi catch-all ("*"); percent-decode it before
	// the guard so spaces/parens in agent-generated filenames resolve.
	rel := chi.URLParam(r, "*")
	decoded, err := url.PathUnescape(rel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	rel = decoded

	resolved, err := tools.SafeWorkspaceJoin(root, rel)
	switch {
	case errors.Is(err, tools.ErrUnsafePath):
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	case errors.Is(err, tools.ErrPathEscapesWorkspace):
		writeError(w, http.StatusBadRequest, "path escapes workspace")
		return
	case os.IsNotExist(err):
		writeError(w, http.StatusNotFound, "file not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to resolve path")
		return
	}

	info, err := os.Stat(resolved) //nolint:gosec // resolved is validated by tools.SafeWorkspaceJoin to live under the workspace root
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "file not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to stat file")
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "path is a directory; use ?format=zip to download the workspace")
		return
	}

	if maxBytes := workspaceDownloadMaxBytes(); info.Size() > maxBytes {
		taskIDStr := chi.URLParam(r, "task_id")
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"file too large for browser download (%d bytes); use the CLI: fleet task workspace get %s %s",
			info.Size(), taskIDStr, filepath.ToSlash(rel)))
		return
	}

	// Content-Type from extension; ServeContent's sniffing covers the rest.
	if ctype := mime.TypeByExtension(filepath.Ext(resolved)); ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	// Force a download (not inline render) with a safe filename. sanitizeFilename
	// (shared with the upload path) maps anything outside [A-Za-z0-9._-] to "_", so
	// CR/LF/quotes cannot inject extra headers; fall back to a generic name if it
	// sanitizes to empty.
	dlName := sanitizeFilename(filepath.Base(resolved))
	if dlName == "" {
		dlName = "download"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", dlName))

	f, err := os.Open(resolved) //nolint:gosec // resolved is validated by SafeWorkspaceJoin to live under the workspace root
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to open file")
		return
	}
	defer func() { _ = f.Close() }()
	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), f)
}

// streamWorkspaceZip streams the whole workspace as a ZIP archive. It honours the
// same cap on TOTAL UNCOMPRESSED size (summed before streaming) so a giant
// workspace can't be pulled through the browser. The archive is streamed
// directly to the response writer without buffering it in RAM.
func (h *Handlers) streamWorkspaceZip(w http.ResponseWriter, _ *http.Request, task *models.Task, root string) {
	// Pre-flight: sum regular-file sizes (re-validated through the guard) and
	// reject before writing any bytes if the total exceeds the cap. Done first so
	// the 413 carries a JSON body rather than a half-written ZIP.
	maxBytes := workspaceDownloadMaxBytes()
	var total int64
	type zipItem struct {
		rel  string
		full string
		info fs.FileInfo
	}
	var items []zipItem
	//nolint:gosec // G703: root is the symlink-resolved workspace dir from loadWorkspaceTask (a DB-stored path), not raw request input; each entry is re-validated by SafeWorkspaceJoin below.
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil //nolint:nilerr // a single un-relativizable path is skipped, not fatal to the archive
		}
		full, jerr := tools.SafeWorkspaceJoin(root, rel)
		if jerr != nil {
			return nil //nolint:nilerr // skip anything that fails the guard (escaping symlink etc.); never abort the walk
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil //nolint:nilerr // a single stat failure is skipped, not fatal to the archive
		}
		total += info.Size()
		if total > maxBytes {
			return errZipTooLarge
		}
		items = append(items, zipItem{rel: filepath.ToSlash(rel), full: full, info: info})
		return nil
	})
	if errors.Is(walkErr, errZipTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"workspace too large for browser download (>%d bytes uncompressed); use the CLI: fleet task workspace get %s",
			maxBytes, task.ID.String()))
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("task-%s-workspace.zip", task.ID.String())))

	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()
	for _, it := range items {
		hdr, herr := zip.FileInfoHeader(it.info)
		if herr != nil {
			return
		}
		hdr.Name = it.rel
		hdr.Method = zip.Deflate
		entry, eerr := zw.CreateHeader(hdr)
		if eerr != nil {
			return
		}
		src, oerr := os.Open(it.full)
		if oerr != nil {
			return
		}
		_, cerr := io.Copy(entry, src)
		_ = src.Close()
		if cerr != nil {
			return
		}
	}
}

// errZipTooLarge is the sentinel WalkDir returns to abort the ZIP pre-flight once
// the uncompressed-size cap is exceeded.
var errZipTooLarge = errors.New("workspace zip exceeds size cap")
