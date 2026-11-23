// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	a2abridge "github.com/ElcanoTek/fleet/internal/a2a"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// readerPermissions is the #980 transcript-auditor key (view_tasks,
// view_logs, view_all_logs) plus every non-admin acting permission, so that in
// the tests below only the per-task scope decides: view_all_logs grants
// reading every task, and acting on a task needs having created it or admin.
var readerPermissions = []models.Permission{
	models.PermissionViewTasks, models.PermissionViewLogs, models.PermissionViewAllLogs,
	models.PermissionCreateTask, models.PermissionCancelTask,
}

// actScopeRouter is the ownership env's handlers behind the real auth
// middleware, with every REST route that reads or acts on one task.
func actScopeRouter(env *ownershipEnv) *chi.Mux {
	h := env.h
	r := chi.NewRouter()
	r.Use(asSeededSession)
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Get("/tasks/{task_id}", h.GetTask)
		r.Put("/tasks/{task_id}", h.UpdateTask)
		r.Post("/tasks/{task_id}/tags", h.UpdateTaskTags)
		r.Post("/tasks/{task_id}/rerun", h.RerunTask)
		r.Post("/tasks/{task_id}/clone", h.CloneTask)
		r.Post("/tasks/{task_id}/wake", h.WakeTask)
	})
	return r
}

// copiesOf counts the tasks copied from source (source_task_id).
func copiesOf(t *testing.T, env *ownershipEnv, source uuid.UUID) int {
	t.Helper()
	var n int
	if err := env.store.DB().Conn().QueryRowContext(context.Background(),
		"SELECT count(*) FROM tasks WHERE source_task_id = $1", source).Scan(&n); err != nil {
		t.Fatalf("count copies: %v", err)
	}
	return n
}

// TestViewAllLogsGrantsReadingNotActing: a key holding view_all_logs reads
// another user's task, and cannot re-run, clone, edit, retag or wake it. A
// re-run or clone by a key runs as the source's owner (docs/TASK-OWNERSHIP.md),
// so it is open only to a key acting for that owner: the key that created the
// source, or an admin. The key still re-runs a task it created itself.
func TestViewAllLogsGrantsReadingNotActing(t *testing.T) {
	t.Parallel()

	env := setupOwnershipEnv(t)
	env.r = actScopeRouter(env)
	readerKey, readerRaw, err := env.keyMgr.CreateKey(context.Background(), "reader", readerPermissions, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create reader key: %v", err)
	}
	reader := apiKey(readerRaw)

	// The owner's job: a recurring task pinning a hosted connection only the
	// owner has, created from the owner's session (no key on it).
	source := env.addRecurringSource(t, &env.owner.ID, nil, models.MCPSelection{{Server: "browserbase"}})
	path := "/tasks/" + source.ID.String()

	t.Run("reads the task", func(t *testing.T) {
		if w := env.do(t, http.MethodGet, path, reader, ""); w.Code != http.StatusOK {
			t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	for _, verb := range []string{"rerun", "clone"} {
		t.Run("cannot "+verb+" it", func(t *testing.T) {
			w := env.do(t, http.MethodPost, path+"/"+verb, reader, `{"overrides":{"prompt":"a different job"}}`)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s = %d, want 403: %s", verb, w.Code, w.Body.String())
			}
			if n := copiesOf(t, env, source.ID); n != 0 {
				t.Fatalf("%s left %d copies of the owner's task, want none", verb, n)
			}
		})
	}

	t.Run("cannot edit it", func(t *testing.T) {
		w := env.do(t, http.MethodPut, path, reader, `{"prompt":"a different job for this schedule"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("PUT = %d, want 403: %s", w.Code, w.Body.String())
		}
		if got, _ := env.store.GetTask(source.ID); got == nil || got.Prompt != source.Prompt {
			t.Fatalf("prompt after a refused edit = %+v, want %q", got, source.Prompt)
		}
	})

	t.Run("cannot retag it", func(t *testing.T) {
		if w := env.do(t, http.MethodPost, path+"/tags", reader, `{"add":["moved"]}`); w.Code != http.StatusForbidden {
			t.Fatalf("tags = %d, want 403: %s", w.Code, w.Body.String())
		}
	})

	t.Run("cannot wake it", func(t *testing.T) {
		parked := addPausedTaskOwnedBy(t, env.store, &env.owner.ID, nil, models.TaskStatusPausedAwaitingWake)
		w := env.do(t, http.MethodPost, "/tasks/"+parked.ID.String()+"/wake", reader, `{"event":"deploy-finished","note":"go ahead"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("wake = %d, want 403: %s", w.Code, w.Body.String())
		}
		if got, _ := env.store.GetTask(parked.ID); got == nil || got.Status != models.TaskStatusPausedAwaitingWake {
			t.Fatalf("parked task after a refused wake = %+v, want still paused", got)
		}
	})

	t.Run("re-runs a task it created, as its own", func(t *testing.T) {
		own := env.addRecurringSource(t, nil, &readerKey.KeyID, models.MCPSelection{{Server: "bundle"}})
		w := env.do(t, http.MethodPost, "/tasks/"+own.ID.String()+"/rerun", reader, `{"overrides":{"prompt":"the same job again"}}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("rerun of its own task = %d, want 201: %s", w.Code, w.Body.String())
		}
		dup := env.reload(t, w)
		if dup.CreatedBy != nil || dup.CreatedByKeyID == nil || *dup.CreatedByKeyID != readerKey.KeyID {
			t.Fatalf("copy attribution = %v / %v, want no user and the key %s", dup.CreatedBy, dup.CreatedByKeyID, readerKey.KeyID)
		}
	})
}

// TestA2AViewAllLogsGrantsReadingNotActing is the A2A form: a key holding
// view_all_logs reads another credential's task, and cannot answer its
// pending question or read or manage its push-notification configs (they
// carry the creator's token and credentials). The creator still does both.
func TestA2AViewAllLogsGrantsReadingNotActing(t *testing.T) {
	t.Parallel()

	store, keyMgr, mux := setupA2AWith(t, true)
	creatorRaw, creatorKey := mintTaskKey(t, keyMgr, "creator")
	_, readerRaw, err := keyMgr.CreateKey(context.Background(), "reader", readerPermissions, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create reader key: %v", err)
	}

	paused := &models.Task{
		ID: uuid.New(), Prompt: "needs a human answer", Status: models.TaskStatusPausedAwaitingInput,
		CreatedAt: time.Now().UTC(), CreatedByKeyID: &creatorKey, Timezone: "UTC",
	}
	if _, err := store.AddTask(paused); err != nil {
		t.Fatal(err)
	}
	id := paused.ID.String()

	if _, env := rpc(t, mux, readerRaw, "GetTask", map[string]any{"id": id}); env == nil || env.Error != nil {
		t.Fatalf("reader GetTask: %+v", env)
	}

	rr, _ := rpc(t, mux, readerRaw, "SendMessage", map[string]any{
		"message":       userMessage("prod, please", id),
		"configuration": map[string]any{"returnImmediately": true},
	})
	if rr.Code != http.StatusForbidden {
		t.Errorf("reader answer = %d, want 403: %s", rr.Code, rr.Body.String())
	}
	if row, _ := store.GetTask(paused.ID); row == nil || row.Status != models.TaskStatusPausedAwaitingInput {
		t.Errorf("task after a refused answer = %+v, want still awaiting input", row)
	}

	pushConfig := map[string]any{"taskId": id, "id": "creator-hook", "url": "https://example.com/hook", "token": "placeholder-token"}
	if _, env := rpc(t, mux, creatorRaw, "CreateTaskPushNotificationConfig", pushConfig); env == nil || env.Error != nil {
		t.Fatalf("creator push config: %+v", env)
	}
	for _, call := range []struct {
		method string
		params map[string]any
	}{
		{"CreateTaskPushNotificationConfig", map[string]any{"taskId": id, "id": "reader-hook", "url": "https://example.com/other"}},
		{"GetTaskPushNotificationConfig", map[string]any{"taskId": id, "id": "creator-hook"}},
		{"ListTaskPushNotificationConfigs", map[string]any{"taskId": id}},
		{"DeleteTaskPushNotificationConfig", map[string]any{"taskId": id, "id": "creator-hook"}},
	} {
		if rr, _ := rpc(t, mux, readerRaw, call.method, call.params); rr.Code != http.StatusForbidden {
			t.Errorf("reader %s = %d, want 403: %s", call.method, rr.Code, rr.Body.String())
		}
	}
	if _, env := rpc(t, mux, creatorRaw, "GetTaskPushNotificationConfig", map[string]any{"taskId": id, "id": "creator-hook"}); env == nil || env.Error != nil {
		t.Fatalf("creator's push config after the reader's calls: %+v", env)
	}

	if _, env := rpc(t, mux, creatorRaw, "SendMessage", map[string]any{
		"message":       userMessage("prod, please", id),
		"configuration": map[string]any{"returnImmediately": true},
	}); env == nil || env.Error != nil {
		t.Fatalf("creator answer: %+v", env)
	}
}

// TestRerunCreatorGivesTheSourceOwnerOnlyToAnActingCaller pins the ownership
// rule where the copy's owner is chosen: the source's owner passes to a copy
// only when the caller may act on the source. Any other key's copy is its own.
func TestRerunCreatorGivesTheSourceOwnerOnlyToAnActingCaller(t *testing.T) {
	t.Parallel()

	env := setupOwnershipEnv(t)
	readerKey, _, err := env.keyMgr.CreateKey(context.Background(), "reader", readerPermissions, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create reader key: %v", err)
	}
	sourceKey := "source-key"
	source := env.addRecurringSource(t, &env.owner.ID, &sourceKey, models.MCPSelection{{Server: "bundle"}})

	creator, err := env.h.rerunCreator(context.Background(), principal{apiKey: readerKey}, source)
	if err != nil {
		t.Fatalf("rerunCreator: %v", err)
	}
	if creator.creatorID != nil {
		t.Errorf("a reading key's copy is owned by %v, want no user (the key's own copy)", creator.creatorID)
	}
	if creator.creatorKey == nil || *creator.creatorKey != readerKey.KeyID {
		t.Errorf("a reading key's copy is keyed to %v, want the calling key %s", creator.creatorKey, readerKey.KeyID)
	}

	creator, err = env.h.rerunCreator(context.Background(), principal{isAdmin: true}, source)
	if err != nil {
		t.Fatalf("rerunCreator (admin key): %v", err)
	}
	if creator.creatorID == nil || *creator.creatorID != env.owner.ID {
		t.Errorf("an admin-key copy is owned by %v, want the source owner %s", creator.creatorID, env.owner.ID)
	}
	if creator.creatorKey == nil || *creator.creatorKey != sourceKey {
		t.Errorf("an admin-key copy is keyed to %v, want the source's key %q", creator.creatorKey, sourceKey)
	}
}

// filesEnv is the real create, edit, copy and upload routes over a data dir of
// the test's own, with one owner (a session user) and a reader key holding
// view_all_logs and create_task.
type filesEnv struct {
	r         *chi.Mux
	h         *Handlers
	dataDir   string
	readerRaw string
	readerKey string
}

const filesOwnerToken = "files-owner-token"

func setupFilesEnv(t *testing.T) *filesEnv {
	t.Helper()
	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	t.Cleanup(cleanup)
	dataDir := t.TempDir()
	h := New(Config{DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0", DataDir: dataDir}, store, keyMgr)
	seedOpsUser(t, store, uuid.New(), "files-owner@example.com", "client", filesOwnerToken)
	key, raw, err := keyMgr.CreateKey(context.Background(), "reader", readerPermissions, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create reader key: %v", err)
	}
	r := chi.NewRouter()
	r.Use(asSeededSession)
	r.Post("/upload", h.HandleUpload)
	r.Post("/tasks", h.CreateTask)
	r.Post("/tasks/batch", h.CreateTaskBatch)
	r.Post("/tasks/estimate", h.EstimateTask)
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Put("/tasks/{task_id}", h.UpdateTask)
		r.Post("/tasks/{task_id}/rerun", h.RerunTask)
		r.Post("/tasks/{task_id}/clone", h.CloneTask)
		r.Post("/tasks/import", h.HandleTaskImport)
	})
	return &filesEnv{r: r, h: h, dataDir: dataDir, readerRaw: raw, readerKey: key.KeyID}
}

func (e *filesEnv) send(t *testing.T, method, path string, auth func(*http.Request), body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	auth(req)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

// upload stages one file through POST /upload and returns its stored name.
func (e *filesEnv) upload(t *testing.T, auth func(*http.Request)) string {
	t.Helper()
	req := multipartUploadRequest(t, "")
	auth(req)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload = %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Filename == "" {
		t.Fatalf("upload response %s: %v", w.Body.String(), err)
	}
	return got.Filename
}

// seedTask adds a scheduled task created by the given user or key.
func (e *filesEnv) seedTask(t *testing.T, owner *uuid.UUID, keyID *string, files []string) *models.Task {
	t.Helper()
	future := time.Now().UTC().Add(time.Hour)
	task := &models.Task{ID: uuid.New(), Prompt: "summarize the attached brief", Status: models.TaskStatusScheduled,
		ScheduledFor: &future, Priority: models.PriorityNormal, CreatedAt: time.Now().UTC(),
		CreatedBy: owner, CreatedByKeyID: keyID, Files: files}
	if _, err := e.h.storage.AddTask(task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return task
}

func createBody(files ...string) string {
	body, _ := json.Marshal(models.TaskCreate{Prompt: "summarize the attached brief", Files: files})
	return string(body)
}

// TestNamingAnUploadNeedsItsUploaderOrOwnTask: a run copies the uploads its
// task names into the workspace its creator can download, so a non-admin
// caller names only uploads it staged itself, uploads on a task it created,
// or the files already on the task it acts on. On every path that takes
// files — create, batch, import, edit, re-run and clone overrides — a key
// holding view_all_logs that names another user's upload gets the answer a
// missing upload gets. Admin names any upload.
func TestNamingAnUploadNeedsItsUploaderOrOwnTask(t *testing.T) {
	t.Parallel()

	env := setupFilesEnv(t)
	owner := bearer(filesOwnerToken)
	reader := apiKey(env.readerRaw)

	ownerUpload := env.upload(t, owner)
	w := env.send(t, http.MethodPost, "/tasks", owner, createBody(ownerUpload))
	if w.Code != http.StatusOK {
		t.Fatalf("owner create with its own upload = %d: %s", w.Code, w.Body.String())
	}
	ownerTask := decodeCreatedTaskID(t, w)
	ownerRow, _ := env.h.storage.GetTask(ownerTask)
	notFound := "file not found: " + ownerUpload

	readerTask := env.seedTask(t, nil, &env.readerKey, nil)
	refusals := []struct {
		name, method, path, body string
	}{
		{"create", http.MethodPost, "/tasks", createBody(ownerUpload)},
		{"estimate", http.MethodPost, "/tasks/estimate", createBody(ownerUpload)},
		{"edit its own task", http.MethodPut, "/tasks/" + readerTask.ID.String(), createBody(ownerUpload)},
		{"re-run its own task", http.MethodPost, "/tasks/" + readerTask.ID.String() + "/rerun", `{"overrides":{"files":["` + ownerUpload + `"]}}`},
		{"clone its own task", http.MethodPost, "/tasks/" + readerTask.ID.String() + "/clone", `{"overrides":{"files":["` + ownerUpload + `"]}}`},
	}
	for _, c := range refusals {
		t.Run("reader cannot name the owner's upload: "+c.name, func(t *testing.T) {
			w := env.send(t, c.method, c.path, reader, c.body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), notFound) {
				t.Fatalf("%s = %d, want 400 %q: %s", c.name, w.Code, notFound, w.Body.String())
			}
		})
	}
	t.Run("reader cannot name the owner's upload: batch", func(t *testing.T) {
		w := env.send(t, http.MethodPost, "/tasks/batch", reader, `{"tasks":[`+createBody(ownerUpload)+`]}`)
		if w.Code == http.StatusOK || !strings.Contains(w.Body.String(), notFound) {
			t.Fatalf("batch = %d, want the task refused with %q: %s", w.Code, notFound, w.Body.String())
		}
	})
	t.Run("batch reports a validation error before an upload refusal, as POST /tasks does", func(t *testing.T) {
		bad, _ := json.Marshal(models.TaskCreate{Prompt: "", Files: []string{ownerUpload}})
		single := env.send(t, http.MethodPost, "/tasks", reader, string(bad))
		var refusal struct {
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(single.Body.Bytes(), &refusal); err != nil || single.Code != http.StatusBadRequest || refusal.Detail == notFound {
			t.Fatalf("POST /tasks = %d %s, want a 400 validation error, not %q", single.Code, single.Body.String(), notFound)
		}
		w := env.send(t, http.MethodPost, "/tasks/batch", reader, `{"tasks":[`+string(bad)+`]}`)
		var got models.BatchTaskResult
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Failed) != 1 || got.Failed[0].Error != refusal.Detail {
			t.Fatalf("batch = %d %s, want the task failed with POST /tasks's %q", w.Code, w.Body.String(), refusal.Detail)
		}
	})
	t.Run("an import dry run reports the refusal the import would", func(t *testing.T) {
		env2, _ := json.Marshal(models.TaskExportEnvelope{Version: models.TaskExportVersion,
			Tasks: []models.TaskExportRecord{{Prompt: "summarize the attached brief", Files: []string{ownerUpload}}}})
		w := env.send(t, http.MethodPost, "/tasks/import?dry_run=true", reader, string(env2))
		var got models.TaskImportResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("dry run = %d %s: %v", w.Code, w.Body.String(), err)
		}
		if !got.DryRun || got.Created != 0 || got.Errors != 1 || len(got.Results) != 1 || got.Results[0].Error != notFound {
			t.Fatalf("dry run = %+v, want the record errored with %q", got, notFound)
		}
	})
	t.Run("reader cannot name the owner's upload: import", func(t *testing.T) {
		env2, _ := json.Marshal(models.TaskExportEnvelope{Version: models.TaskExportVersion,
			Tasks: []models.TaskExportRecord{{Prompt: "summarize the attached brief", Files: []string{ownerUpload}}}})
		w := env.send(t, http.MethodPost, "/tasks/import", reader, string(env2))
		if !strings.Contains(w.Body.String(), notFound) || strings.Contains(w.Body.String(), `"created":1`) {
			t.Fatalf("import = %d, want the record refused with %q: %s", w.Code, notFound, w.Body.String())
		}
	})
	t.Run("the reader's task is unchanged by the refused calls", func(t *testing.T) {
		if got, _ := env.h.storage.GetTask(readerTask.ID); got == nil || len(got.Files) != 0 {
			t.Fatalf("reader's task after refused calls = %+v, want no files", got)
		}
	})

	t.Run("reader names its own upload", func(t *testing.T) {
		own := env.upload(t, reader)
		if w := env.send(t, http.MethodPost, "/tasks", reader, createBody(own)); w.Code != http.StatusOK {
			t.Fatalf("create with its own upload = %d: %s", w.Code, w.Body.String())
		}
		if w := env.send(t, http.MethodPut, "/tasks/"+readerTask.ID.String(), reader, createBody(own)); w.Code != http.StatusOK {
			t.Fatalf("edit with its own upload = %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("the owner keeps and reuses its uploads", func(t *testing.T) {
		if w := env.send(t, http.MethodPost, "/tasks/"+ownerTask.String()+"/rerun", owner, ""); w.Code != http.StatusCreated {
			t.Fatalf("owner rerun keeping its files = %d: %s", w.Code, w.Body.String())
		}
		if w := env.send(t, http.MethodPut, "/tasks/"+ownerTask.String(), owner, createBody(ownerRow.Files...)); w.Code != http.StatusOK {
			t.Fatalf("owner edit keeping its files = %d: %s", w.Code, w.Body.String())
		}
		if w := env.send(t, http.MethodPost, "/tasks", owner, createBody(ownerUpload)); w.Code != http.StatusOK {
			t.Fatalf("owner create naming its upload again = %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("an unrecorded upload is namable through a task its caller created", func(t *testing.T) {
		const legacy = "legacy_brief_0000aaaa.txt"
		if err := os.WriteFile(filepath.Join(env.dataDir, "temp_uploads", legacy), []byte("old brief"), 0o600); err != nil {
			t.Fatal(err)
		}
		ownerID := ownerRow.CreatedBy
		env.seedTask(t, ownerID, nil, []string{legacy})
		if w := env.send(t, http.MethodPost, "/tasks", owner, createBody(legacy)); w.Code != http.StatusOK {
			t.Fatalf("owner create naming an upload on its task = %d: %s", w.Code, w.Body.String())
		}
		if w := env.send(t, http.MethodPost, "/tasks", reader, createBody(legacy)); w.Code != http.StatusBadRequest {
			t.Fatalf("reader create naming the owner's unrecorded upload = %d, want 400: %s", w.Code, w.Body.String())
		}
	})

	t.Run("a name holding U+0000 is refused, not a server error", func(t *testing.T) {
		body := `{"prompt":"summarize the attached brief","files":["brief\u0000.txt"]}`
		if w := env.send(t, http.MethodPost, "/tasks", reader, body); w.Code != http.StatusBadRequest {
			t.Fatalf("create naming a U+0000 file = %d, want 400: %s", w.Code, w.Body.String())
		}
	})

	t.Run("admin names any upload", func(t *testing.T) {
		if w := env.send(t, http.MethodPost, "/tasks", apiKey("test-admin-key"), createBody(ownerUpload)); w.Code != http.StatusOK {
			t.Fatalf("admin create naming the owner's upload = %d: %s", w.Code, w.Body.String())
		}
	})
}

// TestUploadLookupFailureIsAServerError: when the "on a task you created"
// lookup cannot run, batch and import fail the request with a server error
// before writing anything, instead of reporting the record as refused.
func TestUploadLookupFailureIsAServerError(t *testing.T) {
	t.Parallel()

	env := setupFilesEnv(t)
	ownerUpload := env.upload(t, bearer(filesOwnerToken))
	cancelled := func(method, path, body string) *httptest.ResponseRecorder {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // the lookup's query fails on this context
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		apiKey(env.readerRaw)(req)
		w := httptest.NewRecorder()
		env.r.ServeHTTP(w, req)
		return w
	}

	if w := cancelled(http.MethodPost, "/tasks/batch", `{"tasks":[`+createBody(ownerUpload)+`]}`); w.Code != http.StatusInternalServerError {
		t.Errorf("batch with a failed lookup = %d, want 500: %s", w.Code, w.Body.String())
	}

	// A batch of three by a key allowed three requests an hour: a failed
	// lookup fails it before the key is charged for the other two, so the
	// key's next request still goes through.
	_, limitedRaw, err := env.h.apiKeys.CreateKey(context.Background(), "limited", readerPermissions, nil, 3, nil, "")
	if err != nil {
		t.Fatalf("create limited key: %v", err)
	}
	three := `{"tasks":[` + createBody(ownerUpload) + `,` + createBody(ownerUpload) + `,` + createBody(ownerUpload) + `]}`
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/tasks/batch", strings.NewReader(three)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	apiKey(limitedRaw)(req)
	w := httptest.NewRecorder()
	env.r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("limited key's batch with a failed lookup = %d, want 500: %s", w.Code, w.Body.String())
	}
	if w := env.send(t, http.MethodPost, "/tasks", apiKey(limitedRaw), createBody()); w.Code != http.StatusOK {
		t.Errorf("limited key's next create = %d, want 200 (the failed batch charged one request, not three): %s", w.Code, w.Body.String())
	}
	envelope, _ := json.Marshal(models.TaskExportEnvelope{Version: models.TaskExportVersion,
		Tasks: []models.TaskExportRecord{{Prompt: "summarize the attached brief", Files: []string{ownerUpload}}}})
	if w := cancelled(http.MethodPost, "/tasks/import", string(envelope)); w.Code != http.StatusInternalServerError {
		t.Errorf("import with a failed lookup = %d, want 500: %s", w.Code, w.Body.String())
	}
}

// TestOwnerChangeCarriesTheCreatingKeysActs pins what PUT /tasks/{id}/owner
// hands over: the API key that created the task keeps acting on it, so after
// an admin gives the task an owner, the key's edits and copies run as that
// owner and may name the owner's hosted connections.
func TestOwnerChangeCarriesTheCreatingKeysActs(t *testing.T) {
	t.Parallel()

	env := setupOwnershipEnv(t)
	keyID, keyRaw := mustCreateRoleKeyWithID(t, env.keyMgr, "client")
	key := apiKey(keyRaw)
	task := env.addRecurringSource(t, nil, &keyID, models.MCPSelection{{Server: "bundle"}})
	path := "/tasks/" + task.ID.String()
	hosted := `"mcp_selection":[{"server":"browserbase"}]`

	if w := env.do(t, http.MethodPut, path, key, `{"prompt":"refresh the dashboard",`+hosted+`}`); w.Code != http.StatusBadRequest {
		t.Fatalf("key edit naming a hosted connection of no owner = %d, want 400: %s", w.Code, w.Body.String())
	}
	if w := env.do(t, http.MethodPut, path+"/owner", apiKey("test-admin-key"), `{"username":"owner@example.com"}`); w.Code != http.StatusOK {
		t.Fatalf("admin owner change = %d: %s", w.Code, w.Body.String())
	}

	if w := env.do(t, http.MethodPut, path, key, `{"prompt":"refresh the dashboard",`+hosted+`}`); w.Code != http.StatusOK {
		t.Fatalf("creating key's edit naming the new owner's connection = %d, want 200: %s", w.Code, w.Body.String())
	}
	w := env.do(t, http.MethodPost, path+"/rerun", key, `{"overrides":{"prompt":"refresh it now",`+hosted+`}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("creating key's rerun naming the new owner's connection = %d, want 201: %s", w.Code, w.Body.String())
	}
	if dup := env.reload(t, w); dup.CreatedBy == nil || *dup.CreatedBy != env.owner.ID || dup.CreatedByKeyID == nil || *dup.CreatedByKeyID != keyID {
		t.Fatalf("copy attribution = %v / %v, want the new owner %s and the key %s", dup.CreatedBy, dup.CreatedByKeyID, env.owner.ID, keyID)
	}
}

// a2aMethodScope names which tasks an A2A method reaches, like
// cmd/fleet's taskRouteScopes does for the REST routes.
type a2aMethodScope string

const (
	a2aNoTask a2aMethodScope = "no task"  // names no existing task
	a2aRead   a2aMethodScope = "read"     // own tasks, or every task with view_all_logs
	a2aAct    a2aMethodScope = "act"      // the task's creator, or admin
	a2aStop   a2aMethodScope = "stop"     // the creator, or cancel_task on a task the caller can read
	a2aAnswer a2aMethodScope = "send/act" // a new task, or with a taskId an answer: act
)

// a2aMethodScopes classifies every method the A2A dispatcher (A2ARPC)
// answers. A new method fails TestEveryA2AMethodHasAScope until it is listed
// here, and TestA2AViewAllLogsReachesOnlyReadAndStopMethods then drives it.
var a2aMethodScopes = map[string]a2aMethodScope{
	a2abridge.MethodSendMessage:          a2aAnswer,
	a2abridge.MethodSendStreamingMessage: a2aAnswer,
	a2abridge.MethodGetTask:              a2aRead,
	a2abridge.MethodListTasks:            a2aRead,
	a2abridge.MethodSubscribeToTask:      a2aRead,
	a2abridge.MethodCancelTask:           a2aStop,
	a2abridge.MethodCreatePushConfig:     a2aAct,
	a2abridge.MethodGetPushConfig:        a2aAct,
	a2abridge.MethodListPushConfigs:      a2aAct,
	a2abridge.MethodDeletePushConfig:     a2aAct,
	a2abridge.MethodGetExtendedAgentCard: a2aNoTask,
}

// TestEveryA2AMethodHasAScope reads the dispatcher's `switch req.Method` in
// a2a.go and requires each case to be classified in a2aMethodScopes, and each
// classified method to be a case.
func TestEveryA2AMethodHasAScope(t *testing.T) {
	t.Parallel()

	file, err := parser.ParseFile(token.NewFileSet(), "a2a.go", nil, 0)
	if err != nil {
		t.Fatalf("parse a2a.go: %v", err)
	}
	constants := map[string]string{}
	for name, value := range map[string]string{
		"MethodSendMessage": a2abridge.MethodSendMessage, "MethodSendStreamingMessage": a2abridge.MethodSendStreamingMessage,
		"MethodGetTask": a2abridge.MethodGetTask, "MethodListTasks": a2abridge.MethodListTasks,
		"MethodSubscribeToTask": a2abridge.MethodSubscribeToTask, "MethodCancelTask": a2abridge.MethodCancelTask,
		"MethodCreatePushConfig": a2abridge.MethodCreatePushConfig, "MethodGetPushConfig": a2abridge.MethodGetPushConfig,
		"MethodListPushConfigs": a2abridge.MethodListPushConfigs, "MethodDeletePushConfig": a2abridge.MethodDeletePushConfig,
		"MethodGetExtendedAgentCard": a2abridge.MethodGetExtendedAgentCard,
	} {
		constants[name] = value
	}
	dispatched := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "A2ARPC" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			tag, ok := sw.Tag.(*ast.SelectorExpr)
			if !ok || tag.Sel.Name != "Method" {
				return true
			}
			for _, stmt := range sw.Body.List {
				for _, expr := range stmt.(*ast.CaseClause).List {
					sel, ok := expr.(*ast.SelectorExpr)
					if !ok {
						t.Errorf("A2ARPC dispatches on %T; name the method with an a2abridge constant", expr)
						continue
					}
					value, known := constants[sel.Sel.Name]
					if !known {
						t.Errorf("A2ARPC dispatches a2abridge.%s, which this test does not know: add it to the constants above and to a2aMethodScopes", sel.Sel.Name)
						continue
					}
					dispatched[value] = true
				}
			}
			return false
		})
		return false
	})
	if len(dispatched) == 0 {
		t.Fatal("found no `switch req.Method` cases in A2ARPC")
	}
	for method := range dispatched {
		if _, ok := a2aMethodScopes[method]; !ok {
			t.Errorf("A2A method %s has no entry in a2aMethodScopes: decide which tasks it reaches and add it", method)
		}
	}
	for method := range a2aMethodScopes {
		if !dispatched[method] {
			t.Errorf("a2aMethodScopes lists %s, which A2ARPC does not dispatch", method)
		}
	}
}

// TestA2AViewAllLogsReachesOnlyReadAndStopMethods drives every classified A2A
// method with a key holding every permission except admin, against a task
// another key created and then one it created itself: on the other key's task
// it is admitted to the read and stop methods and refused (403) on the act
// methods and on answering; on its own task it is admitted to all of them.
// "Admitted" is neither a transport 401/403 nor TaskNotFound (-32001).
func TestA2AViewAllLogsReachesOnlyReadAndStopMethods(t *testing.T) {
	t.Parallel()

	store, keyMgr, mux := setupA2AWith(t, true)
	_, otherKey := mintTaskKey(t, keyMgr, "other")
	var perms []models.Permission
	for _, p := range models.AllPermissions {
		if p != models.PermissionAdmin {
			perms = append(perms, p)
		}
	}
	reader, readerRaw, err := keyMgr.CreateKey(context.Background(), "reader", perms, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create reader key: %v", err)
	}

	// call sends one method against a fresh finished task created by keyID.
	call := func(t *testing.T, method, keyID string) (int, *rpcEnvelope) {
		t.Helper()
		task := &models.Task{ID: uuid.New(), Prompt: "a2a scope probe", Status: models.TaskStatusSuccess,
			CreatedAt: time.Now().UTC(), CreatedByKeyID: &keyID, Timezone: "UTC"}
		if _, err := store.AddTask(task); err != nil {
			t.Fatal(err)
		}
		id := task.ID.String()
		if method == a2abridge.MethodGetPushConfig {
			// A missing config answers TaskNotFound, so give the task one.
			if _, err := store.UpsertA2APushConfig(context.Background(), models.A2APushConfig{TaskID: task.ID, ID: "probe", URL: "https://example.com/hook"}); err != nil {
				t.Fatal(err)
			}
		}
		var params map[string]any
		switch method {
		case a2abridge.MethodSendMessage, a2abridge.MethodSendStreamingMessage:
			params = map[string]any{"message": userMessage("an answer", id), "configuration": map[string]any{"returnImmediately": true}}
		case a2abridge.MethodListTasks:
			params = map[string]any{}
		case a2abridge.MethodCreatePushConfig:
			params = map[string]any{"taskId": id, "id": "probe", "url": "https://example.com/hook"}
		case a2abridge.MethodGetPushConfig, a2abridge.MethodDeletePushConfig:
			params = map[string]any{"taskId": id, "id": "probe"}
		case a2abridge.MethodListPushConfigs:
			params = map[string]any{"taskId": id}
		default:
			params = map[string]any{"id": id}
		}
		rr, env := rpc(t, mux, readerRaw, method, params)
		return rr.Code, env
	}
	admitted := func(code int, env *rpcEnvelope) bool {
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			return false
		}
		return env == nil || env.Error == nil || env.Error.Code != -32001
	}

	for method, scope := range a2aMethodScopes {
		if scope == a2aNoTask {
			continue
		}
		t.Run(method, func(t *testing.T) {
			code, env := call(t, method, otherKey)
			switch scope {
			case a2aRead, a2aStop:
				if !admitted(code, env) {
					t.Errorf("view_all_logs key on another key's task (%s) = %d %+v, want admitted", scope, code, env)
				}
			default:
				if code != http.StatusForbidden {
					t.Errorf("view_all_logs key on another key's task (%s) = %d %+v, want 403", scope, code, env)
				}
			}
			if code, env := call(t, method, reader.KeyID); !admitted(code, env) {
				t.Errorf("key on its own task (%s) = %d %+v, want admitted", scope, code, env)
			}
		})
	}
}

// keyCopiesQuery reads the operator query from docs/TASK-OWNERSHIP.md: the
// ```sql block after the key-copies-query marker.
func keyCopiesQuery(t *testing.T) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "TASK-OWNERSHIP.md"))
	if err != nil {
		t.Fatalf("read TASK-OWNERSHIP.md: %v", err)
	}
	_, after, ok := strings.Cut(string(doc), "<!-- key-copies-query")
	if !ok {
		t.Fatal("TASK-OWNERSHIP.md has no key-copies-query marker")
	}
	_, after, ok = strings.Cut(after, "```sql\n")
	if !ok {
		t.Fatal("no ```sql block after the key-copies-query marker")
	}
	query, _, ok := strings.Cut(after, "```")
	if !ok {
		t.Fatal("unterminated ```sql block after the key-copies-query marker")
	}
	return query
}

// TestKeyCopiesQueryListsCopiesOfOtherPrincipalsTasks runs the documented
// query over seeded copies: it lists a key's copy of a user's task and a key
// copy whose source is gone, and leaves out a key's copy of its own task, a
// bootstrap-admin-key copy (which keeps the source's key), a session user's
// copy, and a copy by the key that created a source an admin gave an owner.
func TestKeyCopiesQueryListsCopiesOfOtherPrincipalsTasks(t *testing.T) {
	t.Parallel()

	env := setupOwnershipEnv(t)
	owner := &env.owner.ID
	other := &env.other.ID
	k := func(id string) *string { return &id }
	add := func(createdBy *uuid.UUID, keyID *string, source *uuid.UUID) *models.Task {
		t.Helper()
		task := &models.Task{ID: uuid.New(), Prompt: "key copies probe", Status: models.TaskStatusSuccess,
			CreatedAt: time.Now().UTC(), CreatedBy: createdBy, CreatedByKeyID: keyID, SourceTaskID: source}
		if _, err := env.store.AddTask(task); err != nil {
			t.Fatalf("add task: %v", err)
		}
		return task
	}

	userTask := add(owner, nil, nil)
	listed := add(owner, k("key-reader"), &userTask.ID)

	keyTask := add(nil, k("key-own"), nil)
	add(nil, k("key-own"), &keyTask.ID) // a key's copy of its own ownerless task

	sourcedByKey := add(owner, k("key-source"), nil)
	add(owner, k("key-source"), &sourcedByKey.ID) // bootstrap admin key copy keeps the source's key

	add(other, nil, &userTask.ID) // a session user's copy

	reassigned := add(owner, k("key-reassigned"), nil) // created by a key, owner set by an admin
	add(owner, k("key-reassigned"), &reassigned.ID)

	gone := uuid.New()
	orphan := add(owner, k("key-orphan"), &gone)

	rows, err := env.store.DB().Conn().QueryContext(context.Background(), keyCopiesQuery(t))
	if err != nil {
		t.Fatalf("run the documented query: %v", err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	got := map[uuid.UUID]bool{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		var id uuid.UUID
		switch v := vals[0].(type) {
		case []byte:
			id, err = uuid.ParseBytes(v)
		case string:
			id, err = uuid.Parse(v)
		case [16]byte:
			id = uuid.UUID(v)
		default:
			t.Fatalf("copy_id column has type %T", v)
		}
		if err != nil {
			t.Fatal(err)
		}
		got[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[uuid.UUID]bool{listed.ID: true, orphan.ID: true}
	if len(got) != len(want) || !got[listed.ID] || !got[orphan.ID] {
		t.Fatalf("query listed %v, want exactly %v (the key's copy of the user's task and the copy whose source is gone)", got, want)
	}
}

// TestSanitizedUploadNamesAreAttachable: a run of dots in an uploaded file's
// name becomes one dot, and leading and trailing dots go, so every stored name
// has the shape task files accept (stagedUploadName, validateTaskCreate).
// Download names (sanitizeFilename) keep their dots.
func TestSanitizedUploadNamesAreAttachable(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"a..b.pdf":         "a.b.pdf",
		"...pdf":           "pdf",
		"..":               "",
		".":                "",
		"...":              "",
		".hidden":          "hidden",
		"trailing.":        "trailing",
		"..lead..trail..":  "lead.trail",
		"Report v1..2.pdf": "Report_v1.2.pdf",
		"plain.txt":        "plain.txt",
	} {
		got := uploadFilename(in)
		if got != want {
			t.Errorf("uploadFilename(%q) = %q, want %q", in, got, want)
		}
		if got != "" && !stagedUploadName(got) {
			t.Errorf("uploadFilename(%q) = %q, which task files refuse", in, got)
		}
	}
	if got := sanitizeFilename("Report v1..2.pdf"); got != "Report_v1..2.pdf" {
		t.Errorf("sanitizeFilename keeps a download name's dots: got %q, want %q", got, "Report_v1..2.pdf")
	}
}

// TestDownloadFindsAnUploadStagedWithADotRun: an upload staged under a name
// holding ".." before uploads collapsed dot runs is still served by that name.
func TestDownloadFindsAnUploadStagedWithADotRun(t *testing.T) {
	t.Parallel()

	env := setupFilesEnv(t)
	env.r.Get("/files/{filename}", env.h.HandleDownload)
	const legacy = "Report_v1..2_ab12cd34.pdf"
	if err := os.MkdirAll(filepath.Join(env.dataDir, "temp_uploads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.dataDir, "temp_uploads", legacy), []byte("old report"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := env.send(t, http.MethodGet, "/files/"+legacy, apiKey("test-admin-key"), "")
	if w.Code != http.StatusOK || w.Body.String() != "old report" {
		t.Fatalf("download of %s = %d %q, want 200 with its bytes", legacy, w.Code, w.Body.String())
	}
}

// TestUploadWithDotRunIsAttachable: an upload named with ".." is staged under
// a name its uploader can attach to a task.
func TestUploadWithDotRunIsAttachable(t *testing.T) {
	t.Parallel()

	env := setupFilesEnv(t)
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "Report v1..2.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("report")); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	apiKey(env.readerRaw)(req)
	w := httptest.NewRecorder()
	env.r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || strings.Contains(got.Filename, "..") {
		t.Fatalf("stored name %q (%v), want one without \"..\"", got.Filename, err)
	}
	if w := env.send(t, http.MethodPost, "/tasks", apiKey(env.readerRaw), createBody(got.Filename)); w.Code != http.StatusOK {
		t.Fatalf("create naming the upload = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestCopyChecksUploadsAgainstTheCaller: a re-run or clone by the key that
// created a task runs as the task's owner once an admin gives it one, but the
// uploads its overrides name are checked against the key, not the owner. So
// the key cannot name the owner's own upload, and still keeps the files
// already on the task.
func TestCopyChecksUploadsAgainstTheCaller(t *testing.T) {
	t.Parallel()

	env := setupFilesEnv(t)
	env.r.With(env.h.AdminOrUserAuthMiddleware).Put("/tasks/{task_id}/owner", env.h.SetTaskOwner)
	_, keyRaw := mustCreateRoleKeyWithID(t, env.h.apiKeys, "client")
	key := apiKey(keyRaw)
	owner := bearer(filesOwnerToken)

	keyUpload := env.upload(t, key)
	w := env.send(t, http.MethodPost, "/tasks", key, createBody(keyUpload))
	if w.Code != http.StatusOK {
		t.Fatalf("key create = %d: %s", w.Code, w.Body.String())
	}
	path := "/tasks/" + decodeCreatedTaskID(t, w).String()
	ownerUpload := env.upload(t, owner)
	if w := env.send(t, http.MethodPut, path+"/owner", apiKey("test-admin-key"), `{"username":"files-owner@example.com"}`); w.Code != http.StatusOK {
		t.Fatalf("admin owner change = %d: %s", w.Code, w.Body.String())
	}

	notFound := "file not found: " + ownerUpload
	for _, verb := range []string{"rerun", "clone"} {
		w := env.send(t, http.MethodPost, path+"/"+verb, key, `{"overrides":{"files":["`+ownerUpload+`"]}}`)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), notFound) {
			t.Errorf("key %s naming the new owner's upload = %d, want 400 %q: %s", verb, w.Code, notFound, w.Body.String())
		}
	}
	if w := env.send(t, http.MethodPost, path+"/rerun", key, ""); w.Code != http.StatusCreated {
		t.Errorf("key rerun keeping the task's files = %d, want 201: %s", w.Code, w.Body.String())
	}
	if w := env.send(t, http.MethodPost, path+"/clone", key, `{"overrides":{"files":["`+keyUpload+`"]}}`); w.Code != http.StatusCreated {
		t.Errorf("key clone naming the task's own file = %d, want 201: %s", w.Code, w.Body.String())
	}
}
