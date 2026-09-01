// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// A2A push-notification tests (#1279 Phase 2): the CRUD contract over the
// real routes/storage (client-id round-trip, BOTH param spellings, terminal-
// task creation, idempotent delete, creator scoping), inline registration on
// SendMessage, and end-to-end delivery through the poll dispatcher — exact
// Authorization string, both token-header spellings, the spec media type,
// and the SSRF guard refusing a loopback receiver unless relaxed.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	schedpush "github.com/ElcanoTek/fleet/internal/sched/push"
)

func TestA2APushConfigCRUD(t *testing.T) {
	store, keyMgr, mux := setupA2AWith(t, true)
	rawKey, keyID := mintTaskKey(t, keyMgr, "pusher")
	rawOther, _ := mintTaskKey(t, keyMgr, "outsider")

	// Configs must be creatable on an already-terminal task (the TCK
	// registers on completed tasks) — seed one owned by the key.
	task := &models.Task{
		ID: uuid.New(), Prompt: "done already", Status: models.TaskStatusSuccess,
		CreatedAt: time.Now().UTC(), CreatedByKeyID: &keyID, Timezone: "UTC",
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatal(err)
	}

	// Create with the TCK's snake_case task_id and a client-chosen id.
	_, env := rpc(t, mux, rawKey, "CreateTaskPushNotificationConfig", map[string]any{
		"task_id": task.ID.String(), "id": "tck-style-id",
		"url":            "https://example.com/hook",
		"token":          "not-a-real-token-value",
		"authentication": map[string]any{"scheme": "Bearer", "credentials": "not-a-real-credential"},
	})
	if env == nil || env.Error != nil {
		t.Fatalf("create: %+v", env)
	}
	var created struct {
		ID     string `json:"id"`
		TaskID string `json:"taskId"`
		URL    string `json:"url"`
		Token  string `json:"token"`
		Auth   *struct {
			Scheme string `json:"scheme"`
		} `json:"authentication"`
	}
	if err := json.Unmarshal(env.Result, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != "tck-style-id" || created.TaskID != task.ID.String() || created.Auth == nil || created.Auth.Scheme != "Bearer" {
		t.Fatalf("client id/shape must round-trip: %s", env.Result)
	}

	// Get with camelCase taskId this time — both spellings must work.
	_, env = rpc(t, mux, rawKey, "GetTaskPushNotificationConfig", map[string]any{
		"taskId": task.ID.String(), "id": "tck-style-id",
	})
	if env == nil || env.Error != nil {
		t.Fatalf("get: %+v", env)
	}

	// Unknown config id → an error (the TCK accepts any; ours is -32001-class).
	_, env = rpc(t, mux, rawKey, "GetTaskPushNotificationConfig", map[string]any{
		"task_id": task.ID.String(), "id": "no-such-config",
	})
	wantRPCError(t, env, -32001, "TASK_NOT_FOUND")

	// List: configs never null, nextPageToken always present-and-empty.
	_, env = rpc(t, mux, rawKey, "ListTaskPushNotificationConfigs", map[string]any{
		"task_id": task.ID.String(), "page_size": 7,
	})
	var listed struct {
		Configs       []json.RawMessage `json:"configs"`
		NextPageToken *string           `json:"nextPageToken"`
	}
	if err := json.Unmarshal(env.Result, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Configs) != 1 || listed.NextPageToken == nil || *listed.NextPageToken != "" {
		t.Fatalf("list shape wrong: %s", env.Result)
	}

	// Another key cannot even see the task's configs (spec §3.3.2 / ADR-0043).
	_, env = rpc(t, mux, rawOther, "ListTaskPushNotificationConfigs", map[string]any{"task_id": task.ID.String()})
	wantRPCError(t, env, -32001, "TASK_NOT_FOUND")

	// Delete twice: both succeed (spec §3.1.10), result is the Empty object.
	for i := 0; i < 2; i++ {
		_, env = rpc(t, mux, rawKey, "DeleteTaskPushNotificationConfig", map[string]any{
			"task_id": task.ID.String(), "id": "tck-style-id",
		})
		if env == nil || env.Error != nil {
			t.Fatalf("delete #%d: %+v", i+1, env)
		}
	}
	_, env = rpc(t, mux, rawKey, "GetTaskPushNotificationConfig", map[string]any{
		"task_id": task.ID.String(), "id": "tck-style-id",
	})
	wantRPCError(t, env, -32001, "TASK_NOT_FOUND")

	// A bad URL is refused at create time.
	_, env = rpc(t, mux, rawKey, "CreateTaskPushNotificationConfig", map[string]any{
		"task_id": task.ID.String(), "url": "ftp://example.com/x",
	})
	wantRPCError(t, env, -32602, "INVALID_PARAMS")
}

func TestA2APushInlineConfigOnSendMessage(t *testing.T) {
	store, keyMgr, mux := setupA2AWith(t, true)
	rawKey, _ := mintTaskKey(t, keyMgr, "inliner")

	_, env := rpc(t, mux, rawKey, "SendMessage", map[string]any{
		"message": userMessage("Create with an inline push config.", ""),
		"configuration": map[string]any{
			"returnImmediately": true,
			"taskPushNotificationConfig": map[string]any{
				"id": "inline-1", "url": "https://example.com/inline-hook", "token": "not-a-real-token-value",
			},
		},
	})
	if env == nil || env.Error != nil {
		t.Fatalf("send: %+v", env)
	}
	var created struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(env.Result, &created); err != nil {
		t.Fatal(err)
	}
	configs, err := store.ListA2APushConfigs(t.Context(), uuid.MustParse(created.Task.ID))
	if err != nil || len(configs) != 1 || configs[0].ID != "inline-1" || configs[0].Token != "not-a-real-token-value" {
		t.Fatalf("inline config must bind to the created task: %v %+v", err, configs)
	}
}

// pushReceiver is a tiny webhook target recording every delivery.
type pushReceiver struct {
	mu   sync.Mutex
	hits []pushHit
}

type pushHit struct {
	header http.Header
	body   []byte
}

func (r *pushReceiver) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.hits = append(r.hits, pushHit{header: req.Header.Clone(), body: body})
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

func (r *pushReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hits)
}

func (r *pushReceiver) hit(i int) pushHit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[i]
}

// waitFor polls cond until true or a 5s deadline (generous multiples of the
// dispatcher's 1s tick).
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
}

func TestA2APushDeliveryLifecycle(t *testing.T) {
	store, keyMgr, mux := setupA2AWith(t, true)
	rawKey, keyID := mintTaskKey(t, keyMgr, "deliveree")

	recv := &pushReceiver{}
	srv := httptest.NewServer(recv.handler())
	defer srv.Close()

	task := &models.Task{
		ID: uuid.New(), Prompt: "watch me via webhook", Status: models.TaskStatusPending,
		CreatedAt: time.Now().UTC(), CreatedByKeyID: &keyID, Timezone: "UTC",
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatal(err)
	}
	_, env := rpc(t, mux, rawKey, "CreateTaskPushNotificationConfig", map[string]any{
		"task_id": task.ID.String(), "id": "d1", "url": srv.URL,
		"token":          "not-a-real-token-value",
		"authentication": map[string]any{"scheme": "Bearer", "credentials": "not-a-real-credential"},
	})
	if env == nil || env.Error != nil {
		t.Fatalf("create config: %+v", env)
	}

	// allowPrivate: the httptest receiver is loopback — the exact posture the
	// FLEET_A2A_PUSH_ALLOW_PRIVATE escape hatch exists for.
	dispatcher := schedpush.New(store, true)
	ctx, cancel := t.Context(), func() {}
	_ = cancel
	go dispatcher.Run(ctx)

	// The fresh registration announces the CURRENT state first…
	if !waitFor(t, func() bool { return recv.count() >= 1 }) {
		t.Fatal("no delivery for the initial state")
	}
	first := recv.hit(0)
	if got := first.header.Get("Authorization"); got != "Bearer not-a-real-credential" {
		t.Fatalf("Authorization must be the verbatim scheme+credentials, got %q", got)
	}
	if first.header.Get("X-A2A-Notification-Token") != "not-a-real-token-value" ||
		first.header.Get("A2A-Notification-Token") != "not-a-real-token-value" {
		t.Fatalf("both notification-token spellings must carry the token: %+v", first.header)
	}
	if ct := first.header.Get("Content-Type"); ct != "application/a2a+json" {
		t.Fatalf("Content-Type = %q, want application/a2a+json", ct)
	}
	var frame struct {
		StatusUpdate *struct {
			TaskID string `json:"taskId"`
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"statusUpdate"`
	}
	if err := json.Unmarshal(first.body, &frame); err != nil || frame.StatusUpdate == nil {
		t.Fatalf("payload must be a StreamResponse statusUpdate: %s (%v)", first.body, err)
	}
	if frame.StatusUpdate.TaskID != task.ID.String() || frame.StatusUpdate.Status.State != "TASK_STATE_SUBMITTED" {
		t.Fatalf("first delivery wrong: %s", first.body)
	}

	// …then each transition pushes again.
	if _, err := store.UpdateTasksStatusBatch([]uuid.UUID{task.ID}, models.TaskStatusPending, models.TaskStatusRunning); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, func() bool { return recv.count() >= 2 }) {
		t.Fatal("no delivery for pending→running")
	}
	if _, err := store.UpdateTasksStatusBatch([]uuid.UUID{task.ID}, models.TaskStatusRunning, models.TaskStatusSuccess); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, func() bool { return recv.count() >= 3 }) {
		t.Fatal("no delivery for running→success")
	}
	last := recv.hit(recv.count() - 1)
	if err := json.Unmarshal(last.body, &frame); err != nil || frame.StatusUpdate == nil ||
		frame.StatusUpdate.Status.State != "TASK_STATE_COMPLETED" {
		t.Fatalf("terminal delivery wrong: %s", last.body)
	}
}

func TestA2APushSSRFGuardBlocksLoopback(t *testing.T) {
	store, keyMgr, mux := setupA2AWith(t, true)
	rawKey, keyID := mintTaskKey(t, keyMgr, "guarded")

	recv := &pushReceiver{}
	srv := httptest.NewServer(recv.handler())
	defer srv.Close()

	task := &models.Task{
		ID: uuid.New(), Prompt: "must not reach loopback", Status: models.TaskStatusPending,
		CreatedAt: time.Now().UTC(), CreatedByKeyID: &keyID, Timezone: "UTC",
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatal(err)
	}
	if _, env := rpc(t, mux, rawKey, "CreateTaskPushNotificationConfig", map[string]any{
		"task_id": task.ID.String(), "id": "g1", "url": srv.URL,
	}); env == nil || env.Error != nil {
		t.Fatalf("create config: %+v", env)
	}

	// Default posture: the guard refuses the loopback dial; the attempt is
	// still MARKED (at-least-once ATTEMPT is the contract), so the work list
	// drains without a byte reaching the receiver.
	dispatcher := schedpush.New(store, false)
	go dispatcher.Run(t.Context())

	drained := waitFor(t, func() bool {
		work, err := store.ListA2APushWork(t.Context(), 10)
		return err == nil && len(work) == 0
	})
	if !drained {
		t.Fatal("work list should drain via marked attempts")
	}
	if recv.count() != 0 {
		t.Fatalf("SSRF guard must block loopback delivery, receiver saw %d hits", recv.count())
	}
}
