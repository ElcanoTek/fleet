// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/budget"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// seedFinishedTask persists a finished task created by user and/or keyID with
// one iteration of costUSD inside the env's current day window, and returns
// its id.
func (env *budgetTestEnv) seedFinishedTask(t *testing.T, user *uuid.UUID, keyID *string, costUSD float64) uuid.UUID {
	t.Helper()
	task := &models.Task{ID: uuid.New(), Prompt: "finished work", Status: models.TaskStatusSuccess,
		CreatedAt: env.now.Add(-2 * time.Hour), CreatedBy: user, CreatedByKeyID: keyID}
	if _, err := env.store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if err := env.store.AddTaskIteration(context.Background(), &models.TaskIteration{
		ID: uuid.New(), TaskID: task.ID, IterationNumber: 1, Status: models.IterationStatusPassed,
		StartedAt: env.now.Add(-time.Hour), CostUSD: costUSD, PromptTokens: 100,
	}); err != nil {
		t.Fatalf("AddTaskIteration: %v", err)
	}
	return task.ID
}

// requireExceeded asserts the enforcer refuses a create for p with the given
// window spend.
func requireExceeded(t *testing.T, enf *budget.Enforcer, p models.BudgetPrincipals, wantUSD float64) {
	t.Helper()
	err := enf.CheckCreate(context.Background(), p)
	var exceeded *budget.ExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("CheckCreate(%+v) = %v, want a hard-bound refusal", p, err)
	}
	if math.Abs(exceeded.SpendUSD-wantUSD) > 1e-9 {
		t.Fatalf("CheckCreate(%+v) spend = %.4f, want %.4f", p, exceeded.SpendUSD, wantUSD)
	}
}

// TestBudgetCountsSpendOfDeletedTasks: spend recorded for a task counts toward
// its creator's budgets after the task is deleted — through storage.DeleteTask,
// or by the creator through DELETE /tasks/{id}/permanent — for a user budget
// and a key budget alike. New creates stay refused at the hard bound, and
// GET /admin/budgets keeps reporting the spend.
func TestBudgetCountsSpendOfDeletedTasks(t *testing.T) {
	t.Parallel()

	env, cleanup := setupBudgetEnv(t)
	defer cleanup()
	ctx := context.Background()
	enf := env.newEnforcer()

	const email, token = "ulla@example.com", "tok-budget-deleted-ulla"
	ulla := seedOpsUser(t, env.store, uuid.New(), email, "client", token)
	env.putBudget(t, `{"scope":"user","principal_id":"`+email+`","window":"day","hard_usd":1}`)
	asUlla := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		env.r.ServeHTTP(rec, req)
		return rec
	}
	userP := models.BudgetPrincipals{User: email}

	// Deleted through storage.
	first := env.seedFinishedTask(t, &ulla.ID, nil, 5)
	requireExceeded(t, enf, userP, 5)
	if deleted, err := env.store.DeleteTask(ctx, first); err != nil || !deleted {
		t.Fatalf("DeleteTask = %v, %v", deleted, err)
	}
	requireExceeded(t, enf, userP, 5)

	// Deleted by its creator through the route.
	second := env.seedFinishedTask(t, &ulla.ID, nil, 5)
	requireExceeded(t, enf, userP, 10)
	if rec := asUlla(http.MethodDelete, "/tasks/"+second.String()+"/permanent", ""); rec.Code != http.StatusOK {
		t.Fatalf("DELETE own finished task: got %d (%s)", rec.Code, rec.Body.String())
	}
	if got, _ := env.store.GetTask(second); got != nil {
		t.Fatal("the deleted task row must be gone")
	}
	requireExceeded(t, enf, userP, 10)
	if rec := asUlla(http.MethodPost, "/tasks", `{"prompt":"after deleting the spent tasks"}`); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("create after deleting the spent tasks: got %d, want 402 (%s)", rec.Code, rec.Body.String())
	}

	// A key budget counts the spend of a deleted task created with the key.
	keyID, raw := env.createKey(t)
	env.putBudget(t, `{"scope":"key","principal_id":"`+keyID+`","window":"day","hard_usd":1}`)
	keyTask := env.seedFinishedTask(t, nil, &keyID, 2)
	if w := env.do(t, http.MethodDelete, "/tasks/"+keyTask.String()+"/permanent", "", raw); w.Code != http.StatusOK {
		t.Fatalf("DELETE the key's finished task: got %d (%s)", w.Code, w.Body.String())
	}
	requireExceeded(t, enf, models.BudgetPrincipals{Key: keyID}, 2)
	if w := env.do(t, http.MethodPost, "/tasks", `{"prompt":"key create after delete"}`, raw); w.Code != http.StatusPaymentRequired {
		t.Fatalf("key create after deleting its spent task: got %d, want 402 (%s)", w.Code, w.Body.String())
	}

	// The admin listing reports the same spend.
	w := env.do(t, http.MethodGet, "/admin/budgets", "", "test-admin-key")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/budgets: got %d (%s)", w.Code, w.Body.String())
	}
	var listResp struct {
		Budgets []models.BudgetStatus `json:"budgets"`
	}
	if err := json.NewDecoder(w.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	spend := map[string]float64{}
	for _, s := range listResp.Budgets {
		spend[s.Scope+"/"+s.PrincipalID] = s.SpendUSD
	}
	if got := spend["user/"+email]; math.Abs(got-10) > 1e-9 {
		t.Errorf("listed user spend = %.4f, want 10 (%+v)", got, spend)
	}
	if got := spend["key/"+keyID]; math.Abs(got-2) > 1e-9 {
		t.Errorf("listed key spend = %.4f, want 2 (%+v)", got, spend)
	}
}
