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

	"github.com/ElcanoTek/fleet/internal/sched"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
)

// TestNoteProposalReadsRequireAdmin pins that the note proposal queue is read
// with the same grant that publishes and rejects proposals: PermissionAdmin.
// The routes sit in the authenticate-only AdminOrUserAuthMiddleware group (as
// in cmd/fleet), so the gate is the handler's.
func TestNoteProposalReadsRequireAdmin(t *testing.T) {
	t.Parallel()

	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	defer cleanup()
	h := New(Config{DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, keyMgr)
	notesStore := sched.NewStore(store.DB())
	nh := NewNotesHandlers(notesStore, h)
	r := chi.NewRouter()
	r.Use(asSeededSession)
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

// TestNoteActorIgnoresASpoofedUserHeader: created_by comes from the
// authenticated caller, never from a client-supplied X-User-Email. Before the
// fix an admin-key or bearer caller could attribute a note to anyone.
func TestNoteActorIgnoresASpoofedUserHeader(t *testing.T) {
	t.Parallel()

	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	defer cleanup()
	h := New(Config{DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, keyMgr)
	nh := NewNotesHandlers(sched.NewStore(store.DB()), h)
	r := chi.NewRouter()
	r.Use(asSeededSession)
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Post("/notes", nh.CreateNote)
	})
	addLogUser(t, store, "admin-user", "admin", "admin-token")

	for _, tc := range []struct {
		name, header, value, slug, want string
	}{
		{"admin key", "X-API-Key", "test-admin-key", "spoof-admin-key", "admin"},
		{"bearer user", "Authorization", "Bearer admin-token", "spoof-bearer", "admin-user"},
	} {
		body := strings.NewReader(`{"slug":"` + tc.slug + `","title":"T","body":"b"}`)
		req := httptest.NewRequest(http.MethodPost, "/notes", body)
		req.Header.Set(tc.header, tc.value)
		req.Header.Set("X-User-Email", "victim@example.com")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("%s: create note: %d %s", tc.name, rec.Code, rec.Body.String())
		}
		var note sched.Note
		if err := json.Unmarshal(rec.Body.Bytes(), &note); err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		if note.CreatedBy != tc.want {
			t.Errorf("%s: created_by = %q, want %q (the spoofed X-User-Email must be ignored)", tc.name, note.CreatedBy, tc.want)
		}
	}
}
