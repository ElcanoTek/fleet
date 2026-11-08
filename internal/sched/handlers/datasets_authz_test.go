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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// setupDatasetAuthz wires every /datasets route the way cmd/fleet does — the
// global body limiter, then AdminOrUserAuthMiddleware — so both the permission
// gate and the import body cap are exercised through the real middleware stack.
func setupDatasetAuthz(t *testing.T) (*chi.Mux, *storage.Storage, *apikeys.Manager) {
	t.Helper()
	r, store, keyMgr, _ := setupDatasetAuthzHandlers(t)
	return r, store, keyMgr
}

// setupDatasetAuthzHandlers is setupDatasetAuthz that also returns the
// handler set, so a test can wire the budget gate or a dataset runner.
func setupDatasetAuthzHandlers(t *testing.T) (*chi.Mux, *storage.Storage, *apikeys.Manager, *Handlers) {
	t.Helper()
	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	t.Cleanup(cleanup)
	h := New(Config{SharedToken: testSharedToken, DefaultTaskModel: "test/model",
		AdminAPIKey: "test-admin-key",
		Version:     "0.1.0",
	}, store, keyMgr)
	r := chi.NewRouter()
	r.Use(asSeededSession)
	r.Use(h.BodySizeLimitMiddleware)
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Get("/datasets", h.ListDatasets)
		r.Post("/datasets", h.CreateDataset)
		r.Get("/datasets/{datasetID}", h.GetDataset)
		r.Delete("/datasets/{datasetID}", h.DeleteDataset)
		r.Get("/datasets/{datasetID}/rows", h.ListDatasetRows)
		r.Post("/datasets/{datasetID}/rows", h.ImportDatasetRows)
		r.Post("/datasets/{datasetID}/run", h.RunDataset)
		r.Post("/datasets/{datasetID}/pause", h.PauseDataset)
		r.Post("/datasets/{datasetID}/approve", h.ApproveDatasetRows)
		r.Post("/datasets/{datasetID}/rerun", h.RerunDatasetRows)
		r.Get("/datasets/{datasetID}/export", h.ExportDataset)
	})
	return r, store, keyMgr, h
}

// seedDataset inserts a dataset with no recorded creator (fleet-wide
// principals only).
func seedDataset(t *testing.T, store *storage.Storage) *models.Dataset {
	t.Helper()
	return seedOwnedDataset(t, store, nil, nil)
}

// seedOwnedDataset inserts a dataset attributed to the given creating user
// and/or API key (either may be nil).
func seedOwnedDataset(t *testing.T, store *storage.Storage, userID *uuid.UUID, keyID *string) *models.Dataset {
	t.Helper()
	ds := &models.Dataset{
		ID:      uuid.New(),
		Name:    "authz-" + uuid.NewString()[:8],
		Goal:    "g",
		Model:   "test/model",
		Columns: []models.DatasetColumn{{Name: "in", Type: "text"}, {Name: "out", Type: "text", Output: true}},
		Status:  models.DatasetStatusIdle,

		CreatedByUserID: userID,
		CreatedByKeyID:  keyID,
	}
	if err := store.CreateDataset(context.Background(), ds); err != nil {
		t.Fatalf("CreateDataset: %v", err)
	}
	return ds
}

func datasetRequest(method, path, apiKey, contentType string, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-API-Key", apiKey)
	return req
}

// TestDatasetsRequirePermissions is the regression for the /datasets surface
// carrying no permission check at all: the routes sit in the authenticate-only
// AdminOrUserAuthMiddleware group, so a readonly key (or user) could create,
// delete, import into and RUN a dataset — each row an agent run at the pinned
// model. Reads need view_tasks, mutations need create_task, like the task
// surface.
func TestDatasetsRequirePermissions(t *testing.T) {
	t.Parallel()

	r, store, keyMgr := setupDatasetAuthz(t)
	readonly, readonlyKey, err := keyMgr.CreateTypedKey(context.Background(), "viewer", apikeys.KeyTypeReadonly, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateTypedKey: %v", err)
	}
	clientKeyID, clientKey := mustCreateRoleKeyWithID(t, keyMgr, "client")
	// The readonly key reads its own dataset; the mutations below are refused
	// on permission before ownership is consulted.
	ds := seedOwnedDataset(t, store, nil, &readonly.KeyID)
	id := ds.ID.String()
	clientDS := seedOwnedDataset(t, store, nil, &clientKeyID)

	do := func(req *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	createBody := `{"name":"n","goal":"g","model":"test/model","columns":[{"name":"in","type":"text"},{"name":"out","type":"text","output":true}]}`

	t.Run("readonly key may read", func(t *testing.T) {
		for _, path := range []string{"/datasets", "/datasets/" + id, "/datasets/" + id + "/rows", "/datasets/" + id + "/export"} {
			if w := do(datasetRequest(http.MethodGet, path, readonlyKey, "", "")); w.Code != http.StatusOK {
				t.Fatalf("readonly GET %s = %d, want 200: %s", path, w.Code, w.Body.String())
			}
		}
	})

	t.Run("readonly key may not mutate", func(t *testing.T) {
		mutations := []struct{ method, path, ct, body string }{
			{http.MethodPost, "/datasets", "application/json", createBody},
			{http.MethodDelete, "/datasets/" + id, "", ""},
			{http.MethodPost, "/datasets/" + id + "/rows", "application/json", `{"rows":[{"in":"x"}]}`},
			{http.MethodPost, "/datasets/" + id + "/run", "", ""},
			{http.MethodPost, "/datasets/" + id + "/pause", "", ""},
			{http.MethodPost, "/datasets/" + id + "/approve", "application/json", `{}`},
			{http.MethodPost, "/datasets/" + id + "/rerun", "application/json", `{}`},
		}
		for _, m := range mutations {
			if w := do(datasetRequest(m.method, m.path, readonlyKey, m.ct, m.body)); w.Code != http.StatusForbidden {
				t.Fatalf("readonly %s %s = %d, want 403: %s", m.method, m.path, w.Code, w.Body.String())
			}
		}
		if got, err := store.GetDataset(context.Background(), ds.ID); err != nil || got == nil {
			t.Fatalf("the refused DELETE must not have removed the dataset: %v", err)
		}
	})

	t.Run("create_task key may mutate", func(t *testing.T) {
		if w := do(datasetRequest(http.MethodPost, "/datasets", clientKey, "application/json", createBody)); w.Code != http.StatusOK {
			t.Fatalf("client POST /datasets = %d, want 200: %s", w.Code, w.Body.String())
		}
		if w := do(datasetRequest(http.MethodPost, "/datasets/"+clientDS.ID.String()+"/rows", clientKey, "application/json", `{"rows":[{"in":"x"}]}`)); w.Code != http.StatusOK {
			t.Fatalf("client import = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("a missing dataset is 404, a malformed id 400", func(t *testing.T) {
		if w := do(datasetRequest(http.MethodGet, "/datasets/"+uuid.NewString(), clientKey, "", "")); w.Code != http.StatusNotFound {
			t.Fatalf("unknown id = %d, want 404: %s", w.Code, w.Body.String())
		}
		if w := do(datasetRequest(http.MethodGet, "/datasets/not-a-uuid", clientKey, "", "")); w.Code != http.StatusBadRequest {
			t.Fatalf("malformed id = %d, want 400: %s", w.Code, w.Body.String())
		}
	})
}

// TestDatasetImportBodyCapIsTheHandlers pins that the dataset import's own 16
// MiB cap governs: the 1 MiB global JSON-body limiter used to wrap the import
// path too, so a 2 MiB CSV failed under the global cap with a misleading
// error and the handler's MaxBytesReader was dead code. The exemption is
// narrow — POST /datasets (create) stays under the global cap.
func TestDatasetImportBodyCapIsTheHandlers(t *testing.T) {
	t.Parallel()

	r, store, keyMgr := setupDatasetAuthz(t)
	clientKeyID, clientKey := mustCreateRoleKeyWithID(t, keyMgr, "client")
	ds := seedOwnedDataset(t, store, nil, &clientKeyID)

	// ~1.6 MiB of CSV: 4000 rows × 400 bytes, under the 5000-row cap.
	var csv strings.Builder
	csv.WriteString("in\n")
	row := strings.Repeat("x", 400) + "\n"
	for i := 0; i < 4000; i++ {
		csv.WriteString(row)
	}
	if csv.Len() <= MaxJSONBodySize {
		t.Fatalf("fixture is %d bytes; it must exceed the %d-byte global cap to prove anything", csv.Len(), MaxJSONBodySize)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, datasetRequest(http.MethodPost, "/datasets/"+ds.ID.String()+"/rows", clientKey, "text/csv", csv.String()))
	if w.Code != http.StatusOK {
		t.Fatalf("1.6 MiB CSV import = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got struct {
		Imported int `json:"imported"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Imported != 4000 {
		t.Fatalf("imported = %d (err %v), want 4000", got.Imported, err)
	}

	// The global cap still applies to the other dataset POSTs.
	big := `{"name":"n","goal":"` + strings.Repeat("g", MaxJSONBodySize+1) + `","model":"m","columns":[]}`
	w = httptest.NewRecorder()
	r.ServeHTTP(w, datasetRequest(http.MethodPost, "/datasets", clientKey, "application/json", big))
	if w.Code == http.StatusOK {
		t.Fatalf("a >1 MiB POST /datasets must still be refused by the global body cap, got 200")
	}
}

// stubDatasetRunner accepts every Start/Pause, so a test can tell an
// authorized run (200) from a refused one without an agent engine.
type stubDatasetRunner struct{}

func (stubDatasetRunner) Start(context.Context, uuid.UUID) error { return nil }
func (stubDatasetRunner) Pause(uuid.UUID) bool                   { return true }

func listDatasetIDs(t *testing.T, r http.Handler, req *http.Request) map[string]bool {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /datasets = %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Datasets []models.Dataset `json:"datasets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	ids := map[string]bool{}
	for _, d := range got.Datasets {
		ids[d.ID.String()] = true
	}
	return ids
}

// TestDatasetsAreCreatorScoped pins the own-rows model for datasets, the same
// one tasks use: a principal reaches the datasets it created (404 for any
// other, like an unknown id), lists only those, and admin and view_all_logs
// principals reach every dataset, including one with no recorded creator.
func TestDatasetsAreCreatorScoped(t *testing.T) {
	t.Parallel()

	r, store, keyMgr, h := setupDatasetAuthzHandlers(t)
	h.SetDatasetRunner(stubDatasetRunner{})

	alice := addLogUser(t, store, "alice-"+uuid.NewString()[:8], "client", "ds-alice-token")
	addLogUser(t, store, "bob-"+uuid.NewString()[:8], "client", "ds-bob-token")
	addLogUser(t, store, "root-"+uuid.NewString()[:8], "admin", "ds-root-token")
	keyID, key := mustCreateRoleKeyWithID(t, keyMgr, "client")
	_, otherKey := mustCreateRoleKeyWithID(t, keyMgr, "client")
	_, auditorKey, err := keyMgr.CreateKey(context.Background(), "auditor", []models.Permission{models.PermissionViewTasks, models.PermissionViewAllLogs},
		nil, 0, nil, "fleet-wide auditor")
	if err != nil {
		t.Fatalf("create auditor key: %v", err)
	}

	aliceDS := seedOwnedDataset(t, store, &alice.ID, nil)
	keyDS := seedOwnedDataset(t, store, nil, &keyID)
	legacyDS := seedDataset(t, store)

	bearer := func(method, path, token, ct, body string) *http.Request {
		req := datasetRequest(method, path, "", ct, body)
		req.Header.Del("X-API-Key")
		req.Header.Set("Authorization", "Bearer "+token)
		return req
	}
	do := func(req *http.Request) int {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	routes := func(id string) []struct{ method, path, ct, body string } {
		base := "/datasets/" + id
		return []struct{ method, path, ct, body string }{
			{http.MethodGet, base, "", ""},
			{http.MethodGet, base + "/rows", "", ""},
			{http.MethodGet, base + "/export", "", ""},
			{http.MethodPost, base + "/rows", "application/json", `{"rows":[{"in":"x"}]}`},
			{http.MethodPost, base + "/run", "", ""},
			{http.MethodPost, base + "/pause", "", ""},
			{http.MethodPost, base + "/approve", "application/json", `{}`},
			{http.MethodPost, base + "/rerun", "application/json", `{}`},
			{http.MethodDelete, base, "", ""},
		}
	}

	t.Run("another user gets 404 on every route", func(t *testing.T) {
		for _, rt := range routes(aliceDS.ID.String()) {
			if code := do(bearer(rt.method, rt.path, "ds-bob-token", rt.ct, rt.body)); code != http.StatusNotFound {
				t.Errorf("bob %s %s = %d, want 404", rt.method, rt.path, code)
			}
		}
		if _, err := store.GetDataset(context.Background(), aliceDS.ID); err != nil {
			t.Fatalf("dataset must survive the refused DELETE: %v", err)
		}
	})

	t.Run("another key gets 404 on every route", func(t *testing.T) {
		for _, rt := range routes(keyDS.ID.String()) {
			if code := do(datasetRequest(rt.method, rt.path, otherKey, rt.ct, rt.body)); code != http.StatusNotFound {
				t.Errorf("other key %s %s = %d, want 404", rt.method, rt.path, code)
			}
		}
	})

	t.Run("a dataset with no recorded creator is fleet-wide only", func(t *testing.T) {
		if code := do(bearer(http.MethodGet, "/datasets/"+legacyDS.ID.String(), "ds-alice-token", "", "")); code != http.StatusNotFound {
			t.Errorf("client user GET of an ownerless dataset = %d, want 404", code)
		}
		if code := do(datasetRequest(http.MethodGet, "/datasets/"+legacyDS.ID.String(), key, "", "")); code != http.StatusNotFound {
			t.Errorf("client key GET of an ownerless dataset = %d, want 404", code)
		}
	})

	t.Run("the creator reaches its own dataset", func(t *testing.T) {
		for _, rt := range routes(aliceDS.ID.String()) {
			if rt.method == http.MethodDelete || strings.HasSuffix(rt.path, "/pause") {
				continue
			}
			if code := do(bearer(rt.method, rt.path, "ds-alice-token", rt.ct, rt.body)); code != http.StatusOK {
				t.Errorf("alice %s %s = %d, want 200", rt.method, rt.path, code)
			}
		}
		if code := do(datasetRequest(http.MethodGet, "/datasets/"+keyDS.ID.String()+"/export", key, "", "")); code != http.StatusOK {
			t.Errorf("creating key export = %d, want 200", code)
		}
		if code := do(datasetRequest(http.MethodDelete, "/datasets/"+keyDS.ID.String(), key, "", "")); code != http.StatusNoContent {
			t.Errorf("creating key DELETE = %d, want 204", code)
		}
	})

	t.Run("list is filtered to own datasets", func(t *testing.T) {
		req := bearer(http.MethodGet, "/datasets", "ds-alice-token", "", "")
		ids := listDatasetIDs(t, r, req)
		if len(ids) != 1 || !ids[aliceDS.ID.String()] {
			t.Errorf("alice lists %v, want only %s", ids, aliceDS.ID)
		}
		if ids := listDatasetIDs(t, r, bearer(http.MethodGet, "/datasets", "ds-bob-token", "", "")); len(ids) != 0 {
			t.Errorf("bob lists %v, want none", ids)
		}
	})

	t.Run("admin and view_all_logs reach every dataset", func(t *testing.T) {
		for _, req := range []*http.Request{
			bearer(http.MethodGet, "/datasets", "ds-root-token", "", ""),
			datasetRequest(http.MethodGet, "/datasets", "test-admin-key", "", ""),
			datasetRequest(http.MethodGet, "/datasets", auditorKey, "", ""),
		} {
			ids := listDatasetIDs(t, r, req)
			if !ids[aliceDS.ID.String()] || !ids[legacyDS.ID.String()] {
				t.Errorf("fleet-wide list %v must include %s and %s", ids, aliceDS.ID, legacyDS.ID)
			}
		}
		if code := do(datasetRequest(http.MethodGet, "/datasets/"+aliceDS.ID.String()+"/export", auditorKey, "", "")); code != http.StatusOK {
			t.Errorf("view_all_logs export = %d, want 200", code)
		}
		if code := do(bearer(http.MethodPost, "/datasets/"+legacyDS.ID.String()+"/run", "ds-root-token", "", "")); code != http.StatusOK {
			t.Errorf("admin user run of an ownerless dataset = %d, want 200", code)
		}
	})
}

// TestDatasetCreateRecordsCreator pins that POST /datasets records the
// creating principal, so the creator can reach the dataset afterwards.
func TestDatasetCreateRecordsCreator(t *testing.T) {
	t.Parallel()

	r, store, keyMgr := setupDatasetAuthz(t)
	keyID, key := mustCreateRoleKeyWithID(t, keyMgr, "client")
	alice := addLogUser(t, store, "alice-"+uuid.NewString()[:8], "client", "dsc-alice-token")
	body := `{"name":"n","goal":"g","model":"test/model","columns":[{"name":"in","type":"text"},{"name":"out","type":"text","output":true}]}`

	create := func(req *http.Request) *models.Dataset {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("POST /datasets = %d: %s", w.Code, w.Body.String())
		}
		var d models.Dataset
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got, err := store.GetDataset(context.Background(), d.ID)
		if err != nil {
			t.Fatalf("GetDataset: %v", err)
		}
		return got
	}

	byKey := create(datasetRequest(http.MethodPost, "/datasets", key, "application/json", body))
	if byKey.CreatedByKeyID == nil || *byKey.CreatedByKeyID != keyID || byKey.CreatedByUserID != nil {
		t.Errorf("key-created dataset creator = (%v, %v), want key %s", byKey.CreatedByUserID, byKey.CreatedByKeyID, keyID)
	}

	req := datasetRequest(http.MethodPost, "/datasets", "", "application/json", body)
	req.Header.Del("X-API-Key")
	req.Header.Set("Authorization", "Bearer dsc-alice-token")
	byUser := create(req)
	if byUser.CreatedByUserID == nil || *byUser.CreatedByUserID != alice.ID || byUser.CreatedByKeyID != nil {
		t.Errorf("user-created dataset creator = (%v, %v), want user %s", byUser.CreatedByUserID, byUser.CreatedByKeyID, alice.ID)
	}
}

// TestDatasetRunHonorsBudget pins that the actions leading to row runs (run,
// re-run, row import) go through the same per-principal budget gate as task
// creation: an exhausted window answers 402 with Retry-After.
func TestDatasetRunHonorsBudget(t *testing.T) {
	t.Parallel()

	r, store, keyMgr, h := setupDatasetAuthzHandlers(t)
	h.SetDatasetRunner(stubDatasetRunner{})
	keyID, key := mustCreateRoleKeyWithID(t, keyMgr, "client")
	ds := seedOwnedDataset(t, store, nil, &keyID)
	base := "/datasets/" + ds.ID.String()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, datasetRequest(http.MethodPost, base+"/run", key, "", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("run without a budget gate = %d, want 200: %s", w.Code, w.Body.String())
	}

	h.SetBudgetGate(refusingBudgetGate{})
	for _, rt := range []struct{ path, ct, body string }{
		{base + "/run", "", ""},
		{base + "/rerun", "application/json", `{}`},
		{base + "/rows", "application/json", `{"rows":[{"in":"x"}]}`},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, datasetRequest(http.MethodPost, rt.path, key, rt.ct, rt.body))
		if w.Code != http.StatusPaymentRequired {
			t.Errorf("POST %s over budget = %d, want 402: %s", rt.path, w.Code, w.Body.String())
		}
		if w.Header().Get("Retry-After") == "" {
			t.Errorf("POST %s over budget: missing Retry-After", rt.path)
		}
	}
}
