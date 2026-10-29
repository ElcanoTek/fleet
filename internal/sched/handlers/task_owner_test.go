// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// ownershipEnv wires every surface this change touches behind the real auth
// middleware, with three users — the owner (client role) whose hosted
// connections include "browserbase", another client, and an admin-role user —
// and a selection check that resolves exactly like the runner's: "bundle" is a
// bundle server, "browserbase" resolves only for the owner.
type ownershipEnv struct {
	r      *chi.Mux
	h      *Handlers
	store  *storage.Storage
	keyMgr *apikeys.Manager
	owner  *models.User
	other  *models.User
	admin  *models.User
	// checked records the owner each selection check ran for.
	checked []*uuid.UUID
}

const (
	ownerToken = "owner-token"
	otherToken = "other-token"
	adminToken = "admin-user-token"
)

func setupOwnershipEnv(t *testing.T) *ownershipEnv {
	t.Helper()
	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	t.Cleanup(cleanup)
	h := New(Config{DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, keyMgr)
	env := &ownershipEnv{h: h, store: store, keyMgr: keyMgr}

	addUser := func(username, role, token string) *models.User {
		hash := models.HashToken(token)
		u := &models.User{ID: uuid.New(), Username: username, Role: role, CreatedAt: time.Now(), SessionToken: &hash}
		if _, err := store.AddUser(u); err != nil {
			t.Fatalf("AddUser(%s): %v", username, err)
		}
		return u
	}
	env.owner = addUser("owner@example.com", "client", ownerToken)
	env.other = addUser("other@example.com", "client", otherToken)
	env.admin = addUser("admin@example.com", "admin", adminToken)

	h.SetMCPSelectionCheck(func(_ context.Context, task *models.Task) error {
		env.checked = append(env.checked, task.CreatedBy)
		var unknown []string
		for _, c := range task.MCPSelection {
			switch {
			case c.Server == "bundle":
			case c.Server == "browserbase" && task.CreatedBy != nil && *task.CreatedBy == env.owner.ID:
			default:
				unknown = append(unknown, c.Server)
			}
		}
		if len(unknown) > 0 {
			return fmt.Errorf("mcp selection references server(s) %v which are neither in the active bundle catalog nor among the task owner's hosted connections", unknown)
		}
		return nil
	})

	r := chi.NewRouter()
	r.Post("/tasks", h.CreateTask)
	r.Post("/tasks/batch", h.CreateTaskBatch)
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Put("/tasks/{task_id}", h.UpdateTask)
		r.Put("/tasks/{task_id}/owner", h.SetTaskOwner)
		r.Post("/tasks/{task_id}/rerun", h.RerunTask)
		r.Post("/tasks/{task_id}/clone", h.CloneTask)
		r.Post("/tasks/import", h.HandleTaskImport)
	})
	env.r = r
	return env
}

func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func apiKey(key string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("X-API-Key", key) }
}

func (e *ownershipEnv) do(t *testing.T, method, path string, auth func(*http.Request), body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
	}
	auth(req)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

// addRecurringSource seeds the incident's source shape: a user-owned
// recurring refresh task pinning a hosted connection only its owner has.
func (e *ownershipEnv) addRecurringSource(t *testing.T, owner *uuid.UUID, keyID *string, selection models.MCPSelection) *models.Task {
	t.Helper()
	future := time.Now().UTC().Add(time.Hour)
	task := &models.Task{
		ID:             uuid.New(),
		Prompt:         "refresh the TWC dashboard from the DSP exports",
		Title:          "TWC dashboard refresh",
		Status:         models.TaskStatusScheduled,
		ScheduledFor:   &future,
		Recurrence:     "0 9 * * *",
		Timezone:       "UTC",
		Priority:       models.PriorityNormal,
		CreatedAt:      time.Now().UTC(),
		CreatedBy:      owner,
		CreatedByKeyID: keyID,
		MCPSelection:   selection,
	}
	task.LineageID = task.ID
	if _, err := e.store.AddTask(task); err != nil {
		t.Fatalf("add source: %v", err)
	}
	return task
}

func (e *ownershipEnv) reload(t *testing.T, w *httptest.ResponseRecorder) *models.Task {
	t.Helper()
	got, err := e.store.GetTask(decodeCreatedTaskID(t, w))
	if err != nil || got == nil {
		t.Fatalf("reload copy: %v (%s)", err, w.Body.String())
	}
	return got
}

// The overrides the prod operator sent: point the copy at another dashboard.
const otherDashboardOverrides = `{"overrides":{"prompt":"refresh the Brookfield dashboard from the DSP exports","title":"Brookfield refresh","tags":["brookfield"]}}`

// TestRerunCloneByKeyKeepsTheSourceOwner is the 2026-10-26 regression: an
// admin-key clone/rerun used to mint copies with created_by NULL, so the
// owner-only hosted connection in their mcp_selection was unresolvable and
// every run dead-lettered. A principal with no user now gives the copy the
// source's owner, and the copy's selection is checked against that owner.
func TestRerunCloneByKeyKeepsTheSourceOwner(t *testing.T) {
	env := setupOwnershipEnv(t)
	adminScopedKeyID, adminScopedKey := mustCreateRoleKeyWithID(t, env.keyMgr, "admin")
	sourceKeyID := "source-key"
	source := env.addRecurringSource(t, &env.owner.ID, &sourceKeyID, models.MCPSelection{{Server: "browserbase"}})

	for _, verb := range []string{"clone", "rerun"} {
		t.Run(verb+" by the admin key", func(t *testing.T) {
			w := env.do(t, http.MethodPost, "/tasks/"+source.ID.String()+"/"+verb, apiKey("test-admin-key"), otherDashboardOverrides)
			if w.Code != http.StatusCreated {
				t.Fatalf("%s = %d, want 201: %s", verb, w.Code, w.Body.String())
			}
			dup := env.reload(t, w)
			if dup.CreatedBy == nil || *dup.CreatedBy != env.owner.ID {
				t.Fatalf("copy created_by = %v, want the source owner %s", dup.CreatedBy, env.owner.ID)
			}
			if dup.CreatedByKeyID == nil || *dup.CreatedByKeyID != sourceKeyID {
				t.Fatalf("copy created_by_key_id = %v, want the source's %q (the admin key has none of its own)", dup.CreatedByKeyID, sourceKeyID)
			}
			if dup.Prompt == source.Prompt || dup.Title != "Brookfield refresh" {
				t.Fatalf("overrides not applied: %q / %q", dup.Prompt, dup.Title)
			}
		})
		t.Run(verb+" by a scoped key", func(t *testing.T) {
			w := env.do(t, http.MethodPost, "/tasks/"+source.ID.String()+"/"+verb, apiKey(adminScopedKey), otherDashboardOverrides)
			if w.Code != http.StatusCreated {
				t.Fatalf("%s = %d, want 201: %s", verb, w.Code, w.Body.String())
			}
			dup := env.reload(t, w)
			if dup.CreatedBy == nil || *dup.CreatedBy != env.owner.ID {
				t.Fatalf("copy created_by = %v, want the source owner %s", dup.CreatedBy, env.owner.ID)
			}
			if dup.CreatedByKeyID == nil || *dup.CreatedByKeyID != adminScopedKeyID {
				t.Fatalf("copy created_by_key_id = %v, want the calling key %s (spend attribution, ADR-0043)", dup.CreatedByKeyID, adminScopedKeyID)
			}
		})
	}

	t.Run("a key clone of a key-created task keeps it attributed to that key", func(t *testing.T) {
		keyOnly := env.addRecurringSource(t, nil, &sourceKeyID, models.MCPSelection{{Server: "bundle"}})
		w := env.do(t, http.MethodPost, "/tasks/"+keyOnly.ID.String()+"/clone", apiKey("test-admin-key"), "")
		if w.Code != http.StatusCreated {
			t.Fatalf("clone = %d, want 201: %s", w.Code, w.Body.String())
		}
		dup := env.reload(t, w)
		if dup.CreatedBy != nil || dup.CreatedByKeyID == nil || *dup.CreatedByKeyID != sourceKeyID {
			t.Fatalf("copy attribution = %v / %v, want no user and key %q", dup.CreatedBy, dup.CreatedByKeyID, sourceKeyID)
		}
	})

	t.Run("an ownerless source still cannot be copied with an owner-only connection", func(t *testing.T) {
		ownerless := env.addRecurringSource(t, nil, nil, models.MCPSelection{{Server: "browserbase"}})
		w := env.do(t, http.MethodPost, "/tasks/"+ownerless.ID.String()+"/clone", apiKey("test-admin-key"), "")
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "[browserbase]") {
			t.Fatalf("clone of an ownerless source = %d, want 400 naming browserbase: %s", w.Code, w.Body.String())
		}
	})
}

// TestRerunCloneByUserBelongsToTheCaller: a user's copy is theirs, as before
// (#270) — including an admin-role user copying someone else's task. The new
// part is that the copy's selection is checked for that caller, so copying an
// owner-only connection to someone who lacks it is refused up front instead
// of dead-lettering.
func TestRerunCloneByUserBelongsToTheCaller(t *testing.T) {
	env := setupOwnershipEnv(t)
	pinned := env.addRecurringSource(t, &env.owner.ID, nil, models.MCPSelection{{Server: "browserbase"}})
	plain := env.addRecurringSource(t, &env.owner.ID, nil, models.MCPSelection{{Server: "bundle"}})

	for _, verb := range []string{"clone", "rerun"} {
		w := env.do(t, http.MethodPost, "/tasks/"+pinned.ID.String()+"/"+verb, bearer(ownerToken), "")
		if w.Code != http.StatusCreated {
			t.Fatalf("owner %s = %d, want 201: %s", verb, w.Code, w.Body.String())
		}
		if dup := env.reload(t, w); dup.CreatedBy == nil || *dup.CreatedBy != env.owner.ID {
			t.Fatalf("owner's %s created_by = %v, want the owner", verb, dup.CreatedBy)
		}

		w = env.do(t, http.MethodPost, "/tasks/"+plain.ID.String()+"/"+verb, bearer(adminToken), "")
		if w.Code != http.StatusCreated {
			t.Fatalf("admin-user %s = %d, want 201: %s", verb, w.Code, w.Body.String())
		}
		if dup := env.reload(t, w); dup.CreatedBy == nil || *dup.CreatedBy != env.admin.ID {
			t.Fatalf("admin-user %s created_by = %v, want the calling admin user %s", verb, dup.CreatedBy, env.admin.ID)
		}

		w = env.do(t, http.MethodPost, "/tasks/"+pinned.ID.String()+"/"+verb, bearer(adminToken), "")
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "neither in the active bundle catalog nor among the task owner's hosted connections") {
			t.Fatalf("admin-user %s of an owner-only connection = %d, want 400 with the run's message: %s", verb, w.Code, w.Body.String())
		}
	}
}

// TestRerunCloneNewLineage: by default a copy joins the source's lineage (the
// per-job workspace "Resubmit with changes" relies on); new_lineage makes it a
// job of its own.
func TestRerunCloneNewLineage(t *testing.T) {
	env := setupOwnershipEnv(t)
	source := env.addRecurringSource(t, &env.owner.ID, nil, models.MCPSelection{{Server: "browserbase"}})

	for _, verb := range []string{"clone", "rerun"} {
		w := env.do(t, http.MethodPost, "/tasks/"+source.ID.String()+"/"+verb, apiKey("test-admin-key"), otherDashboardOverrides)
		if w.Code != http.StatusCreated {
			t.Fatalf("%s = %d: %s", verb, w.Code, w.Body.String())
		}
		if dup := env.reload(t, w); dup.LineageID != source.LineageID {
			t.Fatalf("default %s lineage = %s, want the source's %s", verb, dup.LineageID, source.LineageID)
		}

		body := `{"overrides":{"prompt":"refresh the Brookfield dashboard","new_lineage":true}}`
		w = env.do(t, http.MethodPost, "/tasks/"+source.ID.String()+"/"+verb, apiKey("test-admin-key"), body)
		if w.Code != http.StatusCreated {
			t.Fatalf("%s new_lineage = %d: %s", verb, w.Code, w.Body.String())
		}
		dup := env.reload(t, w)
		if dup.LineageID == source.LineageID || dup.LineageID != dup.ID {
			t.Fatalf("new_lineage %s lineage = %s (copy %s, source lineage %s), want the copy's own id", verb, dup.LineageID, dup.ID, source.LineageID)
		}
		if dup.SourceTaskID == nil || *dup.SourceTaskID != source.ID {
			t.Fatalf("new_lineage %s lost its provenance: source_task_id = %v", verb, dup.SourceTaskID)
		}
	}
}

// TestCreatePathsCheckTheMCPSelection: every create surface refuses a
// selection its run would refuse, checked for the owner the task will have,
// and accepts the owner's own hosted connection.
func TestCreatePathsCheckTheMCPSelection(t *testing.T) {
	env := setupOwnershipEnv(t)
	create := func(auth func(*http.Request), server string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(models.TaskCreate{Prompt: "a task prompt that is long enough", MCPSelection: models.MCPSelection{{Server: server}}})
		return env.do(t, http.MethodPost, "/tasks", auth, string(body))
	}

	if w := create(bearer(ownerToken), "browserbase"); w.Code != http.StatusOK {
		t.Fatalf("owner with their hosted connection = %d, want 200: %s", w.Code, w.Body.String())
	}
	if w := create(bearer(otherToken), "browserbase"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "[browserbase]") {
		t.Fatalf("user without the connection = %d, want 400: %s", w.Code, w.Body.String())
	}
	// An admin-key create has no owner, so an owner-only connection can never
	// resolve for it — exactly what its runs used to dead-letter on.
	if w := create(apiKey("test-admin-key"), "browserbase"); w.Code != http.StatusBadRequest {
		t.Fatalf("ownerless create with a hosted connection = %d, want 400: %s", w.Code, w.Body.String())
	}
	if w := create(apiKey("test-admin-key"), "bundle"); w.Code != http.StatusOK {
		t.Fatalf("ownerless create with a bundle server = %d, want 200: %s", w.Code, w.Body.String())
	}

	t.Run("batch refuses per task", func(t *testing.T) {
		body, _ := json.Marshal(models.BatchTaskCreate{Tasks: []models.TaskCreate{
			{Prompt: "a task prompt that is long enough", MCPSelection: models.MCPSelection{{Server: "bundle"}}},
			{Prompt: "a task prompt that is long enough", MCPSelection: models.MCPSelection{{Server: "browserbsae"}}},
		}})
		w := env.do(t, http.MethodPost, "/tasks/batch", bearer(ownerToken), string(body))
		if w.Code != http.StatusMultiStatus || !strings.Contains(w.Body.String(), "[browserbsae]") {
			t.Fatalf("batch = %d, want 207 failing the misspelled task: %s", w.Code, w.Body.String())
		}
	})

	t.Run("clone override refuses an unknown server", func(t *testing.T) {
		source := env.addRecurringSource(t, &env.owner.ID, nil, models.MCPSelection{{Server: "bundle"}})
		w := env.do(t, http.MethodPost, "/tasks/"+source.ID.String()+"/clone", apiKey("test-admin-key"), `{"overrides":{"mcp_selection":[{"server":"browserbsae"}]}}`)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "[browserbsae]") {
			t.Fatalf("clone with a misspelled override = %d, want 400: %s", w.Code, w.Body.String())
		}
	})

	t.Run("import checks against the importing owner", func(t *testing.T) {
		envelope := `{"version":1,"tasks":[{"prompt":"an imported task prompt that is long enough","mcp_selection":[{"server":"browserbase"}]}]}`
		w := env.do(t, http.MethodPost, "/tasks/import", bearer(otherToken), envelope)
		if w.Code != http.StatusMultiStatus || !strings.Contains(w.Body.String(), "[browserbase]") {
			t.Fatalf("import by a user without the connection = %d, want 207 with the refusal: %s", w.Code, w.Body.String())
		}
		w = env.do(t, http.MethodPost, "/tasks/import", bearer(ownerToken), envelope)
		if w.Code != http.StatusOK {
			t.Fatalf("import by the owner = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("an edit is checked only when it changes the selection", func(t *testing.T) {
		// An ownerless task already pinning an owner-only connection (a row
		// made before this check): editing its prompt must still work.
		stranded := env.addRecurringSource(t, nil, nil, models.MCPSelection{{Server: "browserbase"}})
		edit := func(selection string) *httptest.ResponseRecorder {
			body := `{"prompt":"refresh the dashboard, edited","recurrence":"0 9 * * *","mcp_selection":` + selection + `}`
			return env.do(t, http.MethodPut, "/tasks/"+stranded.ID.String(), apiKey("test-admin-key"), body)
		}
		if w := edit(`[{"server":"browserbase"}]`); w.Code != http.StatusOK {
			t.Fatalf("edit keeping the selection = %d, want 200: %s", w.Code, w.Body.String())
		}
		if w := edit(`[{"server":"browserbase"},{"server":"browserbsae"}]`); w.Code != http.StatusBadRequest {
			t.Fatalf("edit changing the selection to an unresolvable one = %d, want 400: %s", w.Code, w.Body.String())
		}
	})
}

// TestSetTaskOwnerEndpoint covers PUT /tasks/{id}/owner: admin only, 404 for
// an unknown user or task, the selection checked for the NEW owner, and the
// chain's unfinished next occurrence reassigned with the task.
func TestSetTaskOwnerEndpoint(t *testing.T) {
	env := setupOwnershipEnv(t)
	clientKey := mustCreateRoleKey(t, env.keyMgr, "client")
	// An ownerless copy left by the incident, plus its already-spawned next
	// occurrence.
	dup := env.addRecurringSource(t, nil, nil, models.MCPSelection{{Server: "browserbase"}})
	future := time.Now().UTC().Add(25 * time.Hour)
	next := &models.Task{
		ID: uuid.New(), LineageID: dup.LineageID, PreviousOccurrenceID: &dup.ID,
		Prompt: dup.Prompt, Status: models.TaskStatusScheduled, ScheduledFor: &future,
		Recurrence: dup.Recurrence, Timezone: "UTC", Priority: models.PriorityNormal,
		CreatedAt: time.Now().UTC(), MCPSelection: dup.MCPSelection,
	}
	if _, err := env.store.AddTask(next); err != nil {
		t.Fatalf("add next occurrence: %v", err)
	}
	path := "/tasks/" + dup.ID.String() + "/owner"
	body := func(username string) string { return `{"username":"` + username + `"}` }

	for name, auth := range map[string]func(*http.Request){"client user": bearer(otherToken), "client key": apiKey(clientKey)} {
		if w := env.do(t, http.MethodPut, path, auth, body(env.owner.Username)); w.Code != http.StatusForbidden {
			t.Fatalf("%s = %d, want 403: %s", name, w.Code, w.Body.String())
		}
	}
	if w := env.do(t, http.MethodPut, path, apiKey("test-admin-key"), body("nobody@example.com")); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "User not found") {
		t.Fatalf("unknown user = %d, want 404 User not found: %s", w.Code, w.Body.String())
	}
	if w := env.do(t, http.MethodPut, "/tasks/"+uuid.NewString()+"/owner", apiKey("test-admin-key"), body(env.owner.Username)); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "Task not found") {
		t.Fatalf("unknown task = %d, want 404 Task not found: %s", w.Code, w.Body.String())
	}
	if w := env.do(t, http.MethodPut, path, apiKey("test-admin-key"), body("  ")); w.Code != http.StatusBadRequest {
		t.Fatalf("empty username = %d, want 400: %s", w.Code, w.Body.String())
	}
	// The new owner must be able to run the task: "other" lacks browserbase.
	if w := env.do(t, http.MethodPut, path, apiKey("test-admin-key"), body(env.other.Username)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "[browserbase]") {
		t.Fatalf("owner who cannot resolve the selection = %d, want 400: %s", w.Code, w.Body.String())
	}

	// Every row the reassignment rewrites is checked, not only the target: a
	// next occurrence edited to a connector the new owner lacks refuses it all.
	edited := env.addRecurringSource(t, nil, nil, models.MCPSelection{{Server: "browserbase"}})
	editedNext := &models.Task{
		ID: uuid.New(), LineageID: edited.LineageID, PreviousOccurrenceID: &edited.ID,
		Prompt: edited.Prompt, Status: models.TaskStatusScheduled, ScheduledFor: &future,
		Recurrence: edited.Recurrence, Timezone: "UTC", Priority: models.PriorityNormal,
		CreatedAt: time.Now().UTC(), MCPSelection: models.MCPSelection{{Server: "gamma"}},
	}
	if _, err := env.store.AddTask(editedNext); err != nil {
		t.Fatalf("add edited next occurrence: %v", err)
	}
	w := env.do(t, http.MethodPut, "/tasks/"+edited.ID.String()+"/owner", apiKey("test-admin-key"), body(env.owner.Username))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), editedNext.ID.String()) || !strings.Contains(w.Body.String(), "[gamma]") {
		t.Fatalf("chain with an unresolvable next occurrence = %d, want 400 naming it: %s", w.Code, w.Body.String())
	}
	for _, id := range []uuid.UUID{edited.ID, editedNext.ID} {
		if got, err := env.store.GetTask(id); err != nil || got == nil || got.CreatedBy != nil {
			t.Fatalf("a refused reassignment changed %s: %+v (%v)", id, got, err)
		}
	}

	// An admin-role user may do it too; the username is matched case-insensitively.
	w = env.do(t, http.MethodPut, path, bearer(adminToken), body("Owner@Example.com"))
	if w.Code != http.StatusOK {
		t.Fatalf("set owner = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Task struct {
			CreatedBy         *uuid.UUID `json:"created_by"`
			CreatedByUsername *string    `json:"created_by_username"`
		} `json:"task"`
		UpdatedTaskIDs []uuid.UUID `json:"updated_task_ids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if resp.Task.CreatedBy == nil || *resp.Task.CreatedBy != env.owner.ID || resp.Task.CreatedByUsername == nil || *resp.Task.CreatedByUsername != env.owner.Username {
		t.Fatalf("response task owner = %v / %v, want %s", resp.Task.CreatedBy, resp.Task.CreatedByUsername, env.owner.Username)
	}
	if len(resp.UpdatedTaskIDs) != 2 || resp.UpdatedTaskIDs[0] != dup.ID {
		t.Fatalf("updated_task_ids = %v, want [%s %s]", resp.UpdatedTaskIDs, dup.ID, next.ID)
	}
	for _, id := range []uuid.UUID{dup.ID, next.ID} {
		got, err := env.store.GetTask(id)
		if err != nil || got == nil || got.CreatedBy == nil || *got.CreatedBy != env.owner.ID {
			t.Fatalf("task %s owner = %+v, want %s", id, got, env.owner.ID)
		}
	}
	if last := env.checked[len(env.checked)-1]; last == nil || *last != env.owner.ID {
		t.Fatalf("selection checked for %v, want the new owner %s", last, env.owner.ID)
	}
}
