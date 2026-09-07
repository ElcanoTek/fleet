// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// POST /keys/{id}/rotate's ?grace_period_hours= keeps the OLD key valid after
// rotation. It used to accept any magnitude (87600 = a ten-year "rotation")
// and silently fall back to the default on a typo; it is now bounded to
// [0, 168] with a 400 naming the range, like GetAuditLog's ?hours=.
func TestRotateAPIKeyGracePeriodBounds(t *testing.T) {
	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	t.Cleanup(cleanup)
	h := New(Config{DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, keyMgr)
	mux := chi.NewRouter()
	mux.Group(func(rt chi.Router) {
		rt.Use(h.AdminAuthMiddleware)
		rt.Post("/keys/{key_id}/rotate", h.RotateAPIKey)
	})
	rotate := func(keyID, query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/keys/"+keyID+"/rotate"+query, nil)
		req.Header.Set("X-API-Key", "test-admin-key")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	for _, query := range []string{"?grace_period_hours=abc", "?grace_period_hours=-1", "?grace_period_hours=169", "?grace_period_hours=87600"} {
		keyID, _ := mustCreateRoleKeyWithID(t, keyMgr, "client")
		w := rotate(keyID, query)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "0-168") {
			t.Errorf("rotate%s = %d %q, want 400 naming the 0-168 range", query, w.Code, w.Body.String())
		}
		if got := keyMgr.GetKey(keyID); got == nil || got.RotatedAt != nil {
			t.Errorf("rotate%s: a refused request must not rotate the key (%+v)", query, got)
		}
	}

	for _, tc := range []struct {
		query string
		want  int
	}{{"", 24}, {"?grace_period_hours=0", 0}, {"?grace_period_hours=168", 168}} {
		keyID, _ := mustCreateRoleKeyWithID(t, keyMgr, "client")
		w := rotate(keyID, tc.query)
		if w.Code != http.StatusOK {
			t.Fatalf("rotate%s = %d, want 200: %s", tc.query, w.Code, w.Body.String())
		}
		var got models.APIKeyRotated
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.GracePeriodHours != tc.want {
			t.Errorf("rotate%s grace = %d, want %d", tc.query, got.GracePeriodHours, tc.want)
		}
	}
}
