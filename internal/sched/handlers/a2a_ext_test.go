// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// Phase-2 tests for the extended agent card, the contextId rules
// (CORE-MULTI-002a/005/006), and the configurable unary wait budget.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestA2AExtendedAgentCard(t *testing.T) {
	_, keyMgr, mux := setupA2A(t)
	rawKey, _ := mintTaskKey(t, keyMgr, "card-reader")

	// Authenticated caller: the extended card, schema-valid scopes included.
	_, env := rpc(t, mux, rawKey, "GetExtendedAgentCard", map[string]any{})
	if env == nil || env.Error != nil {
		t.Fatalf("extended card: %+v", env)
	}
	var card struct {
		Capabilities struct {
			ExtendedAgentCard bool `json:"extendedAgentCard"`
			Streaming         bool `json:"streaming"`
		} `json:"capabilities"`
		Skills []struct {
			Description string   `json:"description"`
			Examples    []string `json:"examples"`
		} `json:"skills"`
		SecurityRequirements []struct {
			Schemes map[string]struct {
				List *[]string `json:"list"`
			} `json:"schemes"`
		} `json:"securityRequirements"`
	}
	if err := json.Unmarshal(env.Result, &card); err != nil {
		t.Fatal(err)
	}
	if !card.Capabilities.ExtendedAgentCard || !card.Capabilities.Streaming {
		t.Fatalf("extended card capabilities wrong: %s", env.Result)
	}
	if len(card.Skills) == 0 || !strings.Contains(card.Skills[0].Description, `persona "qa-bot"`) {
		t.Fatalf("extended card must surface the pinned persona: %s", env.Result)
	}
	if len(card.Skills[0].Examples) == 0 {
		t.Fatal("extended card should carry skill examples")
	}
	if len(card.SecurityRequirements) != 1 || card.SecurityRequirements[0].Schemes["apiKey"].List == nil {
		t.Fatalf("extended card securityRequirements must keep the schema-valid shape: %s", env.Result)
	}

	// Params absent entirely (the spec's own §9.4.8 example) works too.
	rr, env2 := rpc(t, mux, rawKey, "GetExtendedAgentCard", nil)
	if env2 == nil || env2.Error != nil {
		t.Fatalf("no-params extended card: code=%d %+v", rr.Code, env2)
	}
}

func TestA2AExtendedCardNotConfigured(t *testing.T) {
	// Declared-but-unconfigured (spec §3.1.11): -32007, never MethodNotFound.
	h := New(Config{AdminAPIKey: "admin-key"}, nil, nil)
	h.SetA2A(&A2AConfig{CardJSON: []byte(`{}`), CardETag: `"x"`})
	mux := newA2ATestMux(h)

	_, env := rpc(t, mux, "admin-key", "GetExtendedAgentCard", map[string]any{})
	wantRPCError(t, env, -32007, "EXTENDED_AGENT_CARD_NOT_CONFIGURED")
}

func TestA2AContextIDRules(t *testing.T) {
	store, keyMgr, mux := setupA2A(t)
	rawKey, keyID := mintTaskKey(t, keyMgr, "contexter")

	// CORE-MULTI-002a: a client-provided contextId on a NEW message must be
	// rejected — never silently replaced with a generated one.
	_, env := rpc(t, mux, rawKey, "SendMessage", map[string]any{
		"message": map[string]any{
			"messageId": "m-ctx-1", "role": "ROLE_USER", "contextId": "client-context-1",
			"parts": []map[string]any{{"text": "A perfectly valid prompt."}},
		},
	})
	wantRPCError(t, env, -32602, "INVALID_PARAMS")

	// Seed an INPUT_REQUIRED task owned by the key for the follow-up rules.
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

	// CORE-MULTI-006: taskId + mismatching contextId must error.
	_, env = rpc(t, mux, rawKey, "SendMessage", map[string]any{
		"message": map[string]any{
			"messageId": "m-ctx-2", "role": "ROLE_USER",
			"taskId": paused.ID.String(), "contextId": "wrong-context",
			"parts": []map[string]any{{"text": "the second one"}},
		},
	})
	wantRPCError(t, env, -32602, "INVALID_PARAMS")

	// CORE-MULTI-005 + a MATCHING contextId: the follow-up resumes the task.
	_, env = rpc(t, mux, rawKey, "SendMessage", map[string]any{
		"message": map[string]any{
			"messageId": "m-ctx-3", "role": "ROLE_USER",
			"taskId": paused.ID.String(), "contextId": paused.ID.String(),
			"parts": []map[string]any{{"text": "the second one"}},
		},
		"configuration": map[string]any{"returnImmediately": true},
	})
	if env == nil || env.Error != nil {
		t.Fatalf("matching contextId follow-up must succeed: %+v", env)
	}
	row, err := store.GetTask(paused.ID)
	if err != nil || row.Status != models.TaskStatusPending {
		t.Fatalf("answer must re-queue the task: %v %v", err, row.Status)
	}
}

func TestA2AUnaryWaitBudgetKnob(t *testing.T) {
	store, keyMgr, mux, h := setupA2AHandlers(t, false)
	rawKey, _ := mintTaskKey(t, keyMgr, "waiter")
	_ = store
	h.a2a.UnaryWaitBudget = time.Second

	// A default (blocking) send on a task nothing will ever run: the
	// configured budget answers with the freshest snapshot instead of
	// holding the connection for the 30-minute default.
	start := time.Now()
	_, env := rpc(t, mux, rawKey, "SendMessage", map[string]any{
		"message": userMessage("Nothing will run this in the test.", ""),
	})
	elapsed := time.Since(start)
	if env == nil || env.Error != nil {
		t.Fatalf("send: %+v", env)
	}
	var out struct {
		Task struct {
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"task"`
	}
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Task.Status.State != "TASK_STATE_SUBMITTED" {
		t.Fatalf("budget-expired wait must answer the snapshot, got %s", env.Result)
	}
	if elapsed < time.Second || elapsed > 10*time.Second {
		t.Fatalf("wait should have been ~1s (the configured budget), took %s", elapsed)
	}
}
