// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
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
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// taskGatesEnv wires every HTTP path that writes a task definition from a
// request, on a deployment whose run ceilings are $10 and 1,000,000 tokens.
// POST /tasks and /tasks/batch authenticate inline, the rest sit behind
// AdminOrUserAuthMiddleware, exactly as in cmd/fleet's orchestrator mux.
type taskGatesEnv struct {
	r      *chi.Mux
	h      *Handlers
	store  *storage.Storage
	keyMgr *apikeys.Manager
}

const (
	gatesTestCostCeiling  = 10.0
	gatesTestTokenCeiling = 1_000_000
)

func setupTaskGates(t *testing.T) *taskGatesEnv {
	t.Helper()
	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	t.Cleanup(cleanup)
	h := New(Config{
		DefaultTaskModel:   "test/model",
		AdminAPIKey:        "test-admin-key",
		Version:            "0.1.0",
		MaxCostUSD:         gatesTestCostCeiling,
		LiveMaxTotalTokens: func() int { return gatesTestTokenCeiling },
	}, store, keyMgr)
	r := chi.NewRouter()
	r.Use(asSeededSession)
	r.Post("/tasks", h.CreateTask)
	r.Post("/tasks/batch", h.CreateTaskBatch)
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Put("/tasks/{task_id}", h.UpdateTask)
		r.Post("/tasks/{task_id}/rerun", h.RerunTask)
		r.Post("/tasks/{task_id}/clone", h.CloneTask)
		r.Post("/tasks/import", h.HandleTaskImport)
	})
	return &taskGatesEnv{r: r, h: h, store: store, keyMgr: keyMgr}
}

// gatesPrincipal is one credential class: how it authenticates, whether it
// carries PermissionAdmin, and how a task it may edit is attributed to it.
type gatesPrincipal struct {
	name  string
	admin bool
	auth  func(*http.Request)
	own   func(*models.Task)
}

func (e *taskGatesEnv) principals(t *testing.T) []gatesPrincipal {
	t.Helper()
	clientKeyID, clientKey := mustCreateRoleKeyWithID(t, e.keyMgr, "client")
	_, adminScopedKey := mustCreateRoleKeyWithID(t, e.keyMgr, "admin")
	clientUser := seedOpsUser(t, e.store, uuid.New(), "task-gates-client@example.com", "client", "task-gates-client-token")
	adminUser := seedOpsUser(t, e.store, uuid.New(), "task-gates-admin@example.com", "admin", "task-gates-admin-token")
	withKey := func(raw string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("X-API-Key", raw) }
	}
	withSession := func(token string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
	}
	return []gatesPrincipal{
		{"client-role key", false, withKey(clientKey), func(task *models.Task) { task.CreatedByKeyID = &clientKeyID }},
		{"client-role user", false, withSession("task-gates-client-token"), func(task *models.Task) { task.CreatedBy = &clientUser.ID }},
		{"admin API key", true, withKey("test-admin-key"), func(*models.Task) {}},
		{"admin-role scoped key", true, withKey(adminScopedKey), func(*models.Task) {}},
		{"admin-role user", true, withSession("task-gates-admin-token"), func(task *models.Task) { task.CreatedBy = &adminUser.ID }},
	}
}

func (e *taskGatesEnv) send(t *testing.T, method, path string, who gatesPrincipal, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	who.auth(req)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

// seedOwned adds a scheduled task the principal may edit, re-run and clone.
func (e *taskGatesEnv) seedOwned(t *testing.T, who gatesPrincipal, name string) *models.Task {
	t.Helper()
	future := time.Now().UTC().Add(time.Hour)
	task := &models.Task{
		ID:           uuid.New(),
		Name:         name,
		Prompt:       "a scheduled task prompt that is long enough",
		Status:       models.TaskStatusScheduled,
		ScheduledFor: &future,
		Priority:     models.PriorityNormal,
		CreatedAt:    time.Now().UTC(),
	}
	who.own(task)
	if _, err := e.store.AddTask(task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return task
}

// gatesOutcome is what one path answered, decoded from its own envelope: the
// task it wrote, or the status and text of its refusal.
type gatesOutcome struct {
	status int
	taskID uuid.UUID // set when a task was written
	detail string    // the refusal text, when none was
}

func errorDetail(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body models.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body (%d): %v: %s", w.Code, err, w.Body.String())
	}
	return body.Detail
}

func createdOrRefused(t *testing.T, w *httptest.ResponseRecorder, okStatus int) gatesOutcome {
	t.Helper()
	if w.Code == okStatus {
		return gatesOutcome{status: w.Code, taskID: decodeCreatedTaskID(t, w)}
	}
	return gatesOutcome{status: w.Code, detail: errorDetail(t, w)}
}

func batchOutcome(t *testing.T, w *httptest.ResponseRecorder) gatesOutcome {
	t.Helper()
	var res models.BatchTaskResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode batch result (%d): %v: %s", w.Code, err, w.Body.String())
	}
	if w.Code == http.StatusOK && len(res.Created) == 1 {
		return gatesOutcome{status: w.Code, taskID: res.Created[0].ID}
	}
	if res.Count != 0 || len(res.Failed) != 1 {
		t.Fatalf("batch of one = %d %+v, want it either created or refused", w.Code, res)
	}
	return gatesOutcome{status: w.Code, detail: res.Failed[0].Error}
}

func importOutcome(t *testing.T, w *httptest.ResponseRecorder) gatesOutcome {
	t.Helper()
	if w.Code != http.StatusOK {
		return gatesOutcome{status: w.Code, detail: errorDetail(t, w)}
	}
	var res models.TaskImportResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode import result: %v: %s", err, w.Body.String())
	}
	if len(res.Results) != 1 || res.Results[0].ID == nil {
		t.Fatalf("import of one = %+v, want one written record", res)
	}
	return gatesOutcome{status: w.Code, taskID: *res.Results[0].ID}
}

func ceilingRecipe(cost *float64, tokens *int) models.TaskCreate {
	return models.TaskCreate{Prompt: "a task prompt that is long enough to validate", MaxCostUSD: cost, MaxTotalTokens: tokens}
}

// gatesPath is one HTTP path that writes a task definition.
type gatesPath struct {
	name string
	// adminOnly: the route refuses a non-admin before it reads a record.
	adminOnly bool
	// do submits the ceilings as who and decodes the answer.
	do func(t *testing.T, e *taskGatesEnv, who gatesPrincipal, cost *float64, tokens *int) gatesOutcome
	// refusal is the status and text this path answers for a record that
	// POST /tasks refuses with 403 and msg, in the path's own envelope. nil on
	// an adminOnly path, which no non-admin reaches.
	refusal func(msg string) (int, string)
}

// taskWritePaths is every HTTP path that writes a task definition a request
// supplies. A2A SendMessage is not a row: its task is built server-side and
// carries no ceiling, and it runs createTaskGoverned like POST /tasks.
func taskWritePaths() []gatesPath {
	asPostTasks := func(msg string) (int, string) { return http.StatusForbidden, msg }
	copyPath := func(verb string) func(*testing.T, *taskGatesEnv, gatesPrincipal, *float64, *int) gatesOutcome {
		return func(t *testing.T, e *taskGatesEnv, who gatesPrincipal, cost *float64, tokens *int) gatesOutcome {
			source := e.seedOwned(t, who, "")
			body := map[string]any{"overrides": map[string]any{"max_cost_usd": cost, "max_total_tokens": tokens}}
			return createdOrRefused(t, e.send(t, http.MethodPost, "/tasks/"+source.ID.String()+"/"+verb, who, body), http.StatusCreated)
		}
	}
	return []gatesPath{
		{
			name: "POST /tasks",
			do: func(t *testing.T, e *taskGatesEnv, who gatesPrincipal, cost *float64, tokens *int) gatesOutcome {
				return createdOrRefused(t, e.send(t, http.MethodPost, "/tasks", who, ceilingRecipe(cost, tokens)), http.StatusOK)
			},
			refusal: asPostTasks,
		},
		{
			// A refused record is a per-task failure: non-atomic → 207 with
			// the other records created (none here).
			name: "POST /tasks/batch",
			do: func(t *testing.T, e *taskGatesEnv, who gatesPrincipal, cost *float64, tokens *int) gatesOutcome {
				body := models.BatchTaskCreate{Tasks: []models.TaskCreate{ceilingRecipe(cost, tokens)}}
				return batchOutcome(t, e.send(t, http.MethodPost, "/tasks/batch", who, body))
			},
			refusal: func(msg string) (int, string) { return http.StatusMultiStatus, msg },
		},
		{
			// Atomic: one refused record aborts the batch with 422.
			name: "POST /tasks/batch (atomic)",
			do: func(t *testing.T, e *taskGatesEnv, who gatesPrincipal, cost *float64, tokens *int) gatesOutcome {
				body := models.BatchTaskCreate{Atomic: true, Tasks: []models.TaskCreate{ceilingRecipe(cost, tokens)}}
				return batchOutcome(t, e.send(t, http.MethodPost, "/tasks/batch", who, body))
			},
			refusal: func(msg string) (int, string) { return http.StatusUnprocessableEntity, msg },
		},
		{
			// Admin-only content rules are checked for every record before any
			// write, like the run_if import rule: 403 naming the record.
			name: "POST /tasks/import",
			do: func(t *testing.T, e *taskGatesEnv, who gatesPrincipal, cost *float64, tokens *int) gatesOutcome {
				env := models.TaskExportEnvelope{Version: models.TaskExportVersion, Tasks: []models.TaskExportRecord{{
					Prompt: "an imported prompt that is long enough", MaxCostUSD: cost, MaxTotalTokens: tokens,
				}}}
				return importOutcome(t, e.send(t, http.MethodPost, "/tasks/import", who, env))
			},
			refusal: func(msg string) (int, string) { return http.StatusForbidden, "task[0] (name=): " + msg },
		},
		{
			name: "PUT /tasks/{id}",
			do: func(t *testing.T, e *taskGatesEnv, who gatesPrincipal, cost *float64, tokens *int) gatesOutcome {
				task := e.seedOwned(t, who, "")
				w := e.send(t, http.MethodPut, "/tasks/"+task.ID.String(), who, ceilingRecipe(cost, tokens))
				if w.Code == http.StatusOK {
					return gatesOutcome{status: w.Code, taskID: task.ID}
				}
				return gatesOutcome{status: w.Code, detail: errorDetail(t, w)}
			},
			refusal: asPostTasks,
		},
		{name: "POST /tasks/{id}/rerun", do: copyPath("rerun"), refusal: asPostTasks},
		{name: "POST /tasks/{id}/clone", do: copyPath("clone"), refusal: asPostTasks},
		{
			name:      "POST /tasks/import?conflict=replace",
			adminOnly: true,
			do: func(t *testing.T, e *taskGatesEnv, who gatesPrincipal, cost *float64, tokens *int) gatesOutcome {
				name := "replace-" + uuid.NewString()
				e.seedOwned(t, who, name)
				env := models.TaskExportEnvelope{Version: models.TaskExportVersion, Tasks: []models.TaskExportRecord{{
					Name: name, Prompt: "a replacing prompt that is long enough", MaxCostUSD: cost, MaxTotalTokens: tokens,
				}}}
				return importOutcome(t, e.send(t, http.MethodPost, "/tasks/import?conflict=replace", who, env))
			},
		},
	}
}

// TestEveryTaskWritePathAppliesTheCeilingRule is the guard table for the
// per-task run ceilings (#1533): on every path that writes a task definition,
// a principal without admin permission is refused a max_cost_usd or
// max_total_tokens above the deployment ceiling with POST /tasks's text (in
// the path's own envelope), an admin is allowed it, and a value at or below
// the ceiling is allowed for everyone and stored as sent. A new write path
// belongs in taskWritePaths.
func TestEveryTaskWritePathAppliesTheCeilingRule(t *testing.T) {
	t.Parallel()

	env := setupTaskGates(t)
	principals := env.principals(t)
	values := []struct {
		name   string
		cost   *float64
		tokens *int
		above  bool
	}{
		{"cost above the ceiling", fptr(25), nil, true},
		{"tokens above the ceiling", nil, iptr(2_000_000), true},
		{"both at the ceiling", fptr(gatesTestCostCeiling), iptr(gatesTestTokenCeiling), false},
		{"both below the ceiling", fptr(4), iptr(500_000), false},
	}

	for _, path := range taskWritePaths() {
		for _, who := range principals {
			if path.adminOnly && !who.admin {
				continue
			}
			for _, v := range values {
				t.Run(path.name+"/"+who.name+"/"+v.name, func(t *testing.T) {
					got := path.do(t, env, who, v.cost, v.tokens)
					if v.above && !who.admin {
						msg := env.h.requireAdminForCeilingsAboveDeployment(false, &models.TaskCreate{MaxCostUSD: v.cost, MaxTotalTokens: v.tokens})
						if msg == "" || !strings.Contains(msg, "only an admin") {
							t.Fatalf("POST /tasks's refusal text = %q, want the ceiling rule's", msg)
						}
						if path.refusal == nil {
							t.Fatalf("%s is admin-only and has no refusal for a non-admin", path.name)
						}
						wantStatus, wantDetail := path.refusal(msg)
						if got.taskID != uuid.Nil || got.status != wantStatus || got.detail != wantDetail {
							t.Fatalf("got status %d, task %s, detail %q; want refused with %d %q",
								got.status, got.taskID, got.detail, wantStatus, wantDetail)
						}
						return
					}
					if got.taskID == uuid.Nil {
						t.Fatalf("got status %d, refusal %q; want the task written", got.status, got.detail)
					}
					stored, err := env.store.GetTask(got.taskID)
					if err != nil || stored == nil {
						t.Fatalf("reload %s: %v", got.taskID, err)
					}
					if v.cost != nil && (stored.MaxCostUSD == nil || *stored.MaxCostUSD != *v.cost) {
						t.Errorf("stored max_cost_usd = %v, want %v", stored.MaxCostUSD, *v.cost)
					}
					if v.tokens != nil && (stored.MaxTotalTokens == nil || *stored.MaxTotalTokens != *v.tokens) {
						t.Errorf("stored max_total_tokens = %v, want %v", stored.MaxTotalTokens, *v.tokens)
					}
				})
			}
		}
	}
}

// TestTaskImportRefusesAnAboveCeilingRecordBeforeAnyWrite pins the import
// convention for an admin-only content rule: one offending record refuses the
// whole envelope before any record is written, and a dry run reports it too.
func TestTaskImportRefusesAnAboveCeilingRecordBeforeAnyWrite(t *testing.T) {
	t.Parallel()

	env := setupTaskGates(t)
	_, clientKey := mustCreateRoleKeyWithID(t, env.keyMgr, "client")
	client := gatesPrincipal{name: "client-role key", auth: func(r *http.Request) { r.Header.Set("X-API-Key", clientKey) }}
	envelope := models.TaskExportEnvelope{Version: models.TaskExportVersion, Tasks: []models.TaskExportRecord{
		{Name: "import-ceiling-fine", Prompt: "a record under the ceiling, long enough", MaxCostUSD: fptr(2)},
		{Name: "import-ceiling-high", Prompt: "a record above the ceiling, long enough", MaxCostUSD: fptr(50)},
	}}
	msg := env.h.requireAdminForCeilingsAboveDeployment(false, &models.TaskCreate{MaxCostUSD: fptr(50)})

	for _, path := range []string{"/tasks/import", "/tasks/import?dry_run=true"} {
		w := env.send(t, http.MethodPost, path, client, envelope)
		if w.Code != http.StatusForbidden || errorDetail(t, w) != "task[1] (name=import-ceiling-high): "+msg {
			t.Fatalf("%s = %d %s, want 403 naming record 1", path, w.Code, w.Body.String())
		}
	}
	if got, err := env.store.GetTaskByName(context.Background(), "import-ceiling-fine"); err != nil || got != nil {
		t.Fatalf("a refused import must write nothing: task=%v err=%v", got, err)
	}
}

// rateLimitedKey is a client-role key allowed limit requests an hour, and its
// id.
func (e *taskGatesEnv) rateLimitedKey(t *testing.T, limit int) (gatesPrincipal, string) {
	t.Helper()
	role := "client"
	key, raw, err := e.keyMgr.CreateKey(context.Background(), "import-rl-"+uuid.NewString(), nil, &role, limit, nil, "")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return gatesPrincipal{name: "rate-limited key", auth: func(r *http.Request) { r.Header.Set("X-API-Key", raw) }, own: func(*models.Task) {}}, key.KeyID
}

// assertKeyUsed checks that the key has used exactly want of its limit hourly
// requests, by probing the counter itself: ConsumeN refuses, and charges
// nothing, when the requests would not fit.
func assertKeyUsed(t *testing.T, keyMgr *apikeys.Manager, keyID string, limit, want int) {
	t.Helper()
	if !keyMgr.ConsumeN(keyID, limit-want) {
		t.Fatalf("the key has used more than %d of its %d requests", want, limit)
	}
	if keyMgr.ConsumeN(keyID, 1) {
		t.Fatalf("the key has used fewer than %d of its %d requests", want, limit)
	}
}

func namedRecords(prefix string, n int) []models.TaskExportRecord {
	records := make([]models.TaskExportRecord, 0, n)
	for i := range n {
		records = append(records, models.TaskExportRecord{
			Name: prefix + string(rune('a'+i)), Prompt: "an imported prompt that is long enough",
		})
	}
	return records
}

// TestTaskImportChargesEveryRecord pins the import's rate-limit accounting to
// the batch path's: an import that writes N records costs the importing key
// exactly N requests of its hourly allowance, and a dry run, which writes
// nothing, exactly one.
func TestTaskImportChargesEveryRecord(t *testing.T) {
	t.Parallel()

	env := setupTaskGates(t)
	const n, limit = 5, 10
	client, keyID := env.rateLimitedKey(t, limit)
	envelope := models.TaskExportEnvelope{Version: models.TaskExportVersion, Tasks: namedRecords("import-rl-", n)}

	if w := env.send(t, http.MethodPost, "/tasks/import?dry_run=true", client, envelope); w.Code != http.StatusOK {
		t.Fatalf("dry run = %d, want 200: %s", w.Code, w.Body.String())
	}
	if w := env.send(t, http.MethodPost, "/tasks/import", client, envelope); w.Code != http.StatusOK {
		t.Fatalf("%d-record import = %d, want 200: %s", n, w.Code, w.Body.String())
	}
	assertKeyUsed(t, env.keyMgr, keyID, limit, 1+n)
}

// TestTaskImportDoesNotChargeSkippedRecords: a record conflict=skip leaves
// alone writes nothing, so it costs nothing; a record the import creates costs
// one, and a request that writes nothing costs only itself. A skip import of 5
// existing names plus 2 new records costs 2, and one of the 5 names alone 1.
func TestTaskImportDoesNotChargeSkippedRecords(t *testing.T) {
	t.Parallel()

	env := setupTaskGates(t)
	const limit = 10
	client, keyID := env.rateLimitedKey(t, limit)
	existing := namedRecords("import-skip-", 5)
	for _, rec := range existing {
		env.seedOwned(t, client, rec.Name)
	}
	withNew := append(append([]models.TaskExportRecord{}, existing...), namedRecords("import-new-", 2)...)

	for i, step := range []struct {
		records []models.TaskExportRecord
		created int
	}{{withNew, 2}, {existing, 0}} {
		envelope := models.TaskExportEnvelope{Version: models.TaskExportVersion, Tasks: step.records}
		w := env.send(t, http.MethodPost, "/tasks/import?conflict=skip", client, envelope)
		if w.Code != http.StatusOK {
			t.Fatalf("skip import %d = %d, want 200: %s", i, w.Code, w.Body.String())
		}
		var res models.TaskImportResponse
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if res.Skipped != 5 || res.Created != step.created {
			t.Fatalf("skip import %d = %+v, want 5 skipped and %d created", i, res, step.created)
		}
	}
	assertKeyUsed(t, env.keyMgr, keyID, limit, 2+1)
}

// TestTaskImportBudgetGatesOnlyCreates: the rolling budget bounds new tasks.
// With the window exhausted, an import that creates is refused with 402,
// while one that only replaces (an edit, which PUT /tasks/{id} does not
// budget either) or only skips goes through.
func TestTaskImportBudgetGatesOnlyCreates(t *testing.T) {
	t.Parallel()

	env := setupTaskGates(t)
	env.h.SetBudgetGate(refusingBudgetGate{})
	admin := gatesPrincipal{name: "admin API key", admin: true, auth: func(r *http.Request) { r.Header.Set("X-API-Key", "test-admin-key") }, own: func(*models.Task) {}}
	env.seedOwned(t, admin, "budget-kept")
	record := func(name string) models.TaskExportEnvelope {
		return models.TaskExportEnvelope{Version: models.TaskExportVersion, Tasks: []models.TaskExportRecord{{Name: name, Prompt: "a budget-gated prompt, long enough"}}}
	}

	for _, c := range []struct {
		path   string
		body   models.TaskExportEnvelope
		status int
	}{
		{"/tasks/import?conflict=replace", record("budget-kept"), http.StatusOK},
		{"/tasks/import?conflict=skip", record("budget-kept"), http.StatusOK},
		{"/tasks/import?conflict=skip", record("budget-new"), http.StatusPaymentRequired},
	} {
		if w := env.send(t, http.MethodPost, c.path, admin, c.body); w.Code != c.status {
			t.Fatalf("%s with %q = %d, want %d: %s", c.path, c.body.Tasks[0].Name, w.Code, c.status, w.Body.String())
		}
	}
}

// TestTaskImportReportsANameTakenAfterThePlan: a planned create whose name
// another request took after the import looked the names up answers as the
// plan would have: skipped under conflict=skip, otherwise POST /tasks's name
// conflict text, not the database's.
func TestTaskImportReportsANameTakenAfterThePlan(t *testing.T) {
	t.Parallel()

	env := setupTaskGates(t)
	admin := gatesPrincipal{name: "admin API key", admin: true, own: func(*models.Task) {}}
	env.seedOwned(t, admin, "taken-late")
	rec := models.TaskExportRecord{Name: "taken-late", Prompt: "a record whose name was taken late, long enough"}
	req := httptest.NewRequest(http.MethodPost, "/tasks/import", nil)
	gates := taskGatesFor(taskCreator{hasAdminPermission: true})

	if got := env.h.importRecord(req, rec, importCreate, models.TaskImportConflictSkip, gates, false, nil); got.Status != models.TaskImportSkipped || got.Reason != "conflict=skip" {
		t.Fatalf("conflict=skip = %+v, want skipped", got)
	}
	want := taskNameTaken("taken-late").detail
	if got := env.h.importRecord(req, rec, importCreate, models.TaskImportConflictError, gates, false, nil); got.Status != models.TaskImportErrored || got.Error != want {
		t.Fatalf("conflict=error = %+v, want errored with %q", got, want)
	}
}

// TestTaskImportChecksOnlyTheRecordsItWrites: the admin-only content rules
// apply to the records an import creates or replaces. A record conflict=skip
// leaves alone writes nothing, so a non-admin re-importing an export that
// holds a gated or above-ceiling task it already has is not refused, in a dry
// run or for real.
func TestTaskImportChecksOnlyTheRecordsItWrites(t *testing.T) {
	t.Parallel()

	env := setupTaskGates(t)
	_, clientKey := mustCreateRoleKeyWithID(t, env.keyMgr, "client")
	client := gatesPrincipal{name: "client-role key", auth: func(r *http.Request) { r.Header.Set("X-API-Key", clientKey) }, own: func(*models.Task) {}}
	env.seedOwned(t, client, "import-kept-high")
	env.seedOwned(t, client, "import-kept-gated")
	envelope := models.TaskExportEnvelope{Version: models.TaskExportVersion, Tasks: []models.TaskExportRecord{
		{Name: "import-kept-high", Prompt: "an exported task above the ceiling, long enough", MaxCostUSD: fptr(50)},
		{Name: "import-kept-gated", Prompt: "an exported gated task, long enough", RunIf: &models.RunIf{Command: "true", TimeoutSeconds: 30}},
		{Name: "import-fresh", Prompt: "a new record under the ceiling, long enough", MaxCostUSD: fptr(2)},
	}}

	for _, path := range []string{"/tasks/import?conflict=skip&dry_run=true", "/tasks/import?conflict=skip"} {
		w := env.send(t, http.MethodPost, path, client, envelope)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200: %s", path, w.Code, w.Body.String())
		}
		var res models.TaskImportResponse
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if res.Skipped != 2 || res.Created != 1 {
			t.Fatalf("%s = %+v, want 2 skipped and 1 created", path, res)
		}
	}
	kept, err := env.store.GetTaskByName(context.Background(), "import-kept-high")
	if err != nil || kept == nil || kept.MaxCostUSD != nil {
		t.Fatalf("a skipped record must leave the row alone: task=%+v err=%v", kept, err)
	}
}

// TestTaskImportReplaceAppliesTheKeyPriorityCeiling: conflict=replace is
// admin-only, but an admin-role scoped key can still carry a priority
// ceiling, which POST /tasks and PUT /tasks/{id} apply to it. A replace is
// the same edit and errors per record.
func TestTaskImportReplaceAppliesTheKeyPriorityCeiling(t *testing.T) {
	t.Parallel()

	env := setupTaskGates(t)
	keyID, raw := mustCreateRoleKeyWithID(t, env.keyMgr, "admin")
	forty := 40
	if err := env.keyMgr.SetMaxPriority(keyID, &forty); err != nil {
		t.Fatalf("SetMaxPriority: %v", err)
	}
	admin := gatesPrincipal{name: "capped admin key", admin: true, auth: func(r *http.Request) { r.Header.Set("X-API-Key", raw) }, own: func(*models.Task) {}}
	seeded := env.seedOwned(t, admin, "replace-urgent")

	w := env.send(t, http.MethodPost, "/tasks/import?conflict=replace", admin, models.TaskExportEnvelope{
		Version: models.TaskExportVersion,
		Tasks:   []models.TaskExportRecord{{Name: "replace-urgent", Prompt: "an urgent replacing prompt, long enough", Priority: 10}},
	})
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("over-cap replace = %d, want 207: %s", w.Code, w.Body.String())
	}
	var res models.TaskImportResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := priorityCapError(&forty, 10).Error(); res.Errors != 1 || res.Results[0].Error != want {
		t.Fatalf("replace result = %+v, want one record refused with %q", res.Results, want)
	}
	got, err := env.store.GetTask(seeded.ID)
	if err != nil || got == nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Priority != models.PriorityNormal || got.Prompt != seeded.Prompt {
		t.Fatalf("a refused replace must leave the row alone: priority=%d prompt=%q", got.Priority, got.Prompt)
	}
}
