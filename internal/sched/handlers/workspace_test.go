// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// seedWorkspaceTask creates a completed task whose workspace_path points at a
// freshly-populated temp dir, and returns the task plus the resolved workspace
// root. The router is wired with the same auth middleware + routes as main.go.
func seedWorkspaceTask(t *testing.T, store *storage.Storage, status models.TaskStatus) (*models.Task, string) {
	t.Helper()
	ws := t.TempDir()
	resolved, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatalf("resolve ws: %v", err)
	}
	if err := os.WriteFile(filepath.Join(resolved, "report.md"), []byte("# report\n"), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(resolved, "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(resolved, "data", "raw.csv"), []byte("a,b\n1,2\n"), 0o644); err != nil {
		t.Fatalf("write csv: %v", err)
	}

	task := &models.Task{
		ID:            uuid.New(),
		Prompt:        "produce a report",
		Status:        status,
		CreatedAt:     time.Now().UTC(),
		WorkspacePath: &resolved,
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	return task, resolved
}

func workspaceRouter(h *Handlers) *chi.Mux {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Get("/tasks/{task_id}/workspace", h.TaskWorkspace)
		r.Get("/tasks/{task_id}/workspace/*", h.TaskWorkspaceFile)
	})
	return r
}

func TestWorkspaceList_Admin(t *testing.T) {
	h, store := setupTest(t)
	task, root := seedWorkspaceTask(t, store, models.TaskStatusSuccess)
	r := workspaceRouter(h)

	req := httptest.NewRequest("GET", "/tasks/"+task.ID.String()+"/workspace", nil)
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list: want 200 got %d: %s", w.Code, w.Body.String())
	}
	var resp workspaceListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.WorkspacePath != root {
		t.Errorf("workspace_path = %q want %q", resp.WorkspacePath, root)
	}
	names := map[string]workspaceEntry{}
	for _, e := range resp.Files {
		names[e.Name] = e
	}
	if _, ok := names["report.md"]; !ok {
		t.Errorf("listing missing report.md: %+v", resp.Files)
	}
	if d, ok := names["data/"]; !ok || !d.IsDir {
		t.Errorf("listing missing data/ dir entry: %+v", resp.Files)
	}
	if f, ok := names["data/raw.csv"]; !ok || f.IsDir || f.SizeBytes == 0 {
		t.Errorf("listing missing data/raw.csv file entry: %+v", resp.Files)
	}
}

func TestWorkspaceDownload_Admin(t *testing.T) {
	h, store := setupTest(t)
	task, _ := seedWorkspaceTask(t, store, models.TaskStatusSuccess)
	r := workspaceRouter(h)

	req := httptest.NewRequest("GET", "/tasks/"+task.ID.String()+"/workspace/data/raw.csv", nil)
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("download: want 200 got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "a,b\n1,2\n" {
		t.Errorf("download body = %q", got)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q; want attachment", cd)
	}
}

// TestWorkspaceDownload_TraversalRejected drives the path-safety guard through
// the HTTP layer: every traversal attempt must be rejected (400/404), never a
// 200 with file contents from outside the workspace.
func TestWorkspaceDownload_TraversalRejected(t *testing.T) {
	h, store := setupTest(t)
	task, root := seedWorkspaceTask(t, store, models.TaskStatusSuccess)

	// A secret outside the workspace, and a symlink inside the workspace that
	// points at it.
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outdir")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	r := workspaceRouter(h)
	cases := []struct {
		name string
		path string
	}{
		{"parent traversal", "/tasks/" + task.ID.String() + "/workspace/../../etc/passwd"},
		{"encoded parent traversal", "/tasks/" + task.ID.String() + "/workspace/%2e%2e/%2e%2e/etc/passwd"},
		{"symlink file escape", "/tasks/" + task.ID.String() + "/workspace/escape"},
		{"symlink dir escape", "/tasks/" + task.ID.String() + "/workspace/outdir/secret.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.path, nil)
			req.Header.Set("X-API-Key", "admin-key")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code == http.StatusOK {
				t.Fatalf("traversal %q returned 200 with body %q; must be rejected", tc.path, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "TOP SECRET") {
				t.Fatalf("traversal %q leaked the out-of-workspace secret", tc.path)
			}
			if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
				t.Errorf("traversal %q: want 400/404 got %d", tc.path, w.Code)
			}
		})
	}
}

// TestWorkspaceOwnership: a non-owner scoped key (with view perms) must be
// refused even though it could see the task via GetTask; the owning key gets in.
func TestWorkspaceOwnership(t *testing.T) {
	h, store := setupTest(t)
	// Use the authz harness's key manager via setupTest's handler: create two
	// keys, attribute the task to one of them.
	ownerRole := "client"
	_, ownerRaw, err := h.apiKeys.CreateKey("owner", nil, nil, &ownerRole, 0, nil, "")
	if err != nil {
		t.Fatalf("create owner key: %v", err)
	}
	otherRaw := mustCreateScopedKey(t, h.apiKeys, "client", nil)

	// Find the owner key's KeyID to attribute the task.
	valid, ownerKey, _ := h.apiKeys.ValidateKey(ownerRaw, nil, nil, nil, nil)
	if !valid || ownerKey == nil {
		t.Fatalf("owner key did not validate")
	}

	// Seed a task attributed to the owner key.
	ws := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(ws)
	if err := os.WriteFile(filepath.Join(resolved, "report.md"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	keyID := ownerKey.KeyID
	task := &models.Task{
		ID:             uuid.New(),
		Prompt:         "owned task",
		Status:         models.TaskStatusSuccess,
		CreatedAt:      time.Now().UTC(),
		WorkspacePath:  &resolved,
		CreatedByKeyID: &keyID,
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	r := workspaceRouter(h)

	t.Run("owner key allowed", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/tasks/"+task.ID.String()+"/workspace", nil)
		req.Header.Set("X-API-Key", ownerRaw)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("owner: want 200 got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("non-owner key forbidden", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/tasks/"+task.ID.String()+"/workspace", nil)
		req.Header.Set("X-API-Key", otherRaw)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("non-owner: want 403 got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("admin key allowed", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/tasks/"+task.ID.String()+"/workspace", nil)
		req.Header.Set("X-API-Key", "admin-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("admin: want 200 got %d: %s", w.Code, w.Body.String())
		}
	})
}

// TestWorkspaceRunningTask: a non-completed task's workspace is not browsable.
func TestWorkspaceRunningTask(t *testing.T) {
	h, store := setupTest(t)
	task, _ := seedWorkspaceTask(t, store, models.TaskStatusRunning)
	r := workspaceRouter(h)

	req := httptest.NewRequest("GET", "/tasks/"+task.ID.String()+"/workspace", nil)
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("running task workspace: want 409 got %d: %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceNoPath: a completed task with no recorded workspace returns 404.
func TestWorkspaceNoPath(t *testing.T) {
	h, store := setupTest(t)
	task := &models.Task{
		ID:        uuid.New(),
		Prompt:    "no workspace",
		Status:    models.TaskStatusSuccess,
		CreatedAt: time.Now().UTC(),
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	r := workspaceRouter(h)

	req := httptest.NewRequest("GET", "/tasks/"+task.ID.String()+"/workspace", nil)
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("no-workspace task: want 404 got %d: %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceZip: ?format=zip streams a valid archive containing the files.
func TestWorkspaceZip(t *testing.T) {
	h, store := setupTest(t)
	task, _ := seedWorkspaceTask(t, store, models.TaskStatusSuccess)
	r := workspaceRouter(h)

	req := httptest.NewRequest("GET", "/tasks/"+task.ID.String()+"/workspace?format=zip", nil)
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("zip: want 200 got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("zip Content-Type = %q", ct)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	got := map[string]bool{}
	for _, f := range zr.File {
		got[f.Name] = true
	}
	if !got["report.md"] || !got["data/raw.csv"] {
		t.Errorf("zip missing expected entries: %v", got)
	}
}

// TestWorkspaceSizeCap: a file over the configured cap returns 413.
func TestWorkspaceSizeCap(t *testing.T) {
	h, store := setupTest(t)
	task, root := seedWorkspaceTask(t, store, models.TaskStatusSuccess)

	t.Setenv("FLEET_WORKSPACE_DOWNLOAD_MAX_BYTES", "8")
	if err := os.WriteFile(filepath.Join(root, "big.bin"), bytes.Repeat([]byte("x"), 64), 0o644); err != nil {
		t.Fatalf("write big: %v", err)
	}
	r := workspaceRouter(h)

	req := httptest.NewRequest("GET", "/tasks/"+task.ID.String()+"/workspace/big.bin", nil)
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize download: want 413 got %d: %s", w.Code, w.Body.String())
	}
}
