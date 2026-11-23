// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Fallback per-file cap, used only if the config carries no value. The
// real limit is Config.UploadMaxBytes (FLEET_UPLOAD_MAX_BYTES, default
// 1 GiB), shared with the chat plane's /attachments endpoint.
const maxUploadSize = 1 << 30 // 1 GiB

// HandleUpload handles file uploads.
func (h *Handlers) HandleUpload(w http.ResponseWriter, r *http.Request) {
	// Authentication
	// Admin API key, a create_task-scoped key, or a gateway session (the password
	// bearer is retired, #2049 M4).
	// This mirrors logic in CreateTask but slightly simpler as we just need "any valid user"

	isAuthed := false
	identity := "" // the principal's log identity, as the auth middleware sets it
	uploader := "" // who the upload is recorded under (uploaderIdentity); "" for the admin key

	// 1. API Key
	if h.verifyAdminKey(r) {
		isAuthed = true
	} else if user, _, handled := h.userPrincipal(w, r); handled {
		// 1a. The user principal (#2050): /upload sits outside the
		// AdminOrUserAuthMiddleware group like POST /tasks, so the gateway
		// session's principal must be honored here too (an upload is only
		// usable via a subsequent task create). Fail-closed: a refused
		// principal has already written its response.
		if user == nil {
			return
		}
		isAuthed = true
		identity = user.Username
		uploader = uploaderIdentity(&user.ID, nil)
	} else {
		// 1b. Scoped API key carrying create_task permission (#719): an uploaded
		// file is only usable via a subsequent task create, so the SAME scoped
		// keys that may create tasks may stage uploads — an external intake app
		// no longer needs the full-access admin key just to attach a brief.
		// ValidateKey consumes one rate-limit token, matching the create paths.
		// A valid-but-under-scoped typed key (readonly/webhook) gets the same
		// definitive 403 as the create paths (#190 semantics), so the caller
		// sees "wrong scope", not "bad credentials".
		if h.scopedKeyCannotCreate(r) {
			writeError(w, http.StatusForbidden, "insufficient key scope: this key type cannot upload task files")
			return
		}
		if apiKey := r.Header.Get("X-API-Key"); apiKey != "" {
			perm := models.PermissionCreateTask
			if valid, key, _ := h.apiKeys.ValidateKeyOn(r.Context(), apikeys.SurfaceOrchestrator, apiKey, &perm, nil, nil); valid && key != nil {
				isAuthed = true
				identity = apiKeyIdentity(key.KeyID)
				uploader = uploaderIdentity(nil, &key.KeyID)
			}
		}
	}

	if !isAuthed {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	// No middleware put the principal on the context; the log lines below
	// carry it the way they would under one. (r itself is left as is.)
	logCtx := logging.WithUser(r.Context(), identity)

	maxBytes := h.config.UploadMaxBytes
	if maxBytes <= 0 {
		maxBytes = maxUploadSize
	}
	// Allow 1 MiB of multipart framing overhead so a file exactly at the
	// limit still fits in the request. The in-memory threshold stays small
	// (32 MiB) — bigger parts spill to temp files instead of buffering the
	// whole upload in RAM (the old code passed the full cap here).
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil { //nolint:gosec // G120: the body is capped by the http.MaxBytesReader set on the line above (maxBytes + 1 MiB framing); parts past 32 MiB spill to temp files.
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("File is over this server's %d MB upload limit", maxBytes/(1024*1024)))
			return
		}
		writeError(w, http.StatusBadRequest, "Invalid upload")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid file")
		return
	}
	defer file.Close()
	if header.Size > maxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("%q is over this server's %d MB upload limit", header.Filename, maxBytes/(1024*1024)))
		return
	}

	// Create temp directory if not exists
	tempDir := filepath.Join(h.config.DataDir, "temp_uploads")
	// Sentinel: Use restrictive permissions (0700) to prevent other users on the system from reading uploaded files.
	// This mitigates local information disclosure risks.
	if err := os.MkdirAll(tempDir, 0700); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create temp dir")
		return
	}

	// Create checksums directory (hidden/system directory to prevent collisions)
	checksumsDir := filepath.Join(tempDir, ".checksums")
	if err := os.MkdirAll(checksumsDir, 0700); err != nil {
		logging.From(logCtx).Error("Failed to create checksums dir", "err", err)
		// Continue, just won't be able to save checksums
	}

	// Generate unique filename preserving original name with short UUID suffix
	// This helps users identify files while avoiding collisions
	originalName := uploadFilename(header.Filename)
	if originalName == "" {
		writeError(w, http.StatusBadRequest, "Invalid filename")
		return
	}
	ext := filepath.Ext(originalName)
	baseName := strings.TrimSuffix(originalName, ext)
	// Use first 8 characters of UUID for brevity
	shortUUID := uuid.New().String()[:8]
	filename := fmt.Sprintf("%s_%s%s", baseName, shortUUID, ext)
	path := filepath.Join(tempDir, filename)
	if !withinDir(tempDir, path) {
		writeError(w, http.StatusBadRequest, "Invalid filename")
		return
	}

	size, checksum, err := writeUploadFile(path, file)
	if err != nil {
		logging.From(logCtx).Error("Failed to save upload", "filename", safe.LogSafe(filename), "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to save file")
		return
	}

	// Save checksum to sidecar file in the dedicated checksums directory
	checksumPath := filepath.Join(tempDir, ".checksums", filename+".sha256")
	if err := os.WriteFile(checksumPath, []byte(checksum), 0600); err != nil { //nolint:gosec // G703: filename is IsLocal-gated + uploadFilename-reduced, and the upload path was asserted within tempDir via withinDir.
		// Non-critical error, just log it
		logging.From(logCtx).Error("Failed to save checksum sidecar", "filename", safe.LogSafe(filename), "err", err)
	}

	// Record who uploaded it: a non-admin may name only its own uploads in a
	// task (checkCallerFiles), so an upload whose uploader cannot be recorded
	// would be unusable — remove it and fail the request instead.
	if err := recordUploader(tempDir, filename, uploader); err != nil {
		logging.From(logCtx).Error("Failed to record uploader", "filename", safe.LogSafe(filename), "err", err)
		_ = os.Remove(path)         //nolint:gosec // G703: path was asserted within tempDir via withinDir above.
		_ = os.Remove(checksumPath) //nolint:gosec // G703: filename is IsLocal-gated + uploadFilename-reduced, and the upload path was asserted within tempDir via withinDir.
		writeError(w, http.StatusInternalServerError, "Failed to save file")
		return
	}

	logging.From(logCtx).Info("File uploaded", "filename", safe.LogSafe(filename), "size_bytes", size, "checksum", safe.LogSafe(checksum))

	writeJSON(w, r, http.StatusOK, map[string]interface{}{
		"filename":      filename,
		"original_name": originalName,
		"checksum":      checksum,
		"size":          size,
	})
}

// writeUploadFile streams src to path, returning the byte count and the
// SHA-256 of what was written. The file is closed — and the close error
// checked — BEFORE success is reported: a deferred Close cannot fail the
// response, and on a disk-full or quota error the close is where the write
// actually fails, so the client used to be told its file was staged while the
// bytes never landed. On any failure the partial file is removed, so a
// half-written upload cannot linger under the name a task would attach.
func writeUploadFile(path string, src io.Reader) (size int64, checksum string, err error) {
	dst, err := os.Create(path) //nolint:gosec // G304: the caller sanitized the filename (uploadFilename: the sanitizeFilename allowlist) AND asserted the resolved path is within the uploads dir via withinDir/filepath.Rel.
	if err != nil {
		return 0, "", err
	}
	hasher := sha256.New()
	size, err = io.Copy(io.MultiWriter(dst, hasher), src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path) //nolint:gosec // G703: path was asserted within tempDir via withinDir by the sole caller before the file was created.
		return 0, "", err
	}
	return size, hex.EncodeToString(hasher.Sum(nil)), nil
}

// HandleDownload serves an uploaded task-input file. With the worker-node
// registry removed (#459) the legacy node-key download path is gone: the
// in-process worker reads inputs directly from the shared data dir, so this
// endpoint is now admin-only (operators inspecting uploaded artifacts).
func (h *Handlers) HandleDownload(w http.ResponseWriter, r *http.Request) {
	if !h.verifyAdminKey(r) {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	filename := chi.URLParam(r, "filename")
	// Sanitize filename to prevent path traversal and ensure consistent naming
	filename = sanitizeFilename(filename)

	if filename == "" {
		writeError(w, http.StatusBadRequest, "Invalid filename")
		return
	}
	// filepath.IsLocal confines the (already sanitizeFilename-reduced) name to a
	// single component under uploadsDir — no absolute path, "..", or separator can
	// escape. It is also the barrier CodeQL's path-injection query recognizes, so
	// the os.Stat / http.ServeFile below clear on rescan. withinDir stays as a
	// resolved-path defense-in-depth net.
	if !filepath.IsLocal(filename) {
		writeError(w, http.StatusBadRequest, "Invalid filename")
		return
	}

	uploadsDir := filepath.Join(h.config.DataDir, "temp_uploads")
	path := filepath.Join(uploadsDir, filename)
	if !withinDir(uploadsDir, path) {
		writeError(w, http.StatusBadRequest, "Invalid filename")
		return
	}
	if _, err := os.Stat(path); os.IsNotExist(err) { //nolint:gosec // G703: filename is IsLocal-gated + sanitizeFilename-reduced AND the resolved path is asserted within uploadsDir via withinDir/filepath.Rel.
		writeError(w, http.StatusNotFound, "File not found")
		return
	}

	// Calculate and set checksum header
	checksum, err := getFileChecksum(r.Context(), h.config.DataDir, filename)
	if err == nil {
		w.Header().Set("X-Checksum-SHA256", checksum)
	}

	// Serve the file
	http.ServeFile(w, r, path) //nolint:gosec // G703: path is asserted within uploadsDir via withinDir/filepath.Rel above; filename is IsLocal-gated + sanitized.
}

// CleanupTempFiles removes files older than the specified duration.
func (h *Handlers) CleanupTempFiles(maxAge time.Duration) {
	tempDir := filepath.Join(h.config.DataDir, "temp_uploads")

	// A missing temp_uploads root means nothing was ever staged — "nothing to
	// clean", not an error: logging it produced an hourly ERROR on every box
	// that has never received an upload. Mirrors worktree.PruneStale's
	// missing-dir early return. A stat failure that is NOT absence (broken
	// permissions, dangling symlink) still logs — that one an operator must
	// see.
	if _, err := os.Stat(tempDir); err != nil {
		if !os.IsNotExist(err) {
			slog.Error("Error accessing temp uploads dir", "dir", safe.LogSafe(tempDir), "err", err)
		}
		return
	}

	err := filepath.Walk(tempDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Log but continue walking - we want to clean up as many files as possible
			slog.Error("Error accessing temp file", "path", safe.LogSafe(path), "err", err)
			return nil
		}
		if !info.IsDir() && time.Since(info.ModTime()) > maxAge {
			//nolint:gosec // G122: this walks the server's own DataDir/temp_uploads (operator-owned), removing aged temp files during scheduled cleanup. The path is produced by filepath.Walk over a server-controlled root, not by request input; symlink-TOCTOU is not a meaningful vector here.
			if err := os.Remove(path); err != nil {
				slog.Error("Failed to remove old temp file", "path", safe.LogSafe(path), "err", err)
			} else {
				slog.Info("Removed old temp file", "path", safe.LogSafe(path))
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("Error cleaning up temp files", "err", err)
	}
}

// calculateFileChecksum calculates the SHA-256 checksum of a file. The only
// caller (getFileChecksum) asserts path containment within temp_uploads via
// withinDir/filepath.Rel before calling.
func calculateFileChecksum(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is validated within temp_uploads by the sole caller (getFileChecksum) via withinDir/filepath.Rel.
	if err != nil {
		return "", err
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// getFileChecksum returns the checksum for a file in the temp_uploads directory.
// It checks for a cached sidecar file first, falling back to calculation if needed.
func getFileChecksum(ctx context.Context, dataDir, filename string) (string, error) {
	// Security: validate filename to prevent path traversal
	if strings.Contains(filename, "..") || strings.Contains(filename, "/") || strings.Contains(filename, "\\") {
		return "", fmt.Errorf("invalid filename: path traversal not allowed")
	}

	uploadsDir := filepath.Join(dataDir, "temp_uploads")
	path := filepath.Join(uploadsDir, filename)

	// Use a dedicated directory for checksums to prevent collisions
	checksumDir := filepath.Join(uploadsDir, ".checksums")
	checksumPath := filepath.Join(checksumDir, filename+".sha256")

	// Containment gate: both the data file and its sidecar must resolve inside
	// temp_uploads (defense in depth on top of the filename validation above).
	if !withinDir(uploadsDir, path) || !withinDir(checksumDir, checksumPath) {
		return "", fmt.Errorf("invalid filename: path traversal not allowed")
	}

	// Try reading from sidecar file first
	if data, err := os.ReadFile(checksumPath); err == nil { //nolint:gosec // G304: filename is validated (no ../, /, \) and checksumPath is asserted within checksumDir via withinDir/filepath.Rel.
		return string(data), nil
	}

	// Fallback to calculation
	checksum, err := calculateFileChecksum(path)
	if err != nil {
		return "", err
	}

	// Ensure checksums directory exists before writing (checksumDir was already
	// validated within uploadsDir above).
	if err := os.MkdirAll(checksumDir, 0700); err == nil {
		// Cache the result for next time
		if err := os.WriteFile(checksumPath, []byte(checksum), 0600); err != nil { //nolint:gosec // G703: checksumPath is asserted within checksumDir via withinDir/filepath.Rel above.
			logging.From(ctx).Error("Failed to save checksum sidecar", "filename", safe.LogSafe(filename), "err", err)
		}
	}

	return checksum, nil
}

// uploadersDirName is the sidecar directory under temp_uploads that records
// who staged each upload: ".uploaders/<filename>" holds the uploader's
// uploaderIdentity. It sits next to ".checksums" and ages out with the
// uploads (CleanupTempFiles and the admin sweep walk the whole tree).
const uploadersDirName = ".uploaders"

// uploaderIdentity is the form an upload's principal is recorded and compared
// in: "user:<id>" for a session user, "key:<key_id>" for an API key. The admin
// key has no identity here (""): it records nothing, and an admin principal
// may name any staged upload.
func uploaderIdentity(userID *uuid.UUID, keyID *string) string {
	switch {
	case userID != nil:
		return "user:" + userID.String()
	case keyID != nil:
		return "key:" + *keyID
	default:
		return ""
	}
}

// stagedUploadName reports whether name has the shape of a staged upload: one
// local path component with no U+0000 (which no upload name holds, and which
// Postgres refuses in a lookup key, pgtext). Anything else is refused by
// validateTaskCreate, so checkCallerFiles leaves it to that message.
func stagedUploadName(name string) bool {
	return name != "" && !strings.Contains(name, "..") && !strings.ContainsAny(name, "/\\\x00") && filepath.IsLocal(name)
}

// recordUploader writes filename's uploader sidecar. An empty uploader (the
// admin key) records nothing.
func recordUploader(tempDir, filename, uploader string) error {
	if uploader == "" {
		return nil
	}
	dir := filepath.Join(tempDir, uploadersDirName)
	path := filepath.Join(dir, filename)
	if !stagedUploadName(filename) || !withinDir(dir, path) {
		return fmt.Errorf("invalid filename: path traversal not allowed")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(uploader), 0600) //nolint:gosec // G703: filename is a single local component and path is asserted within dir via withinDir.
}

// uploaderOf returns the recorded uploader of a staged upload, or "" when none
// is recorded (an admin-key upload, or one staged before uploads were
// recorded).
func (h *Handlers) uploaderOf(name string) string {
	dir := filepath.Join(h.config.DataDir, "temp_uploads", uploadersDirName)
	path := filepath.Join(dir, name)
	if !stagedUploadName(name) || !withinDir(dir, path) {
		return ""
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: name is a single local component and path is asserted within dir via withinDir.
	if err != nil {
		return ""
	}
	return string(data)
}

// checkCallerFiles decides which staged uploads the caller may name in a
// task's files. The run copies each named upload into its workspace, where the
// task's creator can download it, so naming an upload hands out its bytes. A
// principal holding admin may name any upload. Any other caller may name an
// upload that is
//
//   - already on the task it acts on (kept: the task being edited, or the
//     source of a re-run or clone, which the caller passed the act check for);
//   - recorded as its own (uploaderOf, written by HandleUpload); or
//   - on a task it created (the creating user, or the creating API key), which
//     covers uploads staged before uploaders were recorded.
//
// Any other name is refused with the message a missing upload gets, so the
// answer does not say whether the upload exists. The returned error is a
// *createRefusalError.
func (h *Handlers) checkCallerFiles(ctx context.Context, caller taskCreator, files, kept []string) error {
	if caller.hasAdminPermission || len(files) == 0 {
		return nil
	}
	keep := make(map[string]struct{}, len(kept))
	for _, k := range kept {
		keep[strings.TrimSpace(k)] = struct{}{}
	}
	self := uploaderIdentity(caller.creatorID, caller.creatorKey)
	for _, f := range files {
		name := strings.TrimSpace(f)
		if !stagedUploadName(name) {
			continue // validateTaskCreate refuses it with its own message
		}
		if _, ok := keep[name]; ok {
			continue
		}
		if self != "" && h.uploaderOf(name) == self {
			continue
		}
		onOwnTask, err := h.storage.TaskWithFileCreatedBy(ctx, name, caller.creatorID, caller.creatorKey)
		if err != nil {
			logging.From(ctx).Error("Failed to look up tasks naming an upload", "filename", safe.LogSafe(name), "err", err)
			return &createRefusalError{status: http.StatusInternalServerError, detail: "Failed to look up task"}
		}
		if !onOwnTask {
			return &createRefusalError{status: http.StatusBadRequest, detail: fmt.Sprintf("file not found: %s", name)}
		}
	}
	return nil
}

// uploadRefusals runs checkCallerFiles over the files of many records before
// any is charged or written, for the bulk paths (batch, import). A refused
// upload is that record's failure, by record index; a failed lookup is the
// returned error, which fails the request. A nil entry is a record not
// checked. The caller sets taskGates.uploadsChecked and hands each record's
// result to admitCreate (uploadRefusal), so nothing is looked up twice.
func (h *Handlers) uploadRefusals(ctx context.Context, caller taskCreator, files [][]string) (map[int]error, error) {
	refusals := map[int]error{}
	for i, names := range files {
		if err := h.checkCallerFiles(ctx, caller, names, nil); err != nil {
			if !callerFilesRefused(err) {
				return nil, err
			}
			refusals[i] = err
		}
	}
	return refusals, nil
}

// callerFilesRefused reports whether err from checkCallerFiles is a refusal
// of a named upload (400), as opposed to a failed lookup, which the bulk paths
// surface as a server error rather than a per-record failure.
func callerFilesRefused(err error) bool {
	var refusal *createRefusalError
	return errors.As(err, &refusal) && refusal.status == http.StatusBadRequest
}

// writeCallerFilesRefusal writes checkCallerFiles's error: a refusal or lookup
// failure with the status it carries, anything else as a 500.
func writeCallerFilesRefusal(w http.ResponseWriter, err error) {
	var refusal *createRefusalError
	if errors.As(err, &refusal) {
		writeError(w, refusal.status, refusal.detail)
		return
	}
	writeError(w, http.StatusInternalServerError, "Failed to look up task")
}

// withinDir reports whether target resolves to a location inside base. It uses
// filepath.Rel (NOT a string-prefix check, which is fooled by sibling dirs like
// /data/temp_uploads-evil) and rejects any path whose relative form escapes the
// base via "..". base and target are cleaned/absolutized first. This is the
// containment gate the upload/download path operations assert before touching
// the filesystem, on top of sanitizeFilename's allowlist — defense in depth
// against any future caller that forgets to sanitize.
func withinDir(base, target string) bool {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absBase, absTarget)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// uploadFilename is the name POST /upload stores a file under, before its
// unique suffix: sanitizeFilename, with each run of dots made one dot. Task
// files refuse any name holding "..", and the uploader record shares that
// shape check (stagedUploadName), so an upload named "a..b.pdf" kept as is
// would be staged but could never be attached. Download names (HandleDownload,
// the workspace browser) keep plain sanitizeFilename, so a file staged under
// an older name is still found by that name.
func uploadFilename(filename string) string {
	name := sanitizeFilename(filename)
	for strings.Contains(name, "..") {
		name = strings.ReplaceAll(name, "..", ".")
	}
	return strings.Trim(name, "._-")
}

func sanitizeFilename(filename string) string {
	if filename == "" {
		return ""
	}
	base := filepath.Base(filename)
	if base == "." || base == ".." {
		return ""
	}
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '.' || r == '-' || r == '_':
			return r
		default:
			return '_'
		}
	}, base)
	sanitized = strings.Trim(sanitized, "._-")
	if sanitized == "" {
		return ""
	}
	return sanitized
}
