// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// Regression tests for #1081: a typed admin key (fleet_admin_…) must work on
// the AdminAuthMiddleware routes (/keys, /users, …) — the type is minted with
// PermissionAdmin for exactly those routes, and rejecting it there left a key
// type that could not do the thing its name claims. The gate stays type-based:
// every other key class (task/readonly/webhook/legacy) is a definitive 403,
// the bootstrap ADMIN_API_KEY keeps working, and unknown/absent keys stay 401.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// setupAdminAuthz wires representative admin-only routes behind the real
// AdminAuthMiddleware so each credential class travels the whole path.
func setupAdminAuthz(t *testing.T) (*storage.Storage, *apikeys.Manager, *chi.Mux) {
	t.Helper()
	tmpDir := t.TempDir()

	store := storage.New()
	if err := store.Initialize(filepath.Join(tmpDir, "test.db"), storage.DefaultPoolConfig()); err != nil {
		if isDatabaseUnavailable(err) {
			t.Skipf("Skipping tests: database unavailable: %v", err)
		}
		t.Fatalf("init storage: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	acquireTestLock(t, store)
	if err := cleanDB(store); err != nil {
		t.Fatalf("clean db: %v", err)
	}

	keyMgr, err := apikeys.NewManager(filepath.Join(tmpDir, "keys.json"), filepath.Join(tmpDir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("key mgr: %v", err)
	}

	h := New(Config{
		DefaultTaskModel: "test/model",
		AdminAPIKey:      "bootstrap-admin-key",
		DataDir:          tmpDir,
	}, store, keyMgr)

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(h.AdminAuthMiddleware)
		r.Get("/keys", h.ListAPIKeys)
		r.Post("/keys/{key_id}/rotate", h.RotateAPIKey)
		r.Post("/users", h.CreateUser)
	})
	return store, keyMgr, r
}

func adminAuthzDo(r *chi.Mux, method, path, key string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestAdminAuth_TypedAdminKeyWorks pins the #1081 fix: a fleet_admin_ key can
// list keys, rotate a key, and create a user — the routes its type exists for.
func TestAdminAuth_TypedAdminKeyWorks(t *testing.T) {
	_, keyMgr, r := setupAdminAuthz(t)

	adminKey := mustCreateTypedKey(t, keyMgr, apikeys.KeyTypeAdmin, nil)
	victim, _, err := keyMgr.CreateTypedKey("rotate-me", apikeys.KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create rotate-target key: %v", err)
	}

	if w := adminAuthzDo(r, "GET", "/keys", adminKey, nil); w.Code != http.StatusOK {
		t.Errorf("typed admin GET /keys = %d, want 200: %s", w.Code, w.Body.String())
	}
	if w := adminAuthzDo(r, "POST", "/keys/"+victim.KeyID+"/rotate", adminKey, nil); w.Code != http.StatusOK {
		t.Errorf("typed admin rotate = %d, want 200: %s", w.Code, w.Body.String())
	}
	userBody := []byte(`{"username": "typed-admin-made-me", "password": "a-long-enough-password", "role": "client"}`)
	if w := adminAuthzDo(r, "POST", "/users", adminKey, userBody); w.Code != http.StatusCreated {
		t.Errorf("typed admin POST /users = %d, want 201: %s", w.Code, w.Body.String())
	}
}

// TestAdminAuth_BootstrapKeyStillWorks pins that the env-configured
// ADMIN_API_KEY keeps working unchanged alongside typed admin keys.
func TestAdminAuth_BootstrapKeyStillWorks(t *testing.T) {
	_, _, r := setupAdminAuthz(t)

	if w := adminAuthzDo(r, "GET", "/keys", "bootstrap-admin-key", nil); w.Code != http.StatusOK {
		t.Errorf("bootstrap GET /keys = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestAdminAuth_NonAdminKeysAreDefinitive403 pins that the gate does not widen:
// a VALID key of any non-admin class — task, readonly, webhook, and a legacy
// sk- key even when its role carries PermissionAdmin — is a definitive 403.
// The type segment is what gets hashed, so a non-admin key can never present
// as admin; the legacy case pins that the gate is type-based, not
// permission-based.
func TestAdminAuth_NonAdminKeysAreDefinitive403(t *testing.T) {
	_, keyMgr, r := setupAdminAuthz(t)

	adminRole := "admin"
	_, legacyAdminKey, err := keyMgr.CreateKey("legacy-admin", nil, &adminRole, 0, nil, "")
	if err != nil {
		t.Fatalf("create legacy admin-role key: %v", err)
	}

	for _, tc := range []struct {
		name string
		key  string
	}{
		{"task", mustCreateTypedKey(t, keyMgr, apikeys.KeyTypeTask, nil)},
		{"readonly", mustCreateTypedKey(t, keyMgr, apikeys.KeyTypeReadonly, nil)},
		{"webhook", mustCreateTypedKey(t, keyMgr, apikeys.KeyTypeWebhook, []string{"x"})},
		{"legacy-admin-role", legacyAdminKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := adminAuthzDo(r, "GET", "/keys", tc.key, nil); w.Code != http.StatusForbidden {
				t.Errorf("%s GET /keys = %d, want 403: %s", tc.name, w.Code, w.Body.String())
			}
			userBody := []byte(`{"username": "should-not-exist", "password": "a-long-enough-password", "role": "client"}`)
			if w := adminAuthzDo(r, "POST", "/users", tc.key, userBody); w.Code != http.StatusForbidden {
				t.Errorf("%s POST /users = %d, want 403: %s", tc.name, w.Code, w.Body.String())
			}
		})
	}
}

// TestAdminAuth_InvalidOrAbsentKeysStay401 pins the credential-failure tier:
// no key, an unknown key, and a REVOKED typed admin key are all 401 — a
// revoked admin key must lose these routes immediately.
func TestAdminAuth_InvalidOrAbsentKeysStay401(t *testing.T) {
	_, keyMgr, r := setupAdminAuthz(t)

	revoked, revokedRaw, err := keyMgr.CreateTypedKey("revoked-admin", apikeys.KeyTypeAdmin, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create typed admin key: %v", err)
	}
	if err := keyMgr.RevokeKey(revoked.KeyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	for _, tc := range []struct {
		name string
		key  string
	}{
		{"absent", ""},
		{"unknown", "fleet_admin_1111111111111111111111111111"},
		{"revoked-admin", revokedRaw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := adminAuthzDo(r, "GET", "/keys", tc.key, nil); w.Code != http.StatusUnauthorized {
				t.Errorf("%s GET /keys = %d, want 401: %s", tc.name, w.Code, w.Body.String())
			}
		})
	}
}
