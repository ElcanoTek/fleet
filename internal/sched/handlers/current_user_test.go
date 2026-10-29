// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestGetCurrentUser(t *testing.T) {
	h := &Handlers{}

	t.Run("user in context -> identity returned", func(t *testing.T) {
		user := &models.User{Username: "alice@example.com", Role: "client"}
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, user))
		rr := httptest.NewRecorder()
		h.GetCurrentUser(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body["authenticated"] != true || body["username"] != "alice@example.com" || body["role"] != "client" {
			t.Errorf("body = %+v, want authenticated alice/client", body)
		}
	})

	t.Run("no user (API key) -> authenticated without identity", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		rr := httptest.NewRecorder()
		h.GetCurrentUser(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		var body map[string]interface{}
		json.NewDecoder(rr.Body).Decode(&body)
		if body["authenticated"] != true {
			t.Errorf("expected authenticated:true, got %+v", body)
		}
		if _, hasUsername := body["username"]; hasUsername {
			t.Errorf("API-key session should not include a username, got %+v", body)
		}
	})
}
