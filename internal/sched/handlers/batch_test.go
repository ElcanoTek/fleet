// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// postBatch issues an authenticated POST /tasks/batch against the test router
// and returns the recorder + decoded result.
func postBatch(t *testing.T, r http.Handler, body string) (*httptest.ResponseRecorder, models.BatchTaskResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/tasks/batch", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var res models.BatchTaskResult
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &res)
	}
	return w, res
}

func TestCreateTaskBatch_AllValidAtomic(t *testing.T) {
	r, store, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()

	body := `{"atomic":true,"tasks":[
		{"prompt":"Review file a","priority":3},
		{"prompt":"Review file b","priority":4}
	]}`
	w, res := postBatch(t, r, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if res.Count != 2 || len(res.Created) != 2 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v, want Count=2 Created=2 Failed=0", res)
	}
	// Each created entry carries the assigned UUID + its request index.
	for i, c := range res.Created {
		if c.Index != i {
			t.Errorf("created[%d].Index = %d, want %d", i, c.Index, i)
		}
		if c.ID == [16]byte{} {
			t.Errorf("created[%d].ID is zero", i)
		}
		got, err := store.GetTask(c.ID)
		if err != nil || got == nil {
			t.Errorf("created task %s not persisted: %v", c.ID, err)
		}
	}
}

func TestCreateTaskBatch_PartialFailNonAtomic(t *testing.T) {
	r, store, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()

	// Index 1 has an empty prompt (validation failure); indices 0 and 2 are valid.
	// Non-atomic: the two valid tasks are created, the invalid one is skipped, 207.
	body := `{"atomic":false,"tasks":[
		{"prompt":"Review file a"},
		{"prompt":"   "},
		{"prompt":"Review file c"}
	]}`
	w, res := postBatch(t, r, body)
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207 (%s)", w.Code, w.Body.String())
	}
	if res.Count != 2 {
		t.Fatalf("Count = %d, want 2", res.Count)
	}
	if len(res.Created) != 2 || len(res.Failed) != 1 {
		t.Fatalf("len(Created)=%d len(Failed)=%d, want 2/1", len(res.Created), len(res.Failed))
	}
	if res.Failed[0].Index != 1 {
		t.Errorf("failed index = %d, want 1", res.Failed[0].Index)
	}
	if res.Failed[0].Error == "" {
		t.Error("failed error is empty")
	}
	// The valid tasks were persisted; the invalid one was not.
	for _, c := range res.Created {
		if got, _ := store.GetTask(c.ID); got == nil {
			t.Errorf("valid task %s not persisted", c.ID)
		}
	}
}

func TestCreateTaskBatch_AtomicAbortsOnValidationFailure(t *testing.T) {
	r, store, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()

	// Atomic + a validation failure (index 1 empty prompt): nothing is created,
	// 422, the first failure is reported plus every later index as "aborted".
	body := `{"atomic":true,"tasks":[
		{"prompt":"Review file a"},
		{"prompt":""},
		{"prompt":"Review file c"}
	]}`
	w, res := postBatch(t, r, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", w.Code, w.Body.String())
	}
	if res.Count != 0 || len(res.Created) != 0 {
		t.Fatalf("atomic failure must create nothing, got %+v", res)
	}
	if len(res.Failed) < 2 {
		t.Fatalf("expected the failing index + aborted tail, got %d failures", len(res.Failed))
	}
	// Nothing was persisted.
	for _, c := range res.Created {
		if got, _ := store.GetTask(c.ID); got != nil {
			t.Errorf("atomic failure persisted task %s", c.ID)
		}
	}
}

func TestCreateTaskBatch_OversizedRejected(t *testing.T) {
	r, _, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()

	// Build a batch of MaxBatchSize+1 valid tasks to exceed the ceiling.
	tasks := make([]models.TaskCreate, MaxBatchSize+1)
	for i := range tasks {
		tasks[i] = models.TaskCreate{Prompt: "Review file"}
	}
	body, _ := json.Marshal(models.BatchTaskCreate{Tasks: tasks})
	w, _ := postBatch(t, r, string(body))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%s)", w.Code, w.Body.String())
	}
}

func TestCreateTaskBatch_EmptyRejected(t *testing.T) {
	r, _, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()

	w, _ := postBatch(t, r, `{"tasks":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestCreateTaskBatch_Unauthorized(t *testing.T) {
	r, _, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/tasks/batch", bytes.NewBufferString(`{"tasks":[{"prompt":"x"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no auth)", w.Code)
	}
}

// TestCreateTaskBatch_RateLimitAccounting verifies a batch of N tasks charges N
// tokens against the scoped key's hourly cap (ValidateKey charges 1, the handler
// charges N-1 via ConsumeN), so a single batch larger than the key's cap is
// rejected with 429 — the batch endpoint cannot be a rate-limit bypass.
func TestCreateTaskBatch_RateLimitAccounting(t *testing.T) {
	r, _, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()

	// Create a scoped "client" key with a tight hourly cap of 3 via the HTTP API
	// (POST /keys is admin-gated and wired in the test router).
	createBody := `{"name":"batch-rl","role":"client","rate_limit":3}`
	req := httptest.NewRequest(http.MethodPost, "/keys", bytes.NewBufferString(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create key: status = %d (%s)", w.Code, w.Body.String())
	}
	var created models.APIKeyCreated
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created key: %v", err)
	}
	raw := created.APIKey

	// A 3-task batch fits exactly the cap: ValidateKey charges 1, ConsumeN charges
	// 2 → 1+2 = 3 == cap. The batch succeeds (200, Count=3).
	body := `{"tasks":[
		{"prompt":"Review file a"},
		{"prompt":"Review file b"},
		{"prompt":"Review file c"}
	]}`
	req = httptest.NewRequest(http.MethodPost, "/tasks/batch", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", raw)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("within-cap batch: status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var res models.BatchTaskResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.Count != 3 {
		t.Fatalf("within-cap Count = %d, want 3", res.Count)
	}

	// A second key with the SAME cap of 3, then a 5-task batch: ValidateKey
	// charges 1 (1 <= 3, ok), ConsumeN charges 4 → 1+4 = 5 > 3 → 429. The batch
	// endpoint cannot bypass the per-key hourly cap by lumping N tasks into one
	// HTTP request.
	createBody = `{"name":"batch-rl2","role":"client","rate_limit":3}`
	req = httptest.NewRequest(http.MethodPost, "/keys", bytes.NewBufferString(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-admin-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create key2: status = %d (%s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created key2: %v", err)
	}
	raw2 := created.APIKey

	big := `{"tasks":[
		{"prompt":"Review file a"},
		{"prompt":"Review file b"},
		{"prompt":"Review file c"},
		{"prompt":"Review file d"},
		{"prompt":"Review file e"}
	]}`
	req = httptest.NewRequest(http.MethodPost, "/tasks/batch", bytes.NewBufferString(big))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", raw2)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap batch: status = %d, want 429 (%s)", w.Code, w.Body.String())
	}
}
