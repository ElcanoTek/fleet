// Copyright (c) 2025 ElcanoTek
// All rights reserved. This is a private repository.

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fmt"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

func setupTestHandler(t *testing.T) (*chi.Mux, func()) {
	tmpDir, err := os.MkdirTemp("", "sched-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	store := storage.New()
	if err := store.Initialize(filepath.Join(tmpDir, "test.db")); err != nil {
		os.RemoveAll(tmpDir)
		if isDatabaseUnavailable(err) {
			t.Skipf("Skipping tests: database unavailable: %v", err)
		}
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Acquire global test lock to prevent interference from other parallel packages
	acquireTestLock(t, store)

	keyMgr, err := apikeys.NewManager(
		filepath.Join(tmpDir, "api_keys.json"),
		filepath.Join(tmpDir, "audit_log.jsonl"),
	)
	if err != nil {
		store.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("Failed to initialize API key manager: %v", err)
	}

	// Clean up PostgreSQL tables because tests share the same DB
	ctx := context.Background()
	queries := []string{
		"DELETE FROM logs",
		"DELETE FROM tasks",
		"DELETE FROM nodes",
		"DELETE FROM users",
	}
	for _, q := range queries {
		if _, err := store.DB().Conn().ExecContext(ctx, q); err != nil {
			t.Fatalf("Failed to clean up table: %v", err)
		}
	}

	h := New(Config{
		OrchestratorURL:   "http://localhost:8000",
		AdminAPIKey:       "test-admin-key",
		RegistrationToken: "test-reg-token",
		Version:           "0.1.0",
	}, store, keyMgr)

	r := chi.NewRouter()

	// Apply middlewares as in main.go
	r.Group(func(r chi.Router) {
		r.Use(h.RateLimitMiddleware)
		r.Use(h.RegistrationAuthMiddleware)
		r.Post("/register", h.RegisterNode)
	})

	r.Group(func(r chi.Router) {
		r.Use(h.AdminAuthMiddleware)
		r.Get("/nodes", h.ListNodes)
		r.Get("/nodes/{node_id}", h.GetNode)
		r.Delete("/nodes/{node_id}", h.UnregisterNode)

		r.Get("/tasks", h.ListTasks)
		r.Get("/tasks/{task_id}", h.GetTask)
		r.Post("/tasks/cleanup", h.CleanupHistory)
		r.Delete("/tasks/{task_id}", h.CancelTask)

		r.Get("/logs/{task_id}", h.GetLogs)

		r.Post("/keys", h.CreateAPIKey)
		r.Get("/keys", h.ListAPIKeys)
		r.Get("/keys/audit", h.GetAuditLog)
		r.Get("/keys/{key_id}", h.GetAPIKey)
		r.Post("/keys/{key_id}/rotate", h.RotateAPIKey)
		r.Post("/keys/{key_id}/revoke", h.RevokeAPIKey)
		r.Delete("/keys/{key_id}", h.DeleteAPIKey)

		r.Get("/stats", h.GetDashboardStats)
	})

	r.Group(func(r chi.Router) {
		r.Use(h.NodeAuthMiddleware)
		r.Post("/nodes/heartbeat", h.NodeHeartbeat)
		r.Get("/tasks/pending", h.GetPendingTask)
		r.Post("/status", h.ReportStatus)
		r.Post("/logs", h.SubmitLogs)
	})

	r.Post("/tasks", h.CreateTask) // Has its own complex auth logic
	r.Get("/health", h.HealthCheck)

	cleanup := func() {
		store.Close()
		os.RemoveAll(tmpDir)
	}

	return r, cleanup
}

func isDatabaseUnavailable(err error) bool {
	errMsg := err.Error()
	return strings.Contains(errMsg, "failed to connect to database") ||
		strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "no such host")
}

func TestHealthCheck(t *testing.T) {
	r, cleanup := setupTestHandler(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var resp models.HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.Status != "healthy" {
		t.Errorf("Expected status 'healthy', got '%s'", resp.Status)
	}
	if resp.Version != "0.1.0" {
		t.Errorf("Expected version '0.1.0', got '%s'", resp.Version)
	}
}

func TestTaskManagement(t *testing.T) {
	r, cleanup := setupTestHandler(t)
	defer cleanup()

	// Create task
	body := `{"prompt": "Test task prompt", "priority": 5}`
	req := httptest.NewRequest("POST", "/tasks", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var task models.Task
	json.NewDecoder(w.Body).Decode(&task)

	if task.Prompt != "Test task prompt" {
		t.Errorf("Expected prompt 'Test task prompt', got '%s'", task.Prompt)
	}
	if task.Priority != 5 {
		t.Errorf("Expected priority 5, got %d", task.Priority)
	}
	if task.Status != models.TaskStatusPending {
		t.Errorf("Expected status 'pending', got '%s'", task.Status)
	}

	// List tasks
	req = httptest.NewRequest("GET", "/tasks", nil)
	req.Header.Set("X-API-Key", "test-admin-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var paginatedResp models.PaginatedResponse
	json.NewDecoder(w.Body).Decode(&paginatedResp)
	tasks, ok := paginatedResp.Data.([]interface{})
	if !ok {
		t.Fatalf("Expected data to be a slice, got %T", paginatedResp.Data)
	}
	if len(tasks) != 1 {
		t.Errorf("Expected 1 task, got %d", len(tasks))
	}

	// Get specific task
	req = httptest.NewRequest("GET", "/tasks/"+task.ID.String(), nil)
	req.Header.Set("X-API-Key", "test-admin-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	// Cancel task
	req = httptest.NewRequest("DELETE", "/tasks/"+task.ID.String(), nil)
	req.Header.Set("X-API-Key", "test-admin-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var cancelledTask models.Task
	json.NewDecoder(w.Body).Decode(&cancelledTask)
	if cancelledTask.Status != models.TaskStatusCancelled {
		t.Errorf("Expected status 'cancelled', got '%s'", cancelledTask.Status)
	}
}

func TestAPIKeyManagement(t *testing.T) {
	r, cleanup := setupTestHandler(t)
	defer cleanup()

	// Create API key
	body := `{"name": "Test Key", "role": "client", "description": "Test description"}`
	req := httptest.NewRequest("POST", "/keys", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var keyCreated models.APIKeyCreated
	json.NewDecoder(w.Body).Decode(&keyCreated)

	if keyCreated.Name != "Test Key" {
		t.Errorf("Expected name 'Test Key', got '%s'", keyCreated.Name)
	}
	if keyCreated.APIKey == "" {
		t.Error("Expected API key to be returned")
	}

	// List API keys
	req = httptest.NewRequest("GET", "/keys", nil)
	req.Header.Set("X-API-Key", "test-admin-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var keys []models.APIKeyResponse
	json.NewDecoder(w.Body).Decode(&keys)
	if len(keys) != 1 {
		t.Errorf("Expected 1 key, got %d", len(keys))
	}

	// Get specific key
	req = httptest.NewRequest("GET", "/keys/"+keyCreated.KeyID, nil)
	req.Header.Set("X-API-Key", "test-admin-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	// Rotate key
	req = httptest.NewRequest("POST", "/keys/"+keyCreated.KeyID+"/rotate", nil)
	req.Header.Set("X-API-Key", "test-admin-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// Revoke key
	req = httptest.NewRequest("POST", "/keys/"+keyCreated.KeyID+"/revoke", nil)
	req.Header.Set("X-API-Key", "test-admin-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// Delete key
	req = httptest.NewRequest("DELETE", "/keys/"+keyCreated.KeyID, nil)
	req.Header.Set("X-API-Key", "test-admin-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestScopedAPIKeyUsage verifies a scoped (client-role) API key can create a
// task. Node-target gating was removed with the move to per-task mcp_selection,
// so a scoped key no longer has to (and cannot) name a target node — it simply
// creates the task.
func TestScopedAPIKeyUsage(t *testing.T) {
	r, cleanup := setupTestHandler(t)
	defer cleanup()

	// Create scoped API key
	body := `{"name": "Scoped Key", "role": "client", "allowed_node_patterns": ["client-acme-*"]}`
	req := httptest.NewRequest("POST", "/keys", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var keyCreated models.APIKeyCreated
	json.NewDecoder(w.Body).Decode(&keyCreated)

	// Use scoped key to create a task.
	body = `{"prompt": "Test task from scoped key"}`
	req = httptest.NewRequest("POST", "/tasks", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", keyCreated.APIKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var task models.Task
	json.NewDecoder(w.Body).Decode(&task)
	if task.Prompt != "Test task from scoped key" {
		t.Errorf("Expected prompt 'Test task from scoped key', got %q", task.Prompt)
	}
	if task.Status != models.TaskStatusPending {
		t.Errorf("Expected status 'pending', got '%s'", task.Status)
	}
}

func TestHistoryCleanup(t *testing.T) {
	r, cleanup := setupTestHandler(t)
	defer cleanup()

	// Create and complete a task
	body := `{"prompt": "Test task"}`
	req := httptest.NewRequest("POST", "/tasks", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var task models.Task
	json.NewDecoder(w.Body).Decode(&task)

	// Cancel it to mark as completed
	req = httptest.NewRequest("DELETE", "/tasks/"+task.ID.String(), nil)
	req.Header.Set("X-API-Key", "test-admin-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Run cleanup with 0 days (should delete recent tasks)
	req = httptest.NewRequest("POST", "/tasks/cleanup?days=0", nil)
	req.Header.Set("X-API-Key", "test-admin-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var cleanupResp models.CleanupResponse
	json.NewDecoder(w.Body).Decode(&cleanupResp)

	// Cleanup should have deleted the task
	if cleanupResp.DeletedCount != 1 {
		t.Errorf("Expected 1 deleted task, got %d", cleanupResp.DeletedCount)
	}
}

func TestRateLimiter_IncrementalCleanup(t *testing.T) {
	// 1ms window to make things stale quickly
	rl := newRateLimiter(10, time.Millisecond)

	// Fill with stale items
	staleCount := 200
	cutoff := time.Now().Add(-time.Second) // definitely stale
	for i := 0; i < staleCount; i++ {
		ip := fmt.Sprintf("stale-%d", i)
		rl.requests[ip] = []time.Time{cutoff}
	}

	// Verify initial state
	if len(rl.requests) != staleCount {
		t.Fatalf("Expected %d items, got %d", staleCount, len(rl.requests))
	}

	// Drive cleanup with a single active IP
	// This will trigger the incremental cleanup logic repeatedly.
	for i := 0; i < 1000; i++ {
		rl.Allow("active-ip")
	}

	// Now map should contain "active-ip" + maybe some leftovers from 200 stale.
	// We expect significant cleanup.

	if len(rl.requests) > 120 {
		t.Errorf("Rate limiter failed to cleanup. Size: %d", len(rl.requests))
	}
}
