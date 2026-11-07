// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/sched"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
)

// TestNoteProposalReadsRequireAdmin pins that the note proposal queue is read
// with the same grant that publishes and rejects proposals: PermissionAdmin.
// The routes sit in the authenticate-only AdminOrUserAuthMiddleware group (as
// in cmd/fleet), so the gate is the handler's.
func TestNoteProposalReadsRequireAdmin(t *testing.T) {
	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	defer cleanup()
	h := New(Config{DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, keyMgr)
	notesStore := sched.NewStore(store.DB())
	nh := NewNotesHandlers(notesStore, h)
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Get("/notes/proposals", nh.ListProposals)
		r.Get("/notes/proposals/{id}", nh.GetProposal)
	})

	prop, err := notesStore.CreateProposal(context.Background(), "authz-proposal", "Title", "body", "why", "task-1")
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	paths := []string{"/notes/proposals?status=pending", "/notes/proposals/" + prop.ID.String()}

	addLogUser(t, store, "client-user", "client", "client-token")
	addLogUser(t, store, "admin-user", "admin", "admin-token")
	_, readonlyKey, err := keyMgr.CreateTypedKey("viewer", apikeys.KeyTypeReadonly, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateTypedKey readonly: %v", err)
	}
	_, typedAdminKey, err := keyMgr.CreateTypedKey("root", apikeys.KeyTypeAdmin, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateTypedKey admin: %v", err)
	}

	cases := []struct {
		name, header, value string
		want                int
	}{
		{"client user", "Authorization", "Bearer client-token", http.StatusForbidden},
		{"readonly key", "X-API-Key", readonlyKey, http.StatusForbidden},
		{"admin user", "Authorization", "Bearer admin-token", http.StatusOK},
		{"typed admin key", "X-API-Key", typedAdminKey, http.StatusOK},
		{"admin API key", "X-API-Key", "test-admin-key", http.StatusOK},
	}
	for _, tc := range cases {
		for _, path := range paths {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set(tc.header, tc.value)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("%s GET %s = %d, want %d: %s", tc.name, path, w.Code, tc.want, w.Body.String())
			}
		}
	}
}
