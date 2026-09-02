// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// Inbound recursion guard for fleet-to-fleet delegation (#1368): the
// X-Fleet-A2A-Depth header a delegating peer sends is stamped on the created
// task and refused past the ceiling; follow-up answers ignore it.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	a2abridge "github.com/ElcanoTek/fleet/internal/a2a"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func rpcWithDepth(t *testing.T, mux http.Handler, apiKey, depth string, params any) *rpcEnvelope {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": params})
	req := httptest.NewRequest("POST", "/a2a", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	req.Header.Set("X-API-Key", apiKey)
	if depth != "" {
		req.Header.Set(a2abridge.DepthHeader, depth)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var env rpcEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	return &env
}

func createdTaskID(t *testing.T, env *rpcEnvelope) uuid.UUID {
	t.Helper()
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var out struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatal(err)
	}
	id, err := uuid.Parse(out.Task.ID)
	if err != nil {
		t.Fatalf("task id %q: %v", out.Task.ID, err)
	}
	return id
}

func TestA2AInboundDelegationDepth(t *testing.T) {
	store, keyMgr, mux, h := setupA2AHandlers(t, false)
	h.a2a.MaxDelegationDepth = 2
	rawKey, keyID := mintTaskKey(t, keyMgr, "delegator")
	send := func(depth string) *rpcEnvelope {
		return rpcWithDepth(t, mux, rawKey, depth, map[string]any{
			"message":       userMessage("A perfectly valid prompt.", ""),
			"configuration": map[string]any{"returnImmediately": true},
		})
	}

	// No header: an inbound A2A create is one hop.
	row, err := store.GetTask(createdTaskID(t, send("")))
	if err != nil || row.A2ADelegationDepth != 1 {
		t.Fatalf("absent header must stamp depth 1: %v %+v", err, row)
	}
	// A declared depth within the ceiling round-trips onto the row.
	row, err = store.GetTask(createdTaskID(t, send("2")))
	if err != nil || row.A2ADelegationDepth != 2 {
		t.Fatalf("declared depth 2 must be stamped: %v %d", err, row.A2ADelegationDepth)
	}
	// Below one clamps to one (a peer cannot claim to be shallower than a hop).
	row, err = store.GetTask(createdTaskID(t, send("0")))
	if err != nil || row.A2ADelegationDepth != 1 {
		t.Fatalf("depth 0 must clamp to 1: %v %d", err, row.A2ADelegationDepth)
	}
	// Past the ceiling: refused before a task exists, naming the knob.
	env := send("3")
	wantRPCError(t, env, -32600, "INVALID_REQUEST")
	if !strings.Contains(env.Error.Message, "FLEET_A2A_MAX_DELEGATION_DEPTH") {
		t.Errorf("refusal must name the knob: %s", env.Error.Message)
	}
	// Junk is refused too.
	wantRPCError(t, send("deep"), -32600, "INVALID_REQUEST")

	// A follow-up answer ignores the header entirely: it extends no chain.
	paused := &models.Task{
		ID: uuid.New(), Prompt: "awaiting input", Status: models.TaskStatusPausedAwaitingInput,
		CreatedAt: time.Now().UTC(), CreatedByKeyID: &keyID, Timezone: "UTC",
	}
	if _, err := store.AddTask(paused); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Conn().ExecContext(context.Background(),
		"UPDATE tasks SET pending_question = 'which one?' WHERE id = $1", paused.ID); err != nil {
		t.Fatal(err)
	}
	env = rpcWithDepth(t, mux, rawKey, "99", map[string]any{
		"message":       userMessage("the second one", paused.ID.String()),
		"configuration": map[string]any{"returnImmediately": true},
	})
	if env.Error != nil {
		t.Fatalf("follow-up must ignore the depth header: %+v", env.Error)
	}
	row, err = store.GetTask(paused.ID)
	if err != nil || row.A2ADelegationDepth != 0 {
		t.Fatalf("answering must not re-stamp depth: %v %d", err, row.A2ADelegationDepth)
	}
}

func TestA2ADelegationDepthDefaultsWhenUnconfigured(t *testing.T) {
	// A zero ceiling (a test-built config) falls back to the package default
	// instead of refusing every create.
	if d, err := a2aDelegationDepth("", 0); err != nil || d != 1 {
		t.Fatalf("absent header: %d %v", d, err)
	}
	if _, err := a2aDelegationDepth("3", 0); err != nil {
		t.Fatalf("depth 3 must be within the default ceiling: %v", err)
	}
	if _, err := a2aDelegationDepth("4", 0); err == nil {
		t.Fatal("depth 4 must exceed the default ceiling of 3")
	}
}
