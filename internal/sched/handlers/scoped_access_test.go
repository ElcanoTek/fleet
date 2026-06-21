// Copyright (c) 2025 ElcanoTek
// All rights reserved. This is a private repository.

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/google/uuid"
)

func TestScopedAccess(t *testing.T) {
	// Setup test infrastructure
	tmpDir, err := os.MkdirTemp("", "sched-test-scoped-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	store := storage.New()
	if err := store.Initialize(filepath.Join(tmpDir, "test.db")); err != nil {
		if isDatabaseUnavailable(err) {
			t.Skipf("Skipping tests: database unavailable: %v", err)
		}
		t.Fatalf("Failed to initialize storage: %v", err)
	}
	defer store.Close()

	acquireTestLock(t, store)

	keyMgr, err := apikeys.NewManager(
		filepath.Join(tmpDir, "api_keys.json"),
		filepath.Join(tmpDir, "audit_log.jsonl"),
	)
	if err != nil {
		t.Fatalf("Failed to initialize API key manager: %v", err)
	}

	h := New(Config{
		OrchestratorURL:   "http://localhost:8000",
		AdminAPIKey:       "test-admin-key",
		RegistrationToken: "test-reg-token",
		Version:           "0.1.0",
	}, store, keyMgr)

	// Clean up tables
	ctx := context.Background()
	queries := []string{
		"DELETE FROM logs",
		"DELETE FROM tasks",
		"DELETE FROM nodes",
		"DELETE FROM users",
	}
	for _, q := range queries {
		if _, err := store.DB().Conn().ExecContext(ctx, q); err != nil {
			t.Fatalf("Failed to clean up table: %v", err)
		}
	}

	// Create nodes
	nodes := []*models.Node{
		{ID: uuid.New(), Name: "client-a-1", Hostname: "client-a-1", Status: models.NodeStatusIdle},
		{ID: uuid.New(), Name: "client-a-2", Hostname: "client-a-2", Status: models.NodeStatusIdle},
		{ID: uuid.New(), Name: "client-b-1", Hostname: "client-b-1", Status: models.NodeStatusIdle},
	}
	for _, n := range nodes {
		store.AddNode(n)
	}

	// Helper to create request context with user
	createRequest := func(role string, scopes []string) *http.Request {
		req := httptest.NewRequest("GET", "/nodes", nil)
		user := &models.User{
			ID:       uuid.New(),
			Username: "test-user",
			Role:     role,
			Scopes:   scopes,
		}
		ctx := context.WithValue(req.Context(), userContextKey, user)
		return req.WithContext(ctx)
	}

	// Test Cases

	t.Run("Admin with No Scopes -> See All", func(t *testing.T) {
		req := createRequest("admin", []string{})
		w := httptest.NewRecorder()
		h.ListNodes(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", w.Code)
		}
		var resp models.PaginatedResponse
		json.NewDecoder(w.Body).Decode(&resp)
		// Assuming json unmarshals to map for Data
		data, _ := json.Marshal(resp.Data)
		var resultNodes []*models.Node
		json.Unmarshal(data, &resultNodes)

		if len(resultNodes) != 3 {
			t.Errorf("Admin should see all 3 nodes, saw %d", len(resultNodes))
		}
	})

	t.Run("Client with No Scopes -> See None", func(t *testing.T) {
		req := createRequest("client", []string{})
		w := httptest.NewRecorder()
		h.ListNodes(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", w.Code)
		}
		var resp models.PaginatedResponse
		json.NewDecoder(w.Body).Decode(&resp)

		data, _ := json.Marshal(resp.Data)
		var resultNodes []*models.Node
		json.Unmarshal(data, &resultNodes)

		if len(resultNodes) != 0 {
			t.Errorf("Client with no scopes should see 0 nodes, saw %d", len(resultNodes))
		}
	})

	t.Run("Client with Scope client-a-* -> See A nodes", func(t *testing.T) {
		req := createRequest("client", []string{"client-a-*"})
		w := httptest.NewRecorder()
		h.ListNodes(w, req)

		var resp models.PaginatedResponse
		json.NewDecoder(w.Body).Decode(&resp)

		data, _ := json.Marshal(resp.Data)
		var resultNodes []*models.Node
		json.Unmarshal(data, &resultNodes)

		if len(resultNodes) != 2 {
			t.Errorf("Should see 2 nodes, saw %d", len(resultNodes))
		}
		for _, n := range resultNodes {
			if n.Name == "client-b-1" {
				t.Error("Should not see client-b-1")
			}
		}
	})

	t.Run("Client with Multiple Scopes -> See matched", func(t *testing.T) {
		req := createRequest("client", []string{"client-a-1", "client-b-1"})
		w := httptest.NewRecorder()
		h.ListNodes(w, req)

		var resp models.PaginatedResponse
		json.NewDecoder(w.Body).Decode(&resp)

		data, _ := json.Marshal(resp.Data)
		var resultNodes []*models.Node
		json.Unmarshal(data, &resultNodes)

		if len(resultNodes) != 2 {
			t.Errorf("Should see 2 nodes, saw %d", len(resultNodes))
		}
	})
}
