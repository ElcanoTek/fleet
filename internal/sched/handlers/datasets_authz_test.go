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
	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	t.Cleanup(cleanup)
	h := New(Config{DefaultTaskModel: "test/model",
		OrchestratorURL: "http://localhost:8000",
		AdminAPIKey:     "test-admin-key",
		Version:         "0.1.0",
	}, store, keyMgr)
	r := chi.NewRouter()
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
	return r, store, keyMgr
}

func seedDataset(t *testing.T, store *storage.Storage) *models.Dataset {
	t.Helper()
	ds := &models.Dataset{
		ID:      uuid.New(),
		Name:    "authz-" + uuid.NewString()[:8],
		Goal:    "g",
		Model:   "test/model",
		Columns: []models.DatasetColumn{{Name: "in", Type: "text"}, {Name: "out", Type: "text", Output: true}},
		Status:  models.DatasetStatusIdle,
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
	r, store, keyMgr := setupDatasetAuthz(t)
	_, readonlyKey, err := keyMgr.CreateTypedKey("viewer", apikeys.KeyTypeReadonly, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateTypedKey: %v", err)
	}
	clientKey := mustCreateRoleKey(t, keyMgr, "client")
	ds := seedDataset(t, store)
	id := ds.ID.String()

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
		if w := do(datasetRequest(http.MethodPost, "/datasets/"+id+"/rows", clientKey, "application/json", `{"rows":[{"in":"x"}]}`)); w.Code != http.StatusOK {
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
	r, store, keyMgr := setupDatasetAuthz(t)
	clientKey := mustCreateRoleKey(t, keyMgr, "client")
	ds := seedDataset(t, store)

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
