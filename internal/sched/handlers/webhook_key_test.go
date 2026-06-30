// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestHandleWebhookTrigger_WebhookKey covers the #190 webhook-key auth path: a
// fleet_webhook_ key scoped to the slug authorizes the trigger WITHOUT an HMAC
// signature, a key scoped to a different slug is 403, and the HMAC path is
// untouched (no key → unchanged behavior).
func TestHandleWebhookTrigger_WebhookKey(t *testing.T) {
	_, store, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()

	keyMgr, err := apikeys.NewManager(filepath.Join(t.TempDir(), "api_keys.json"), filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("key mgr: %v", err)
	}
	h := New(Config{Version: "0.1.0"}, store, keyMgr)
	r := chi.NewRouter()
	r.Post("/triggers/{slug}", h.HandleWebhookTrigger)

	template := models.NewTask(models.TaskCreate{Prompt: "base prompt", TriggerType: models.TriggerTypeWebhook})
	if _, err := store.AddTask(template); err != nil {
		t.Fatalf("seed template task: %v", err)
	}
	trig := &models.TaskTrigger{
		ID:             uuid.New(),
		TaskID:         template.ID,
		Slug:           "gh-hook",
		Secret:         "webhook-secret-at-least-32-bytes-long!",
		PromptTemplate: `Event: {{index .Body "action"}}`,
	}
	if err := store.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	scopedKey := mustCreateTypedKey(t, keyMgr, apikeys.KeyTypeWebhook, []string{"gh-hook"})
	otherKey := mustCreateTypedKey(t, keyMgr, apikeys.KeyTypeWebhook, []string{"other-hook"})
	body := []byte(`{"action":"deploy"}`)

	// 1) Webhook key scoped to the slug → 202, no signature needed.
	req := httptest.NewRequest(http.MethodPost, "/triggers/gh-hook", bytes.NewReader(body))
	req.Header.Set("X-API-Key", scopedKey)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("scoped webhook key: got %d, want 202 (%s)", rec.Code, rec.Body.String())
	}

	// 2) Webhook key NOT scoped to the slug → 403.
	req = httptest.NewRequest(http.MethodPost, "/triggers/gh-hook", bytes.NewReader(body))
	req.Header.Set("X-API-Key", otherKey)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("out-of-scope webhook key: got %d, want 403", rec.Code)
	}

	// 3) Webhook key via Authorization: Bearer also works.
	req = httptest.NewRequest(http.MethodPost, "/triggers/gh-hook", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+scopedKey)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("bearer webhook key: got %d, want 202 (%s)", rec.Code, rec.Body.String())
	}

	// 4) No key, no signature → 401 (HMAC path unchanged).
	req = httptest.NewRequest(http.MethodPost, "/triggers/gh-hook", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-credential request: got %d, want 401", rec.Code)
	}
}
