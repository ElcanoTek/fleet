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

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func mustCreateTypedKey(t *testing.T, keyMgr *apikeys.Manager, kt apikeys.KeyType, slugs []string) string {
	t.Helper()
	_, raw, err := keyMgr.CreateTypedKey("test-"+string(kt), kt, slugs, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create typed key: %v", err)
	}
	return raw
}

// TestTypedKeyRouteScope verifies the #190 middleware type-scope gate on the
// AdminOrUserAuthMiddleware routes: an under-scoped typed key is a definitive
// 403 (not the 401 fall-through), while an in-scope key proceeds normally.
func TestTypedKeyRouteScope(t *testing.T) {
	store, keyMgr, r, cleanup := setupAuthzHandler(t)
	defer cleanup()

	taskA := addTask(t, store, "task A")

	readonlyKey := mustCreateTypedKey(t, keyMgr, apikeys.KeyTypeReadonly, nil)
	webhookKey := mustCreateTypedKey(t, keyMgr, apikeys.KeyTypeWebhook, []string{"pr-review"})
	taskKey := mustCreateTypedKey(t, keyMgr, apikeys.KeyTypeTask, nil)

	t.Run("readonly key may GET tasks", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/tasks", nil)
		req.Header.Set("X-API-Key", readonlyKey)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("readonly GET /tasks = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("readonly key is 403 on a mutation", func(t *testing.T) {
		body, _ := json.Marshal(models.TaskCreate{Prompt: "an edit that is sufficiently long to pass"})
		req := httptest.NewRequest("PUT", "/tasks/"+taskA.ID.String(), bytes.NewReader(body))
		req.Header.Set("X-API-Key", readonlyKey)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("readonly PUT = %d, want 403", w.Code)
		}
	})

	t.Run("webhook key is 403 even on a read route", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/tasks", nil)
		req.Header.Set("X-API-Key", webhookKey)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("webhook GET /tasks = %d, want 403 (webhook keys belong on /triggers only)", w.Code)
		}
	})

	t.Run("task key may read and mutate", func(t *testing.T) {
		// GET
		req := httptest.NewRequest("GET", "/tasks", nil)
		req.Header.Set("X-API-Key", taskKey)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("task GET /tasks = %d, want 200: %s", w.Code, w.Body.String())
		}
		// PUT (UpdateTask gates on create_task, which the task type carries)
		body, _ := json.Marshal(models.TaskCreate{Prompt: "an edit that is sufficiently long to pass"})
		req = httptest.NewRequest("PUT", "/tasks/"+taskA.ID.String(), bytes.NewReader(body))
		req.Header.Set("X-API-Key", taskKey)
		req.Header.Set("Content-Type", "application/json")
		w = httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("task PUT = %d, want 200: %s", w.Code, w.Body.String())
		}
	})
}

// TestTypedKeyCannotCreateTask verifies the task-create family returns 403 (not
// the 401 fall-through) for a valid but under-scoped typed key — the shared
// scopedKeyCannotCreate gate (#190). The 403 fires before any DB work.
func TestTypedKeyCannotCreateTask(t *testing.T) {
	_, store, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()

	keyMgr, err := apikeys.NewManager(filepath.Join(t.TempDir(), "api_keys.json"), filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("key mgr: %v", err)
	}
	h := New(Config{Version: "0.1.0"}, store, keyMgr)
	r := chi.NewRouter()
	r.Post("/tasks", h.CreateTask)

	readonlyKey := mustCreateTypedKey(t, keyMgr, apikeys.KeyTypeReadonly, nil)
	webhookKey := mustCreateTypedKey(t, keyMgr, apikeys.KeyTypeWebhook, []string{"x"})

	for _, tc := range []struct {
		name string
		key  string
	}{
		{"readonly", readonlyKey},
		{"webhook", webhookKey},
	} {
		t.Run(tc.name+" cannot create", func(t *testing.T) {
			body, _ := json.Marshal(models.TaskCreate{Prompt: "a brand new task prompt that is long enough"})
			req := httptest.NewRequest("POST", "/tasks", bytes.NewReader(body))
			req.Header.Set("X-API-Key", tc.key)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s POST /tasks = %d, want 403", tc.name, w.Code)
			}
		})
	}
}
