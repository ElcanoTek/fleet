// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

const maxUploadSize = 250 * 1024 * 1024 // 250MB

// HandleUpload handles file uploads.
func (h *Handlers) HandleUpload(w http.ResponseWriter, r *http.Request) {
	// Authentication
	// Check for Admin API Key or User Token
	// This mirrors logic in CreateTask but slightly simpler as we just need "any valid user"

	isAuthed := false

	// 1. API Key
	if h.verifyAdminKey(r) {
		isAuthed = true
	} else {
		// 2. User Token
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			token := ""
			if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
				token = authHeader[7:]
			} else {
				token = authHeader
			}
			user, err := h.storage.GetUserByToken(token)
			if err == nil && user != nil {
				isAuthed = true
			}
		}

		// 3. Elcano unified-auth cookie (scoped tier): verify natively, then
		// require the email to be a provisioned user. Mirrors CreateTask.
		if !isAuthed {
			if sess := h.elcanoSessionFromRequest(r); sess != nil {
				u, err := h.lookupMember(r.Context(), sess.Email)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					writeError(w, http.StatusInternalServerError, "Membership check failed")
					return
				}
				if u == nil {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "not_a_member"})
					return
				}
				isAuthed = true
			}
		}
	}

	if !isAuthed {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		writeError(w, http.StatusBadRequest, "File too large")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid file")
		return
	}
	defer file.Close()

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
		log.Printf("Failed to create checksums dir: %v", err)
		// Continue, just won't be able to save checksums
	}

	// Generate unique filename preserving original name with short UUID suffix
	// This helps users identify files while avoiding collisions
	originalName := sanitizeFilename(header.Filename)
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

	dst, err := os.Create(path) //nolint:gosec // G304: filename is sanitized (sanitizeFilename allowlist) AND the resolved path is asserted within tempDir via withinDir/filepath.Rel.
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to save file")
		return
	}
	defer dst.Close()

	// Calculate checksum while writing
	hasher := sha256.New()
	writer := io.MultiWriter(dst, hasher)

	size, err := io.Copy(writer, file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to write file")
		return
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))

	// Save checksum to sidecar file in the dedicated checksums directory
	checksumPath := filepath.Join(tempDir, ".checksums", filename+".sha256")
	if err := os.WriteFile(checksumPath, []byte(checksum), 0600); err != nil {
		// Non-critical error, just log it
		log.Printf("Failed to save checksum sidecar for %s: %v", filename, err)
	}

	log.Printf("File uploaded: %s (size: %d, checksum: %s)", filename, size, checksum)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"filename":      filename,
		"original_name": originalName,
		"checksum":      checksum,
		"size":          size,
	})
}

// HandleDownload handles file downloads for runners.
func (h *Handlers) HandleDownload(w http.ResponseWriter, r *http.Request) {
	// Require authentication for file downloads
	isAdmin := h.verifyAdminKey(r)
	var node *models.Node
	var nodeErr error

	if !isAdmin {
		node, nodeErr = h.verifyNodeKey(r)
		if nodeErr != nil {
			writeError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
	}

	filename := chi.URLParam(r, "filename")
	// Sanitize filename to prevent path traversal and ensure consistent naming
	filename = sanitizeFilename(filename)

	if filename == "" {
		writeError(w, http.StatusBadRequest, "Invalid filename")
		return
	}

	if node != nil && !isAdmin {
		allowed, err := h.nodeCanAccessFile(r.Context(), node.ID, filename)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to authorize file access")
			return
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "File access denied")
			return
		}
	}

	uploadsDir := filepath.Join(h.config.DataDir, "temp_uploads")
	path := filepath.Join(uploadsDir, filename)
	if !withinDir(uploadsDir, path) {
		writeError(w, http.StatusBadRequest, "Invalid filename")
		return
	}
	if _, err := os.Stat(path); os.IsNotExist(err) { //nolint:gosec // G703: filename is sanitized (sanitizeFilename) AND the resolved path is asserted within uploadsDir via withinDir/filepath.Rel.
		writeError(w, http.StatusNotFound, "File not found")
		return
	}

	// Calculate and set checksum header
	checksum, err := getFileChecksum(h.config.DataDir, filename)
	if err == nil {
		w.Header().Set("X-Checksum-SHA256", checksum)
	}

	// Serve the file
	http.ServeFile(w, r, path) //nolint:gosec // G703: path is asserted within uploadsDir via withinDir/filepath.Rel above; filename is sanitized.
}

// CleanupTempFiles removes files older than the specified duration.
func (h *Handlers) CleanupTempFiles(maxAge time.Duration) {
	tempDir := filepath.Join(h.config.DataDir, "temp_uploads")

	// Safety: clear the checksum cache periodically to prevent any possibility of memory leaks
	// or stale entries accumulating over long periods (since files are temp anyway).
	// This is a simple and effective strategy since checksums are cheap to re-read from sidecar files.
	h.checksumCache.Clear()

	err := filepath.Walk(tempDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Log but continue walking - we want to clean up as many files as possible
			log.Printf("Error accessing temp file %s: %v", path, err)
			return nil
		}
		if !info.IsDir() && time.Since(info.ModTime()) > maxAge {
			//nolint:gosec // G122: this walks the server's own DataDir/temp_uploads (operator-owned), removing aged temp files during scheduled cleanup. The path is produced by filepath.Walk over a server-controlled root, not by request input; symlink-TOCTOU is not a meaningful vector here.
			if err := os.Remove(path); err != nil {
				log.Printf("Failed to remove old temp file %s: %v", path, err)
			} else {
				log.Printf("Removed old temp file: %s", path)
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("Error cleaning up temp files: %v", err)
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
func getFileChecksum(dataDir, filename string) (string, error) {
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
			//nolint:gosec // G706: filename is sanitized via logSafe (strips CR/LF) and already passed sanitizeFilename; gosec's taint tracker cannot see through the helper.
			log.Printf("Failed to save checksum sidecar for %s: %v", logSafe(filename), err)
		}
	}

	return checksum, nil
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

func (h *Handlers) nodeCanAccessFile(ctx context.Context, nodeID uuid.UUID, filename string) (bool, error) {
	return h.storage.CanNodeAccessFileWithContext(ctx, nodeID, filename)
}
