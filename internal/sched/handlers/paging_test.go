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

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// The pure parsing contract: absent → default, in-range → the value, anything
// else → one 400 naming the range. No database needed.
func TestParseLimit(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		wantN    int
		wantCode int // 0 = no error written
	}{
		{"absent uses default", "", 50, 0},
		{"empty uses default", "limit=", 50, 0},
		{"in range", "limit=7", 7, 0},
		{"at max", "limit=500", 500, 0},
		{"zero rejected", "limit=0", 0, http.StatusBadRequest},
		{"negative rejected", "limit=-3", 0, http.StatusBadRequest},
		{"over max rejected", "limit=501", 0, http.StatusBadRequest},
		{"non-numeric rejected", "limit=abc", 0, http.StatusBadRequest},
		{"float rejected", "limit=1.5", 0, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x?"+tc.query, nil)
			w := httptest.NewRecorder()
			n, ok := parseLimit(w, r, 50, 500)
			if tc.wantCode == 0 {
				if !ok || n != tc.wantN {
					t.Fatalf("got (%d, %v), want (%d, true)", n, ok, tc.wantN)
				}
				if w.Code != http.StatusOK || w.Body.Len() != 0 {
					t.Fatalf("expected nothing written on success, got %d %q", w.Code, w.Body.String())
				}
				return
			}
			if ok {
				t.Fatalf("expected rejection, got limit=%d", n)
			}
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantCode)
			}
			if !strings.Contains(w.Body.String(), "must be 1-500") {
				t.Fatalf("error should name the accepted range, got %q", w.Body.String())
			}
		})
	}
}

func TestParseOffset(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		wantN    int
		wantCode int
	}{
		{"absent is zero", "", 0, 0},
		{"zero", "offset=0", 0, 0},
		{"positive", "offset=250", 250, 0},
		{"negative rejected", "offset=-1", 0, http.StatusBadRequest},
		{"non-numeric rejected", "offset=ten", 0, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x?"+tc.query, nil)
			w := httptest.NewRecorder()
			n, ok := parseOffset(w, r)
			if tc.wantCode == 0 {
				if !ok || n != tc.wantN {
					t.Fatalf("got (%d, %v), want (%d, true)", n, ok, tc.wantN)
				}
				return
			}
			if ok || w.Code != tc.wantCode {
				t.Fatalf("got (ok=%v, status=%d), want rejection with %d", ok, w.Code, tc.wantCode)
			}
		})
	}
}

// pagingRouter mounts the four offset/limit list endpoints behind the admin
// key, the way cmd/fleet does, so each request travels the real handler.
func pagingRouter(t *testing.T) (func(method, target string) *httptest.ResponseRecorder, *storage.Storage) {
	t.Helper()
	_, store, cleanup := setupTestHandlerWithStore(t)
	t.Cleanup(cleanup)

	h := New(Config{DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, nil)
	mux := chi.NewRouter()
	mux.Group(func(rt chi.Router) {
		rt.Use(h.AdminAuthMiddleware)
		rt.Get("/tasks", h.ListTasks)
		rt.Get("/tasks/upcoming", h.GetUpcomingRuns)
		rt.Get("/tasks/paused", h.ListPausedTasks)
		rt.Get("/datasets/{datasetID}/rows", h.ListDatasetRows)
	})
	do := func(method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("X-API-Key", "test-admin-key")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	return do, store
}

// TestListEndpoints_RejectBadPagingUniformly is the regression for the
// three endpoints that used to swallow strconv errors: a malformed limit is a
// 400 on every list endpoint, not a silent default on some and a 400 on
// others, and a negative offset on the rows endpoint is a 400 instead of the
// Postgres "OFFSET must not be negative" error surfacing as a 500.
func TestListEndpoints_RejectBadPagingUniformly(t *testing.T) {
	do, store := pagingRouter(t)

	ds := &models.Dataset{
		ID:      uuid.New(),
		Name:    "paging",
		Goal:    "g",
		Columns: []models.DatasetColumn{{Name: "in", Type: "text"}, {Name: "out", Type: "text", Output: true}},
		Status:  models.DatasetStatusIdle,
	}
	if err := store.CreateDataset(context.Background(), ds); err != nil {
		t.Fatalf("CreateDataset: %v", err)
	}
	var cells []json.RawMessage
	for i := 0; i < 5; i++ {
		cells = append(cells, json.RawMessage(`{"in":"row"}`))
	}
	if _, err := store.AddDatasetRows(context.Background(), ds.ID, cells); err != nil {
		t.Fatalf("AddDatasetRows: %v", err)
	}
	rowsPath := "/datasets/" + ds.ID.String() + "/rows"

	bad := []struct {
		name   string
		target string
		want   string // substring of the error body
	}{
		{"tasks limit=abc", "/tasks?limit=abc", "must be 1-500"},
		{"tasks offset=-1", "/tasks?offset=-1", "Invalid offset"},
		{"upcoming limit=0", "/tasks/upcoming?limit=0", "must be 1-1000"},
		{"upcoming limit=abc", "/tasks/upcoming?limit=abc", "must be 1-1000"},
		{"upcoming limit over max", "/tasks/upcoming?limit=1001", "must be 1-1000"},
		{"paused limit=abc", "/tasks/paused?limit=abc", "must be 1-1000"},
		{"paused limit=-5", "/tasks/paused?limit=-5", "must be 1-1000"},
		{"rows limit=abc", rowsPath + "?limit=abc", "must be 1-1000"},
		{"rows limit over max", rowsPath + "?limit=100000", "must be 1-1000"},
		{"rows offset=-1", rowsPath + "?offset=-1", "Invalid offset"},
		{"rows offset=abc", rowsPath + "?offset=abc", "Invalid offset"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			w := do(http.MethodGet, tc.target)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("body %q should contain %q", w.Body.String(), tc.want)
			}
		})
	}

	// And the happy path still pages: absent params return everything (well
	// under the default), explicit limit/offset slice the ordered rows.
	decodeRows := func(w *httptest.ResponseRecorder) []*models.DatasetRow {
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Rows []*models.DatasetRow `json:"rows"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.Rows
	}
	if got := decodeRows(do(http.MethodGet, rowsPath)); len(got) != 5 {
		t.Fatalf("unparameterized list returned %d rows, want 5", len(got))
	}
	page := decodeRows(do(http.MethodGet, rowsPath+"?limit=2&offset=3"))
	if len(page) != 2 || page[0].RowIndex != 3 || page[1].RowIndex != 4 {
		idx := make([]int, 0, len(page))
		for _, r := range page {
			idx = append(idx, r.RowIndex)
		}
		t.Fatalf("limit=2&offset=3 returned row indexes %v, want [3 4]", idx)
	}
	if got := decodeRows(do(http.MethodGet, rowsPath+"?offset=0&limit=1000")); len(got) != 5 {
		t.Fatalf("explicit in-range bounds returned %d rows, want 5", len(got))
	}
}

// The paused queue and upcoming feed keep their defaults when the parameter
// is absent — an unparameterized call is unchanged by the validation.
func TestListEndpoints_AbsentLimitKeepsDefaults(t *testing.T) {
	do, store := pagingRouter(t)

	future := time.Now().UTC().Add(time.Hour)
	task := &models.Task{ID: uuid.New(), Name: "once", Prompt: "p", Status: models.TaskStatusScheduled, ScheduledFor: &future, Priority: 1, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	for _, target := range []string{"/tasks/upcoming", "/tasks/paused", "/tasks"} {
		if w := do(http.MethodGet, target); w.Code != http.StatusOK {
			t.Fatalf("GET %s without paging params = %d, want 200: %s", target, w.Code, w.Body.String())
		}
	}
	w := do(http.MethodGet, "/tasks/upcoming")
	var resp struct {
		Upcoming []UpcomingRun `json:"upcoming"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Upcoming) != 1 || resp.Upcoming[0].TaskID != task.ID.String() {
		t.Fatalf("expected the one seeded one-shot in the default feed, got %+v", resp.Upcoming)
	}
}

// parseBoundedInt is parseLimit's general form for the non-paging knobs
// (?days=, ?runs=, ?grace_period_hours=) that each used to swallow the strconv
// error and keep their default: absent → default, in range → the value,
// anything else → one 400 naming the parameter and its range.
func TestParseBoundedInt(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		wantN    int
		wantCode int // 0 = no error written
	}{
		{"absent uses default", "", 24, 0},
		{"empty uses default", "hours=", 24, 0},
		{"at min", "hours=0", 0, 0},
		{"in range", "hours=12", 12, 0},
		{"at max", "hours=168", 168, 0},
		{"below min rejected", "hours=-1", 0, http.StatusBadRequest},
		{"above max rejected", "hours=87600", 0, http.StatusBadRequest},
		{"non-numeric rejected", "hours=abc", 0, http.StatusBadRequest},
		{"float rejected", "hours=1.5", 0, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x?"+tc.query, nil)
			w := httptest.NewRecorder()
			n, ok := parseBoundedInt(w, r, "hours", 24, 0, 168)
			if tc.wantCode == 0 {
				if !ok || n != tc.wantN {
					t.Fatalf("got (%d, %v), want (%d, true)", n, ok, tc.wantN)
				}
				if w.Body.Len() != 0 {
					t.Fatalf("expected nothing written on success, got %q", w.Body.String())
				}
				return
			}
			if ok {
				t.Fatalf("expected rejection, got %d", n)
			}
			if w.Code != tc.wantCode || !strings.Contains(w.Body.String(), "hours") || !strings.Contains(w.Body.String(), "0-168") {
				t.Fatalf("status=%d body=%q, want 400 naming the parameter and range", w.Code, w.Body.String())
			}
		})
	}
}

// The three call sites: a bad value is a 400 naming the range, not a silent
// default (or, for ?runs=, a silent clamp).
func TestBoundedQueryParams_RejectBadValues(t *testing.T) {
	_, store, cleanup := setupTestHandlerWithStore(t)
	t.Cleanup(cleanup)
	h := New(Config{DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, nil)
	mux := chi.NewRouter()
	mux.Group(func(rt chi.Router) {
		rt.Use(h.AdminAuthMiddleware)
		rt.Get("/sla/report", h.GetSLAReport)
		rt.Get("/admin/pipeline-metrics", h.PipelineMetrics)
	})
	do := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("X-API-Key", "test-admin-key")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	for _, tc := range []struct{ target, want string }{
		{"/sla/report?days=abc", "must be 1-90"},
		{"/sla/report?days=0", "must be 1-90"},
		{"/sla/report?days=365", "must be 1-90"},
		{"/admin/pipeline-metrics?runs=abc", "must be 1-500"},
		{"/admin/pipeline-metrics?runs=501", "must be 1-500"},
	} {
		if w := do(tc.target); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s = %d %q, want 400 containing %q", tc.target, w.Code, w.Body.String(), tc.want)
		}
	}
	for _, target := range []string{"/sla/report", "/sla/report?days=30", "/admin/pipeline-metrics", "/admin/pipeline-metrics?runs=5"} {
		if w := do(target); w.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200: %s", target, w.Code, w.Body.String())
		}
	}
}
