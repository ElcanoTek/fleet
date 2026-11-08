// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestCreateTaskDuplicateNameIsConflict: a task name is unique (the partial
// unique index idx_tasks_name_unique), so creating a second task under a name
// already in use is the caller's mistake — 409 with a message naming the
// clash — not the 500 "Failed to create task" the raw unique violation used to
// surface as. Unnamed tasks never collide.
func TestCreateTaskDuplicateNameIsConflict(t *testing.T) {
	t.Parallel()

	r, cleanup := setupTestHandler(t)
	defer cleanup()

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", "test-admin-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	named := `{"name":"nightly-report","prompt":"summarise the day"}`
	if w := post(named); w.Code != http.StatusOK {
		t.Fatalf("first create: %d %s, want 200", w.Code, w.Body.String())
	}
	w := post(named)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate name: %d %s, want 409", w.Code, w.Body.String())
	}
	var body models.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 409 body: %v (%s)", err, w.Body.String())
	}
	if !strings.Contains(body.Detail, `"nightly-report" already exists`) {
		t.Errorf("409 detail %q does not name the clashing task", body.Detail)
	}

	// Unnamed tasks are exempt from the index: any number may be created.
	for i := range 2 {
		if w := post(`{"prompt":"unnamed"}`); w.Code != http.StatusOK {
			t.Fatalf("unnamed create #%d: %d %s, want 200", i+1, w.Code, w.Body.String())
		}
	}
}

// TestIsTaskNameConflict pins the classifier to the name index: a unique
// violation on any other constraint, or any other error, stays a 500.
func TestIsTaskNameConflict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"name index violation", &pgconn.PgError{Code: "23505", ConstraintName: taskNameUniqueIndex}, true},
		{"wrapped", fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505", ConstraintName: taskNameUniqueIndex}), true},
		{"other unique index", &pgconn.PgError{Code: "23505", ConstraintName: "tasks_pkey"}, false},
		{"other sqlstate on the index", &pgconn.PgError{Code: "23503", ConstraintName: taskNameUniqueIndex}, false},
		{"plain error", errors.New("23505 idx_tasks_name_unique"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isTaskNameConflict(tc.err); got != tc.want {
			t.Errorf("%s: isTaskNameConflict = %v, want %v", tc.name, got, tc.want)
		}
	}
}
