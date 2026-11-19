// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// triggerKindRouter mounts both trigger routes on one router, as the
// orchestrator mux does, with a real key manager for the webhook-key form.
func triggerKindRouter(t *testing.T) (*chi.Mux, *storage.Storage, *apikeys.Manager) {
	t.Helper()
	_, store, cleanup := setupTestHandlerWithStore(t)
	t.Cleanup(cleanup)
	keyMgr, err := apikeys.NewManager(filepath.Join(t.TempDir(), "api_keys.json"), filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("key mgr: %v", err)
	}
	h := New(Config{DefaultTaskModel: "test/model", Version: "0.1.0"}, store, keyMgr)
	r := chi.NewRouter()
	r.Use(asSeededSession)
	r.Post("/triggers/{slug}", h.HandleWebhookTrigger)
	r.Post("/triggers/email/{slug}", h.HandleEmailTrigger)
	return r, store, keyMgr
}

// taskCount is the number of task rows: a spawned run is a new row.
func taskCount(t *testing.T, store *storage.Storage) int {
	t.Helper()
	_, total, err := store.GetTasksFiltered(db.TaskFilter{}, 1, 0)
	if err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return total
}

func postTrigger(r http.Handler, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestHandleWebhookTrigger_EmailKindSlugAnsweredAsUnknown pins that the generic
// route answers an email trigger's slug exactly as it answers an unknown slug:
// the same status and the same body, for both ways the route authorizes a call
// (the trigger's HMAC signature, and a fleet_webhook_ key listing the slug), and
// no run is spawned. An email trigger's runs follow the email path's rules
// (approved senders, DKIM/SPF, attachment caps, Message-ID dedup, and no
// connectors unless the template opted in); the generic route applies none of
// them, so it must not fire one.
func TestHandleWebhookTrigger_EmailKindSlugAnsweredAsUnknown(t *testing.T) {
	t.Parallel()

	r, store, keyMgr := triggerKindRouter(t)
	// The template has a connector, so a run spawned by the wrong route would
	// carry it (the webhook spawn inherits the MCP selection).
	mcp := models.MCPSelection{{Server: "github", Account: "default"}}
	secret := seedEmailTrigger(t, store, "inbound-mail", validPolicy(), false, mcp)
	const unknown = "no-such-trigger"
	key := mustCreateTypedKey(t, keyMgr, apikeys.KeyTypeWebhook, []string{"inbound-mail", unknown})

	body, err := json.Marshal(goodEmail())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"HMAC signature", map[string]string{"X-Hub-Signature-256": signBody(secret, body)}},
		{"X-Fleet-Signature-256", map[string]string{"X-Fleet-Signature-256": signBody(secret, body)}},
		{"webhook key listing the slug", map[string]string{"X-API-Key": key}},
		{"webhook key listing the slug, as bearer", map[string]string{"Authorization": "Bearer " + key}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := taskCount(t, store)
			got := postTrigger(r, "/triggers/inbound-mail", body, tc.headers)
			want := postTrigger(r, "/triggers/"+unknown, body, tc.headers)
			if want.Code != http.StatusUnauthorized {
				t.Fatalf("unknown slug answered %d, want 401 (%s)", want.Code, want.Body.String())
			}
			if got.Code != want.Code || got.Body.String() != want.Body.String() {
				t.Errorf("email trigger's slug on the generic route answered %d %q; an unknown slug answers %d %q",
					got.Code, got.Body.String(), want.Code, want.Body.String())
			}
			if after := taskCount(t, store); after != before {
				t.Errorf("the generic route spawned %d run(s) for an email trigger", after-before)
			}
		})
	}

	// Positive control: the same signed body on the email route is accepted, so
	// the refusal above is the route declining the kind, not a bad signature.
	if w := postTrigger(r, "/triggers/email/inbound-mail", body, map[string]string{"X-Hub-Signature-256": signBody(secret, body)}); w.Code != http.StatusAccepted {
		t.Fatalf("email route with the same signature: %d, want 202 (%s)", w.Code, w.Body.String())
	}
}

// TestHandleEmailTrigger_WebhookKindSlugAnsweredAsUnknown is the other
// direction: the email route answers a webhook trigger's slug exactly as an
// unknown slug and spawns nothing, while the webhook trigger still fires on
// its own route.
func TestHandleEmailTrigger_WebhookKindSlugAnsweredAsUnknown(t *testing.T) {
	t.Parallel()

	r, store, _ := triggerKindRouter(t)
	task := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusScheduled, Priority: models.PriorityNormal, TriggerType: models.TriggerTypeWebhook}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	secret := "webhook-kind-secret-webhook-kind-secret"
	if err := store.CreateTrigger(context.Background(), &models.TaskTrigger{ID: uuid.New(), TaskID: task.ID, Slug: "gh-hook", Secret: secret, Kind: models.TriggerKindWebhook}); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	body, err := json.Marshal(goodEmail())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sig := map[string]string{"X-Hub-Signature-256": signBody(secret, body)}

	before := taskCount(t, store)
	got := postTrigger(r, "/triggers/email/gh-hook", body, sig)
	want := postTrigger(r, "/triggers/email/no-such-trigger", body, sig)
	if got.Code != http.StatusUnauthorized || got.Code != want.Code || got.Body.String() != want.Body.String() {
		t.Errorf("webhook trigger's slug on the email route answered %d %q; an unknown slug answers %d %q (want 401)",
			got.Code, got.Body.String(), want.Code, want.Body.String())
	}
	if after := taskCount(t, store); after != before {
		t.Errorf("the email route spawned %d run(s) for a webhook trigger", after-before)
	}

	// The webhook trigger still fires on its own route.
	if w := postTrigger(r, "/triggers/gh-hook", body, sig); w.Code != http.StatusAccepted {
		t.Fatalf("webhook trigger on its own route: %d, want 202 (%s)", w.Code, w.Body.String())
	}
	if after := taskCount(t, store); after != before+1 {
		t.Errorf("webhook trigger on its own route: %d new run(s), want 1", after-before)
	}
}
