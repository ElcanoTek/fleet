// Copyright (c) 2025 ElcanoTek
// All rights reserved. This is a private repository.

package handlers

import (
	"bytes"
	"context"
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
)

// acquireTestLock serializes the handlers test package against the other sched
// test packages that share the same database. The gate already runs the suite
// with -p 1, but the advisory lock keeps the package safe to run on its own.
func acquireTestLock(t *testing.T, s *storage.Storage) {
	ctx := context.Background()
	// Get a dedicated connection for the lock
	conn, err := s.DB().Conn().Conn(ctx)
	if err != nil {
		t.Fatalf("Failed to get DB connection for lock: %v", err)
	}

	// Acquire lock (waits if held by another test)
	// We use a fixed ID (1) to serialize all tests across packages that share the DB
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(1)"); err != nil {
		conn.Close()
		t.Fatalf("Failed to acquire test lock: %v", err)
	}

	// Release lock when test finishes
	t.Cleanup(func() {
		if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock(1)"); err != nil {
			t.Logf("Failed to release test lock: %v", err)
		}
		conn.Close()
	})
}

func cleanDB(s *storage.Storage) error {
	ctx := context.Background()
	// Use raw DB access to clean tables
	db := s.DB()
	tables := []string{"logs", "tasks", "nodes", "users"}
	for _, table := range tables {
		if _, err := db.Conn().ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
	}
	return nil
}

func setupTest(t *testing.T) (*Handlers, *storage.Storage, string) {
	tmpDir := t.TempDir()
	store := storage.New()
	if err := store.Initialize(filepath.Join(tmpDir, "test.db")); err != nil {
		if isDatabaseUnavailable(err) {
			t.Skipf("Skipping tests: database unavailable: %v", err)
		}
		t.Fatalf("Failed to init storage: %v", err)
	}

	// Acquire global test lock to prevent interference from other parallel packages
	acquireTestLock(t, store)

	// Clean up database to ensure a fresh state for each test
	if err := cleanDB(store); err != nil {
		t.Fatalf("Failed to clean database: %v", err)
	}

	keyMgr, _ := apikeys.NewManager(filepath.Join(tmpDir, "keys.json"), filepath.Join(tmpDir, "audit.jsonl"))

	h := New(Config{
		AdminAPIKey: "admin-key",
		DataDir:     tmpDir,
	}, store, keyMgr)

	return h, store, tmpDir
}

func TestUpload(t *testing.T) {
	h, _, _ := setupTest(t)

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

func TestUserLogin(t *testing.T) {
	h, store, _ := setupTest(t)

	// First create a user
	createBody := `{"username": "testlogin", "password": "testpassword123", "role": "client"}`
	req := httptest.NewRequest("POST", "/users", bytes.NewBufferString(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	h.CreateUser(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("Failed to create user: %d - %s", w.Code, w.Body.String())
	}

	// Test login with wrong password
	loginBody := `{"username": "testlogin", "password": "wrongpassword"}`
	req = httptest.NewRequest("POST", "/auth/login", bytes.NewBufferString(loginBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.Login(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401 for wrong password, got %d", w.Code)
	}

	// Test login with non-existent user
	loginBody = `{"username": "nonexistent", "password": "anypassword"}`
	req = httptest.NewRequest("POST", "/auth/login", bytes.NewBufferString(loginBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.Login(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401 for non-existent user, got %d", w.Code)
	}

	// Test login with correct credentials
	loginBody = `{"username": "testlogin", "password": "testpassword123"}`
	req = httptest.NewRequest("POST", "/auth/login", bytes.NewBufferString(loginBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.Login(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200 for correct login, got %d: %s", w.Code, w.Body.String())
	}

	var loginResp models.LoginResponse
	if err := json.NewDecoder(w.Body).Decode(&loginResp); err != nil {
		t.Fatalf("Failed to decode login response: %v", err)
	}

	if loginResp.Token == "" {
		t.Error("Expected token in login response, got empty string")
	}

	if loginResp.User.Username != "testlogin" {
		t.Errorf("Expected username 'testlogin', got '%s'", loginResp.User.Username)
	}

	// Verify token can be used for authentication
	user, err := store.GetUserByToken(loginResp.Token)
	if err != nil || user == nil {
		t.Errorf("Failed to retrieve user by token: %v", err)
	}
}

func TestDashboardStatsWithUserToken(t *testing.T) {
	h, store, _ := setupTest(t)

	// Create a user with a session token
	user := &models.User{
		ID:        uuid.New(),
		Username:  "dashboard-user",
		Role:      "client",
		Scopes:    []string{},
		CreatedAt: time.Now(),
	}
	token := "dashboard-test-token"
	tokenHash := models.HashToken(token)
	user.SessionToken = &tokenHash
	store.AddUser(user)

	// Test dashboard stats without auth - should fail
	req := httptest.NewRequest("GET", "/stats", nil)
	w := httptest.NewRecorder()
	h.GetDashboardStats(w, req)

	// The handler itself doesn't check auth - that's done by middleware
	// So we test the AdminOrUserAuthMiddleware instead
	// Test that user token authentication works in middleware

	// Test the middleware directly
	middlewareHandler := h.AdminOrUserAuthMiddleware(http.HandlerFunc(h.GetDashboardStats))

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

func TestLoginToDashboardFlow(t *testing.T) {
	h, _, _ := setupTest(t)

	// Step 1: Create a user (admin creates it)
	createBody := `{"username": "flowtest", "password": "securepassword123", "role": "client"}`
	req := httptest.NewRequest("POST", "/users", bytes.NewBufferString(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	h.CreateUser(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("Failed to create user: %d - %s", w.Code, w.Body.String())
	}

	// Step 2: Login
	loginBody := `{"username": "flowtest", "password": "securepassword123"}`
	req = httptest.NewRequest("POST", "/auth/login", bytes.NewBufferString(loginBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.Login(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Login failed: %d - %s", w.Code, w.Body.String())
	}

	var loginResp models.LoginResponse
	json.NewDecoder(w.Body).Decode(&loginResp)
	userToken := loginResp.Token

	// Step 3: Access dashboard stats with the bearer token. CSRF is enforced
	// via the stateless Origin check (see TestCSRFMiddlewareOrigin), not a
	// token, and GET is a safe method anyway — so no CSRF token is involved.
	middlewareHandler := h.AdminOrUserAuthMiddleware(http.HandlerFunc(h.GetDashboardStats))

	req = httptest.NewRequest("GET", "/stats", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	middlewareHandler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Dashboard stats failed with valid token: %d - %s", w.Code, w.Body.String())
	}

	var stats models.DashboardStats
	if err := json.NewDecoder(w.Body).Decode(&stats); err != nil {
		t.Fatalf("Failed to decode stats: %v", err)
	}

	// Verify stats structure is valid (should have zero values since DB is empty)
	if stats.TotalNodes != 0 {
		t.Logf("Note: Expected 0 total nodes in clean test DB, got %d", stats.TotalNodes)
	}
}

func TestGetLogsWithUserAuth(t *testing.T) {
	h, store, _ := setupTest(t)

	// Create an admin user with session token
	adminUser := &models.User{
		ID:           uuid.New(),
		Username:     "log-viewer-user",
		PasswordHash: "hashed-password",
		Role:         "admin",
		CreatedAt:    time.Now(),
	}
	userToken := "logs-test-token"
	tokenHash := models.HashToken(userToken)
	adminUser.SessionToken = &tokenHash
	store.AddUser(adminUser)

	// Register a node
	node := &models.Node{
		ID:       uuid.New(),
		Hostname: "test-node",
		Name:     "test-node-1",
		APIKey:   "node-api-key-123",
		Status:   models.NodeStatusIdle,
	}
	store.AddNode(node)

	// Create a task
	task := &models.Task{
		ID:        uuid.New(),
		Prompt:    "Test task for logs",
		Status:    models.TaskStatusPending,
		CreatedAt: time.Now().UTC(),
	}
	task, err := store.AddTask(task)
	if err != nil {
		t.Fatalf("Failed to create task: %v", err)
	}

	// Assign task to node
	task.Status = models.TaskStatusAssigned
	task.AssignedNodeID = &node.ID
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
	if !exists {
		t.Errorf("Expected agent_session_id field in task")
	} else if agentSessionID == nil {
		t.Errorf("Expected agent_session_id to be set, got nil")
	} else if agentSessionID != logSession.ID {
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
	h, store, _ := setupTest(t)

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
