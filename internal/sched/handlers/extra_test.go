// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

func setupTest(t *testing.T) (*Handlers, *storage.Storage) {
	tmpDir := t.TempDir()
	store := storage.New()
	if err := store.Initialize(fleetdb.New(t).SchedDSN(), fleetdb.SchedPool()); err != nil {
		t.Fatalf("Failed to init storage: %v", err)
	}
	// Close the pool when the test ends: without this every setupTest caller
	// leaks a full connection pool for the life of the package binary, and the
	// package creeps toward the Postgres max_connections ceiling ("sorry, too
	// many clients already") as tests are added.

	keyMgr, _ := apikeys.NewManager(filepath.Join(tmpDir, "keys.json"), filepath.Join(tmpDir, "audit.jsonl"))

	h := New(Config{SharedToken: testSharedToken, DefaultTaskModel: "test/model",
		AdminAPIKey: "admin-key",
		DataDir:     tmpDir,
	}, store, keyMgr)

	return h, store
}

func TestUpload(t *testing.T) {
	t.Parallel()

	h, _ := setupTest(t)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "test.txt")
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte("hello world"))
	writer.Close()

	// Test Unauthenticated
	req := httptest.NewRequest("POST", "/upload", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()

	h.HandleUpload(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401, got %d", w.Code)
	}

	// Test Authenticated (Admin)
	req = httptest.NewRequest("POST", "/upload", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-API-Key", "admin-key")
	w = httptest.NewRecorder()

	h.HandleUpload(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}
}

func TestDashboardStatsWithUserToken(t *testing.T) {
	t.Parallel()

	h, store := setupTest(t)

	// An account with ops_scope run, signed in as a session (seededSessions).
	seedOpsUser(t, store, uuid.New(), "dashboard-user", "client", "dashboard-test-token")

	// Test dashboard stats without auth - should fail
	req := httptest.NewRequest("GET", "/stats", nil)
	w := httptest.NewRecorder()
	h.GetDashboardStats(w, req)

	// The handler itself doesn't check auth - that's done by middleware
	// So we test the AdminOrUserAuthMiddleware instead
	// Test that user token authentication works in middleware

	// Test the middleware directly
	middlewareHandler := asSeededSession(h.AdminOrUserAuthMiddleware(http.HandlerFunc(h.GetDashboardStats)))

	// Without auth
	req = httptest.NewRequest("GET", "/stats", nil)
	w = httptest.NewRecorder()
	middlewareHandler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 without auth, got %d", w.Code)
	}

	// With invalid token
	req = httptest.NewRequest("GET", "/stats", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")
	w = httptest.NewRecorder()
	middlewareHandler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 with invalid token, got %d", w.Code)
	}

	// With valid user token
	req = httptest.NewRequest("GET", "/stats", nil)
	req.Header.Set("Authorization", "Bearer dashboard-test-token")
	w = httptest.NewRecorder()
	middlewareHandler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200 with valid user token, got %d: %s", w.Code, w.Body.String())
	}

	// With admin API key
	req = httptest.NewRequest("GET", "/stats", nil)
	req.Header.Set("X-API-Key", "admin-key")
	w = httptest.NewRecorder()
	middlewareHandler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200 with admin key, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetLogsWithUserAuth(t *testing.T) {
	t.Parallel()

	h, store := setupTest(t)

	// An admin account, signed in as a session (seededSessions).
	userToken := "logs-test-token"
	adminUser := seedOpsUser(t, store, uuid.New(), "log-viewer-user", "admin", userToken)

	// Create a task owned by the admin user (GetLogs visibility is gated on
	// created_by/scope; the admin user sees every task regardless).
	task := &models.Task{
		ID:        uuid.New(),
		Prompt:    "Test task for logs",
		Status:    models.TaskStatusPending,
		CreatedBy: &adminUser.ID,
		CreatedAt: time.Now().UTC(),
	}
	task, err := store.AddTask(task)
	if err != nil {
		t.Fatalf("Failed to create task: %v", err)
	}

	// Promote it to running.
	task.Status = models.TaskStatusRunning
	store.UpdateTask(task)

	// Submit logs
	logSession := models.LogSession{
		ID:               "test-session-id",
		Title:            "Test Session",
		PromptTokens:     100,
		CompletionTokens: 50,
		Cost:             0.01,
		Messages: []models.LogMessage{
			{
				ID:      "msg-1",
				Role:    "user",
				Content: "Hello, world!",
			},
			{
				ID:      "msg-2",
				Role:    "assistant",
				Content: "Hello! How can I help you today?",
			},
		},
	}
	_, err = store.AddLog(task.ID, &logSession)
	if err != nil {
		t.Fatalf("Failed to add log: %v", err)
	}

	// Update task with session ID (this is what shows "View" vs "None" in dashboard)
	task.AgentSessionID = &logSession.ID
	store.UpdateTask(task)

	// Create a chi router with the logs endpoint using AdminOrUserAuthMiddleware
	r := chi.NewRouter()
	r.Use(asSeededSession)
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Get("/logs/{task_id}", h.GetLogs)
		r.Get("/tasks", h.ListTasks)
	})

	// Test 1: Get logs with user Bearer token
	req := httptest.NewRequest("GET", "/logs/"+task.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify response contains the log session
	var returnedSession models.LogSession
	if err := json.NewDecoder(w.Body).Decode(&returnedSession); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if returnedSession.ID != logSession.ID {
		t.Errorf("Expected session ID '%s', got '%s'", logSession.ID, returnedSession.ID)
	}

	if len(returnedSession.Messages) != 2 {
		t.Errorf("Expected 2 messages, got %d", len(returnedSession.Messages))
	}

	// Verify message content
	if returnedSession.Messages[0].Role != "user" {
		t.Errorf("Expected first message role 'user', got '%s'", returnedSession.Messages[0].Role)
	}

	// Test 2: Verify agent_session_id is returned in task list
	req = httptest.NewRequest("GET", "/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200 for task list, got %d: %s", w.Code, w.Body.String())
	}

	var taskListResponse models.PaginatedResponse
	if err := json.NewDecoder(w.Body).Decode(&taskListResponse); err != nil {
		t.Fatalf("Failed to decode task list: %v", err)
	}

	if taskListResponse.Total < 1 {
		t.Errorf("Expected at least 1 task, got %d", taskListResponse.Total)
	}

	// Verify the task has agent_session_id set
	tasksData, ok := taskListResponse.Data.([]interface{})
	if !ok {
		t.Fatalf("Expected Data to be []interface{}, got %T", taskListResponse.Data)
	}

	if len(tasksData) < 1 {
		t.Fatalf("Expected at least 1 task in data")
	}

	taskData, ok := tasksData[0].(map[string]interface{})
	if !ok {
		t.Fatalf("Expected task to be map[string]interface{}")
	}

	agentSessionID, exists := taskData["agent_session_id"]
	switch {
	case !exists:
		t.Errorf("Expected agent_session_id field in task")
	case agentSessionID == nil:
		t.Errorf("Expected agent_session_id to be set, got nil")
	case agentSessionID != logSession.ID:
		t.Errorf("Expected agent_session_id '%s', got '%v'", logSession.ID, agentSessionID)
	}
}

// TestGetLogsForScheduledTaskReturns404 pins the contract the dashboard relies
// on: a task that has never run (status=scheduled, agent_session_id=nil) has
// no logs, and GET /logs/{id} returns a clean 404 — never a 5xx. The frontend
// now short-circuits this fetch when agent_session_id is empty, but if that
// signal ever drifts the backend must still degrade gracefully so the modal's
// 404 path renders the empty state instead of cascading into an error toast.
func TestGetLogsForScheduledTaskReturns404(t *testing.T) {
	t.Parallel()

	h, store := setupTest(t)

	scheduledFor := time.Now().Add(1 * time.Hour).UTC()
	task := &models.Task{
		ID:           uuid.New(),
		Prompt:       "Run a report tomorrow",
		Status:       models.TaskStatusScheduled,
		ScheduledFor: &scheduledFor,
		CreatedAt:    time.Now().UTC(),
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("Failed to create scheduled task: %v", err)
	}

	r := chi.NewRouter()
	r.Use(asSeededSession)
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Get("/logs/{task_id}", h.GetLogs)
	})

	req := httptest.NewRequest("GET", "/logs/"+task.ID.String(), nil)
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected 404 for scheduled task with no logs, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDashboardStatsAgentPool: the agent-pool fields are absent until
// cmd/fleet wires the seam, and once wired they are stamped LIVE on every
// response — including cache hits — so the stats cache can't freeze them.
func TestDashboardStatsAgentPool(t *testing.T) {
	t.Parallel()

	h, _ := setupTest(t)

	fetch := func() models.DashboardStats {
		t.Helper()
		req := httptest.NewRequest("GET", "/stats", nil)
		w := httptest.NewRecorder()
		h.GetDashboardStats(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var stats models.DashboardStats
		if err := json.NewDecoder(w.Body).Decode(&stats); err != nil {
			t.Fatalf("decode stats: %v", err)
		}
		return stats
	}

	// Unwired: fields omitted.
	stats := fetch()
	if stats.ActiveAgents != nil || stats.AgentSlots != nil {
		t.Fatalf("expected agent fields absent before wiring, got %+v", stats)
	}

	// Wired: live values attached. The first fetch above populated the stats
	// cache, so this response is a cache hit — the agent numbers must still be
	// the CURRENT closure values, not frozen ones.
	active := 3
	h.SetAgentPoolStats(func() int { return active }, func() int { return 8 })
	stats = fetch()
	if stats.ActiveAgents == nil || *stats.ActiveAgents != 3 {
		t.Fatalf("expected active_agents=3, got %+v", stats.ActiveAgents)
	}
	if stats.AgentSlots == nil || *stats.AgentSlots != 8 {
		t.Fatalf("expected agent_slots=8, got %+v", stats.AgentSlots)
	}

	// The pool moves; the next response reflects it immediately even though
	// the cached task counts haven't expired.
	active = 5
	stats = fetch()
	if stats.ActiveAgents == nil || *stats.ActiveAgents != 5 {
		t.Fatalf("expected live active_agents=5 on a cache hit, got %+v", stats.ActiveAgents)
	}
}
