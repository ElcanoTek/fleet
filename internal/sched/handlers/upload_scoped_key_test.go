// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// setupUploadTest mirrors setupTest but also returns the key manager so tests
// can mint scoped keys against the SAME manager the handlers consult.
func setupUploadTest(t *testing.T) (*Handlers, *apikeys.Manager) {
	t.Helper()
	tmpDir := t.TempDir()
	store := storage.New()
	if err := store.Initialize(filepath.Join(tmpDir, "test.db"), storage.DefaultPoolConfig()); err != nil {
		if isDatabaseUnavailable(err) {
			t.Skipf("Skipping tests: database unavailable: %v", err)
		}
		t.Fatalf("Failed to init storage: %v", err)
	}
	acquireTestLock(t, store)
	if err := cleanDB(store); err != nil {
		t.Fatalf("Failed to clean database: %v", err)
	}
	keyMgr, err := apikeys.NewManager(filepath.Join(tmpDir, "keys.json"), filepath.Join(tmpDir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	h := New(Config{AdminAPIKey: "admin-key", DataDir: tmpDir}, store, keyMgr)
	return h, keyMgr
}

func multipartUploadRequest(t *testing.T, apiKey string) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "brief.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("task brief")); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	req := httptest.NewRequest("POST", "/upload", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	return req
}

// TestUpload_ScopedCreateTaskKey pins #719: a scoped API key carrying
// create_task permission may stage uploads (the same authority the create
// paths honor), so an external intake app can attach files without holding the
// full-access admin key.
func TestUpload_ScopedCreateTaskKey(t *testing.T) {
	h, keyMgr := setupUploadTest(t)

	_, raw, err := keyMgr.CreateKey("intake", nil, []models.Permission{models.PermissionCreateTask}, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	w := httptest.NewRecorder()
	h.HandleUpload(w, multipartUploadRequest(t, raw))
	if w.Code != http.StatusOK {
		t.Errorf("scoped create_task key: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// TestUpload_UnderScopedTypedKey pins the #190-style definitive refusal on the
// upload path: a VALID typed key without create_task permission (readonly) is
// 403 (wrong scope), not 401 (bad credentials).
func TestUpload_UnderScopedTypedKey(t *testing.T) {
	h, keyMgr := setupUploadTest(t)

	_, raw, err := keyMgr.CreateTypedKey("viewer", apikeys.KeyTypeReadonly, nil, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateTypedKey: %v", err)
	}

	w := httptest.NewRecorder()
	h.HandleUpload(w, multipartUploadRequest(t, raw))
	if w.Code != http.StatusForbidden {
		t.Errorf("readonly typed key: got %d, want 403 (%s)", w.Code, w.Body.String())
	}
}

// TestUpload_InvalidScopedKeyStays401 keeps the existing contract: a garbage
// key is still an authentication failure, not a scope failure.
func TestUpload_InvalidScopedKeyStays401(t *testing.T) {
	h, _ := setupUploadTest(t)

	w := httptest.NewRecorder()
	h.HandleUpload(w, multipartUploadRequest(t, "sk-not-a-real-key"))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("invalid key: got %d, want 401 (%s)", w.Code, w.Body.String())
	}
}
