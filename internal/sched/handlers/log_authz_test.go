// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// Regression tests for #980: a run-log transcript is readable by the principal
// that created the task, or by a principal holding the explicit fleet-wide
// view_all_logs grant — and by nobody else. Before the fix, any view_logs holder
// (including a scoped fleet_task_* key minted for one automation) could GET the
// transcript of every task on the box.

import (
	"bytes"
	"encoding/json"
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

// setupLogAuthz wires the four transcript routes behind the real
// AdminOrUserAuthMiddleware so each credential class travels the whole path
// (middleware admits, handler authorizes).
func setupLogAuthz(t *testing.T) (*storage.Storage, *apikeys.Manager, *chi.Mux) {
	t.Helper()
	tmpDir := t.TempDir()

	store := storage.New()
	if err := store.Initialize(filepath.Join(tmpDir, "test.db"), storage.DefaultPoolConfig()); err != nil {
		if isDatabaseUnavailable(err) {
			t.Skipf("Skipping tests: database unavailable: %v", err)
		}
		t.Fatalf("init storage: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	acquireTestLock(t, store)
	if err := cleanDB(store); err != nil {
		t.Fatalf("clean db: %v", err)
	}

	keyMgr, err := apikeys.NewManager(filepath.Join(tmpDir, "keys.json"), filepath.Join(tmpDir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("key mgr: %v", err)
	}

	h := New(Config{
		DefaultTaskModel: "test/model",
		AdminAPIKey:      "admin-key",
		DataDir:          tmpDir,
	}, store, keyMgr)

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Get("/logs/{task_id}", h.GetLogs)
		r.Get("/logs/{task_id}/history", h.GetLogHistory)
		r.Get("/logs/{task_id}/history/{entry_id}", h.GetLogHistoryEntry)
		r.Get("/tasks/{task_id}/stream", h.StreamTaskLogs)
	})
	r.Group(func(r chi.Router) {
		r.Use(h.AdminAuthMiddleware)
		r.Post("/keys", h.CreateAPIKey)
	})
	return store, keyMgr, r
}

// addLoggedTask creates a completed task attributed to the given creator (user
// and/or API key, either may be nil) and gives it a transcript to leak.
func addLoggedTask(t *testing.T, store *storage.Storage, createdBy *uuid.UUID, createdByKeyID *string) *models.Task {
	t.Helper()
	task := &models.Task{
		ID:             uuid.New(),
		Prompt:         "task with a sensitive transcript",
		Status:         models.TaskStatusSuccess,
		CreatedBy:      createdBy,
		CreatedByKeyID: createdByKeyID,
		CreatedAt:      time.Now().UTC(),
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	if _, err := store.AddLog(task.ID, &models.LogSession{
		ID:    "session-" + task.ID.String(),
		Title: "Session",
		Messages: []models.LogMessage{
			{ID: "m1", Role: "assistant", Content: "connector rows the caller must not see"},
		},
	}); err != nil {
		t.Fatalf("add log: %v", err)
	}
	return task
}

func addLogUser(t *testing.T, store *storage.Storage, username, role, token string) *models.User {
	t.Helper()
	hash := models.HashToken(token)
	user := &models.User{
		ID:           uuid.New(),
		Username:     username,
		PasswordHash: "hashed",
		Role:         role,
		SessionToken: &hash,
		CreatedAt:    time.Now().UTC(),
	}
	stored, err := store.AddUser(user)
	if err != nil {
		t.Fatalf("add user %s: %v", username, err)
	}
	if stored != nil {
		return stored
	}
	return user
}

// logRequest issues one transcript read with the given credential header.
func logRequest(t *testing.T, r *chi.Mux, path, header, value string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(header, value)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// TestRunLogReadIsCreatorScoped is the #980 regression: a task key that did not
// create the task is refused its transcript on every transcript route, while the
// creating key still reads its own.
func TestRunLogReadIsCreatorScoped(t *testing.T) {
	store, keyMgr, r := setupLogAuthz(t)

	ownerKey, ownerRaw, err := keyMgr.CreateTypedKey("owner", apikeys.KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create owner key: %v", err)
	}
	_, intruderRaw, err := keyMgr.CreateTypedKey("intruder", apikeys.KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create intruder key: %v", err)
	}
	_, readonlyRaw, err := keyMgr.CreateTypedKey("watcher", apikeys.KeyTypeReadonly, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create readonly key: %v", err)
	}

	task := addLoggedTask(t, store, nil, &ownerKey.KeyID)

	paths := []string{
		"/logs/" + task.ID.String(),
		"/logs/" + task.ID.String() + "/history",
		"/logs/" + task.ID.String() + "/history/1",
		"/tasks/" + task.ID.String() + "/stream",
	}

	// Every transcript route refuses a view_logs holder that is not the creator.
	// The history/stream siblings are checked too because the original bug was
	// one gate copied into four places and weakened in all of them.
	for _, path := range paths {
		for name, raw := range map[string]string{"task key": intruderRaw, "readonly key": readonlyRaw} {
			if code := logRequest(t, r, path, "X-API-Key", raw); code != http.StatusForbidden {
				t.Errorf("%s on %s: want 403, got %d", name, path, code)
			}
		}
	}

	// The creating key still reads its own transcript.
	if code := logRequest(t, r, "/logs/"+task.ID.String(), "X-API-Key", ownerRaw); code != http.StatusOK {
		t.Errorf("creating key must read its own transcript: got %d", code)
	}
	if code := logRequest(t, r, "/logs/"+task.ID.String()+"/history", "X-API-Key", ownerRaw); code != http.StatusOK {
		t.Errorf("creating key must read its own history: got %d", code)
	}
	if code := logRequest(t, r, "/tasks/"+task.ID.String()+"/stream", "X-API-Key", ownerRaw); code != http.StatusOK {
		t.Errorf("creating key must stream its own transcript: got %d", code)
	}
}

// TestRunLogFleetWideGrants pins who may still read every transcript: the admin
// key, and a key minted with the explicit view_all_logs permission.
func TestRunLogFleetWideGrants(t *testing.T) {
	store, keyMgr, r := setupLogAuthz(t)

	task := addLoggedTask(t, store, nil, nil) // created out-of-band; owned by nobody
	path := "/logs/" + task.ID.String()

	if code := logRequest(t, r, path, "X-API-Key", "admin-key"); code != http.StatusOK {
		t.Errorf("admin key must read any transcript: got %d", code)
	}

	_, auditorRaw, err := keyMgr.CreateKey("auditor", []models.Permission{models.PermissionViewTasks, models.PermissionViewLogs, models.PermissionViewAllLogs},
		nil, 0, nil, "fleet-wide log auditor")
	if err != nil {
		t.Fatalf("create auditor key: %v", err)
	}
	if code := logRequest(t, r, path, "X-API-Key", auditorRaw); code != http.StatusOK {
		t.Errorf("view_all_logs key must read any transcript: got %d", code)
	}

	// An unattributed task is readable only under a fleet-wide grant — a plain
	// view_logs key owns nothing and therefore reads nothing.
	_, plainRaw, err := keyMgr.CreateTypedKey("plain", apikeys.KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create plain key: %v", err)
	}
	if code := logRequest(t, r, path, "X-API-Key", plainRaw); code != http.StatusForbidden {
		t.Errorf("plain view_logs key must not read an unowned transcript: got %d", code)
	}
}

// TestRunLogUserScoping covers the user (web UI) credential class: a non-admin
// user reads its own tasks' transcripts only; an admin-role user reads all.
func TestRunLogUserScoping(t *testing.T) {
	store, _, r := setupLogAuthz(t)

	alice := addLogUser(t, store, "alice", "client", "alice-token")
	addLogUser(t, store, "bob", "client", "bob-token")
	addLogUser(t, store, "root", "admin", "root-token")

	task := addLoggedTask(t, store, &alice.ID, nil)
	path := "/logs/" + task.ID.String()

	if code := logRequest(t, r, path, "Authorization", "Bearer alice-token"); code != http.StatusOK {
		t.Errorf("creator must read own transcript: got %d", code)
	}
	if code := logRequest(t, r, path, "Authorization", "Bearer bob-token"); code != http.StatusForbidden {
		t.Errorf("another user must not read the transcript: got %d", code)
	}
	if code := logRequest(t, r, path, "Authorization", "Bearer root-token"); code != http.StatusOK {
		t.Errorf("admin-role user must read any transcript: got %d", code)
	}
}

// TestRunLogUnknownTaskIs404 keeps the dashboard contract: a task with no
// transcript (or no task at all) still degrades to a clean 404 rather than a
// 5xx — the ownership gate must not turn a missing row into a server error.
func TestRunLogUnknownTaskIs404(t *testing.T) {
	_, _, r := setupLogAuthz(t)

	if code := logRequest(t, r, "/logs/"+uuid.New().String(), "X-API-Key", "admin-key"); code != http.StatusNotFound {
		t.Errorf("unknown task: want 404, got %d", code)
	}
}

// TestCreateKeyExplicitPermissions covers the mint path for a fleet-wide
// auditor key: an explicit permission set is honored, and the ambiguous
// combinations are rejected instead of silently resolved.
func TestCreateKeyExplicitPermissions(t *testing.T) {
	_, _, r := setupLogAuthz(t)

	post := func(t *testing.T, body models.APIKeyCreate) (int, models.APIKeyCreated) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/keys", bytes.NewReader(raw))
		req.Header.Set("X-API-Key", "admin-key")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var created models.APIKeyCreated
		_ = json.Unmarshal(w.Body.Bytes(), &created)
		return w.Code, created
	}

	t.Run("explicit permissions are granted", func(t *testing.T) {
		code, created := post(t, models.APIKeyCreate{
			Name:        "auditor",
			Permissions: []models.Permission{models.PermissionViewTasks, models.PermissionViewLogs, models.PermissionViewAllLogs},
		})
		if code != http.StatusCreated && code != http.StatusOK {
			t.Fatalf("create: got %d", code)
		}
		found := false
		for _, p := range created.Permissions {
			if p == string(models.PermissionViewAllLogs) {
				found = true
			}
		}
		if !found {
			t.Fatalf("minted key is missing view_all_logs: %v", created.Permissions)
		}
	})

	role := "readonly"
	for name, body := range map[string]models.APIKeyCreate{
		"with role": {Name: "k", Role: &role, Permissions: []models.Permission{models.PermissionViewAllLogs}},
		"with type": {Name: "k", Type: "readonly", Permissions: []models.Permission{models.PermissionViewAllLogs}},
		"unknown":   {Name: "k", Permissions: []models.Permission{models.Permission("read_everything")}},
	} {
		t.Run("rejected "+name, func(t *testing.T) {
			if code, _ := post(t, body); code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d", code)
			}
		})
	}
}
