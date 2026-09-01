// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// Tests for the task-kicker seam (#1279): writes that make a task immediately
// claimable fire the wired pool wake exactly when they should — a create
// landing in pending kicks, a scheduled-for-later create does not, and a
// follow-up answer that re-queues a paused task kicks again.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestTaskKickerFiresOnClaimableWrites(t *testing.T) {
	store, keyMgr, mux, h := setupA2AHandlers(t, false)
	rawKey, keyID := mintTaskKey(t, keyMgr, "kicker")

	var kicks int32
	h.SetTaskKicker(func() { atomic.AddInt32(&kicks, 1) })

	// An A2A send creates a pending task through createTaskGoverned: one kick,
	// fired before the response is written.
	_, env := rpc(t, mux, rawKey, "SendMessage", map[string]any{
		"message":       userMessage("A perfectly valid prompt.", ""),
		"configuration": map[string]any{"returnImmediately": true},
	})
	if env == nil || env.Error != nil {
		t.Fatalf("send: %+v", env)
	}
	if n := atomic.LoadInt32(&kicks); n != 1 {
		t.Fatalf("a create landing in pending must kick once, got %d", n)
	}

	// A follow-up answer re-queues a paused task: another kick.
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
	_, env = rpc(t, mux, rawKey, "SendMessage", map[string]any{
		"message":       userMessage("the second one", paused.ID.String()),
		"configuration": map[string]any{"returnImmediately": true},
	})
	if env == nil || env.Error != nil {
		t.Fatalf("answer: %+v", env)
	}
	if n := atomic.LoadInt32(&kicks); n != 2 {
		t.Fatalf("an answer that re-queues a paused task must kick, got %d kicks", n)
	}
}

func TestTaskKickerSkipsNonClaimableCreates(t *testing.T) {
	_, _, _, h := setupA2AHandlers(t, false)

	var kicks int32
	h.SetTaskKicker(func() { atomic.AddInt32(&kicks, 1) })

	// A create scheduled for later lands in `scheduled`, not `pending` — the
	// scheduler promotes it when due, so there is nothing for the pool to
	// claim now and the kick must not fire.
	future := time.Now().UTC().Add(time.Hour)
	task, err := h.createTaskGoverned(context.Background(), taskCreator{},
		models.TaskCreate{Prompt: "later", ScheduledFor: &future})
	if err != nil {
		t.Fatalf("scheduled create: %v", err)
	}
	if task.Status != models.TaskStatusScheduled {
		t.Fatalf("fixture broke: expected a scheduled task, got %s", task.Status)
	}
	if n := atomic.LoadInt32(&kicks); n != 0 {
		t.Fatalf("a scheduled-for-later create must not kick, got %d", n)
	}

	// The nil-kicker default stays a no-op (the pre-kick behavior).
	h.SetTaskKicker(nil)
	if _, err := h.createTaskGoverned(context.Background(), taskCreator{},
		models.TaskCreate{Prompt: "now"}); err != nil {
		t.Fatalf("pending create with nil kicker: %v", err)
	}
}
