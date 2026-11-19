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

	"github.com/ElcanoTek/fleet/internal/config/envreg"
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
//   - Path traversal. The task's directory is opened without following a
//     symbolic link at any level below the workspace root (tools.OpenRunDir — a
//     whole-root sandbox can rename a worktree directory and leave a link in
//     its place), and every per-file access is resolved and opened through
//     that open directory (tools.WorkspaceDir.Resolve, tools.OpenNoFollow): it
//     rejects "..", absolute paths, and NUL bytes up front, and follows a link
//     only while it stays inside the directory. Symlinks the agent planted that
//     point outside the workspace (including the structural
//     protocols/personas/system_prompts/skills links in a chat-style workspace)
//     are therefore NOT followable through this endpoint, and nothing swapped
//     in after the resolution is followed either.
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
	if v := strings.TrimSpace(envreg.Get("FLEET_WORKSPACE_DOWNLOAD_MAX_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultWorkspaceDownloadMaxBytes
}

// taskWorkspaceOwned reports whether the request principal may read the given
// task's workspace files. A workspace is private to its creator. Admin always wins; otherwise the principal must be
// the task's creator (taskCreatedByPrincipal — the shared ownership predicate
// the run-log gate also uses).
//
// It stays stricter than the run-log gate on purpose: the fleet-wide
// PermissionViewAllLogs (#980) opens transcripts to a designated auditor, not
// the file bytes a run produced. Widening workspace access is a separate
// decision, and this is deliberately not it.
func taskWorkspaceOwned(p principal, task *models.Task) bool {
	if p.isAdmin {
		return true
	}
	return taskCreatedByPrincipal(p, task)
}

// loadWorkspaceTask resolves + authorizes the task for a workspace request and
// returns its workspace directory, opened without following a symbolic link at
// any level below the workspace root. It writes the HTTP error and returns
// ok=false on any failure (bad ID, not found, forbidden, no workspace recorded,
// task not yet complete, directory missing or a symbolic link). On success the
// caller closes dir and does every filesystem access through it.
func (h *Handlers) loadWorkspaceTask(w http.ResponseWriter, r *http.Request) (task *models.Task, dir *tools.WorkspaceDir, ok bool) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewLogs) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return nil, nil, false
	}

	taskIDStr := chi.URLParam(r, "task_id")
	taskID, err := uuid.Parse(taskIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return nil, nil, false
	}

	task, err = h.storage.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return nil, nil, false
	}

	// Ownership gate — stricter than ordinary task visibility (see doc above).
	if !taskWorkspaceOwned(p, task) {
		writeError(w, http.StatusForbidden, "Workspace files are private to the task creator")
		return nil, nil, false
	}

	if task.WorkspacePath == nil || strings.TrimSpace(*task.WorkspacePath) == "" {
		writeError(w, http.StatusNotFound, "No workspace recorded for this task")
		return nil, nil, false
	}

	// Only completed tasks expose their workspace: a running task's workspace is
	// mid-write, and pending/scheduled tasks have none yet.
	if task.Status != models.TaskStatusSuccess && task.Status != models.TaskStatusError {
		writeError(w, http.StatusConflict, "Workspace is only available for completed tasks")
		return nil, nil, false
	}

	// Open the directory once, without following a symbolic link below the
	// workspace root: a missing directory (cleaned up / never created) or one
	// that is a symbolic link surfaces as 404 rather than a filesystem error.
	rootAbs, err := filepath.Abs(*task.WorkspacePath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to resolve workspace path")
		return nil, nil, false
	}
	dir, err = tools.OpenRunDir(r.Context(), tools.WorkspaceDirForConversation(""), rootAbs)
	if err != nil {
		if errors.Is(err, tools.ErrWorkspaceDirLink) || os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "Workspace directory no longer exists")
			return nil, nil, false
		}
		writeError(w, http.StatusInternalServerError, "Failed to resolve workspace path")
		return nil, nil, false
	}
	return task, dir, true
}

// TaskWorkspace handles GET /tasks/{task_id}/workspace. With ?format=zip it
// streams the whole workspace as a ZIP archive; otherwise it returns the JSON
// directory listing.
func (h *Handlers) TaskWorkspace(w http.ResponseWriter, r *http.Request) {
	task, dir, ok := h.loadWorkspaceTask(w, r)
	if !ok {
		return
	}
	defer func() { _ = dir.Close() }()
	if strings.EqualFold(r.URL.Query().Get("format"), "zip") {
		h.streamWorkspaceZip(w, r, task, dir)
		return
	}
	h.listWorkspace(w, r, dir)
}

// listWorkspace walks the opened workspace directory and returns the file/dir
// inventory. The walk never descends a symbolic link, and an entry that does
// not resolve inside the directory (a link out of it) is skipped — defence in
// depth so a planted symlink can't make the listing reach the host filesystem.
func (h *Handlers) listWorkspace(w http.ResponseWriter, r *http.Request, dir *tools.WorkspaceDir) {
	files := make([]workspaceEntry, 0, 16)
	truncated := false
	_ = fs.WalkDir(dir.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries; a single bad file must not abort the listing
		}
		if rel == "." {
			return nil
		}
		if len(files) >= maxWorkspaceListEntries {
			truncated = true
			return fs.SkipAll
		}
		// Defence in depth: an entry that does not resolve inside the
		// directory (a link out of it) or cannot be read is skipped (and, if a
		// dir, not descended into).
		if _, rerr := dir.Resolve(rel); rerr != nil {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil //nolint:nilerr // a single stat failure is skipped, not fatal to the listing
		}
		name := rel
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

	writeJSON(w, r, http.StatusOK, workspaceListResponse{
		Files:         files,
		WorkspacePath: dir.RealPath(),
		Truncated:     truncated,
	})
}

// TaskWorkspaceFile handles GET /tasks/{task_id}/workspace/{path...}. It streams
// a single file from the task's workspace, enforcing the shared path-traversal
// guard and a size cap.
func (h *Handlers) TaskWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	_, dir, ok := h.loadWorkspaceTask(w, r)
	if !ok {
		return
	}
	defer func() { _ = dir.Close() }()

	// The trailing path arrives as a chi catch-all ("*"); percent-decode it before
	// the guard so spaces/parens in agent-generated filenames resolve.
	rel := chi.URLParam(r, "*")
	decoded, err := url.PathUnescape(rel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	rel = decoded

	resolved, err := dir.Resolve(rel)
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
	if resolved == "." {
		writeError(w, http.StatusBadRequest, "path is a directory; use ?format=zip to download the workspace")
		return
	}

	// The resolved name has no link in it; a live sandbox can still change
	// the workspace afterwards, so open it refusing a link at any component
	// and check the opened descriptor rather than the name.
	f, info, err := tools.OpenNoFollow(dir.Root, resolved)
	switch {
	case errors.Is(err, tools.ErrNotRegularFile) && info != nil && info.IsDir():
		writeError(w, http.StatusBadRequest, "path is a directory; use ?format=zip to download the workspace")
		return
	case errors.Is(err, tools.ErrNotRegularFile):
		writeError(w, http.StatusBadRequest, "path is not a regular file")
		return
	case os.IsNotExist(err):
		writeError(w, http.StatusNotFound, "file not found")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "file unavailable")
		return
	}
	defer func() { _ = f.Close() }()

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

	// Serve exactly the size checked above, even if the file grows meanwhile.
	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), io.NewSectionReader(f, 0, info.Size()))
}

// streamWorkspaceZip streams the whole workspace as a ZIP archive. It honours the
// same cap on TOTAL UNCOMPRESSED size (summed before streaming) so a giant
// workspace can't be pulled through the browser. The archive is streamed
// directly to the response writer without buffering it in RAM.
func (h *Handlers) streamWorkspaceZip(w http.ResponseWriter, _ *http.Request, task *models.Task, dir *tools.WorkspaceDir) {
	// Pre-flight: sum regular-file sizes (re-validated through the guard) and
	// reject before writing any bytes if the total exceeds the cap. Done first so
	// the 413 carries a JSON body rather than a half-written ZIP.
	maxBytes := workspaceDownloadMaxBytes()
	var total int64
	type zipItem struct {
		rel      string
		resolved string
		size     int64
		info     fs.FileInfo
	}
	// Every entry is resolved inside the opened directory and later opened
	// with no link followed, so neither the pre-flight nor the copy can follow
	// a link out of the workspace, even one that appears after the walk.
	var items []zipItem
	walkErr := fs.WalkDir(dir.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		resolved, rerr := dir.Resolve(rel)
		if rerr != nil || resolved == "." {
			return nil //nolint:nilerr // skip anything that fails the guard (escaping symlink etc.); never abort the walk
		}
		// Size and type of what the entry names (an in-workspace link counts
		// as its target), so the cap sums the bytes that will be copied.
		info, ierr := dir.Stat(resolved)
		if ierr != nil || !info.Mode().IsRegular() {
			return nil //nolint:nilerr // a single stat failure or non-regular entry is skipped, not fatal to the archive
		}
		total += info.Size()
		if total > maxBytes {
			return errZipTooLarge
		}
		items = append(items, zipItem{rel: rel, resolved: resolved, size: info.Size(), info: info})
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
		src, _, oerr := tools.OpenNoFollow(dir.Root, it.resolved)
		if oerr != nil {
			continue // changed since the pre-flight: left out
		}
		hdr, herr := zip.FileInfoHeader(it.info)
		if herr != nil {
			_ = src.Close()
			return
		}
		hdr.Name = filepath.ToSlash(it.rel)
		hdr.Method = zip.Deflate
		entry, eerr := zw.CreateHeader(hdr)
		if eerr != nil {
			_ = src.Close()
			return
		}
		// Copy no more than the pre-flight size, so the archive stays within
		// the cap however the file changes while it is read.
		_, cerr := io.Copy(entry, io.LimitReader(src, it.size))
		_ = src.Close()
		if cerr != nil {
			return
		}
	}
}

// errZipTooLarge is the sentinel WalkDir returns to abort the ZIP pre-flight once
// the uncompressed-size cap is exceeded.
var errZipTooLarge = errors.New("workspace zip exceeds size cap")
