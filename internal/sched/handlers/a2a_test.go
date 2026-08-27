// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// A2A dispatcher tests (#1279): the JSON-RPC contract over the REAL routes,
// storage, and key manager — golden-shape assertions on envelopes, the
// ADR-0043 creator scoping (an invisible task answers -32001 TaskNotFound,
// never 403), the capability gating the Agent Card promises (-32003 push,
// -32004 extended card), version/auth gating, and the streaming lifecycle
// (snapshot first, statusUpdate on transition, close at terminal).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	a2abridge "github.com/ElcanoTek/fleet/internal/a2a"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

func setupA2A(t *testing.T) (*storage.Storage, *apikeys.Manager, *chi.Mux) {
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
		AdminAPIKey:      "admin-key",
		DataDir:          tmpDir,
	}, store, keyMgr)
	card, cardETag, err := a2abridge.MarshalCard(a2abridge.BuildCard(a2abridge.CardSpec{
		Name: "Test Fleet", Version: "0.0.0-test", RPCURL: "/v1/a2a",
	}))
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	h.SetA2A(&A2AConfig{
		CardJSON: card,
		CardETag: cardETag,
		Persona:  "qa-bot",
	})

	r := chi.NewRouter()
	r.Get("/.well-known/agent-card.json", h.A2AAgentCard)
	r.Post("/a2a", h.A2ARPC)
	return store, keyMgr, r
}

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int              `json:"code"`
		Message string           `json:"message"`
		Data    []map[string]any `json:"data"`
	} `json:"error"`
}

// rpc posts one JSON-RPC call with the given key and returns the recorder plus
// the decoded envelope (nil envelope for non-200 / non-JSON responses).
func rpc(t *testing.T, mux http.Handler, apiKey, method string, params any) (*httptest.ResponseRecorder, *rpcEnvelope) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/a2a", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	var env rpcEnvelope
	if rr.Code == http.StatusOK && json.Unmarshal(rr.Body.Bytes(), &env) == nil {
		return rr, &env
	}
	return rr, nil
}

func wantRPCError(t *testing.T, env *rpcEnvelope, code int, reason string) {
	t.Helper()
	if env == nil || env.Error == nil {
		t.Fatalf("want JSON-RPC error %d, got %+v", code, env)
	}
	if env.Error.Code != code {
		t.Fatalf("error code = %d (%s), want %d", env.Error.Code, env.Error.Message, code)
	}
	if len(env.Error.Data) == 0 || env.Error.Data[0]["@type"] != "type.googleapis.com/google.rpc.ErrorInfo" {
		t.Errorf("error.data must carry a google.rpc.ErrorInfo detail, got %+v", env.Error.Data)
	} else if reason != "" && env.Error.Data[0]["reason"] != reason {
		t.Errorf("ErrorInfo reason = %v, want %s", env.Error.Data[0]["reason"], reason)
	}
}

func userMessage(text string, taskID string) map[string]any {
	msg := map[string]any{
		"messageId": "m-" + uuid.NewString(),
		"role":      "ROLE_USER",
		"parts":     []map[string]any{{"text": text}},
	}
	if taskID != "" {
		msg["taskId"] = taskID
	}
	return msg
}

func mintTaskKey(t *testing.T, keyMgr *apikeys.Manager, name string) (string, string) {
	t.Helper()
	key, raw, err := keyMgr.CreateTypedKey(name, apikeys.KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return raw, key.KeyID
}

func TestA2ADisabledAnswers501(t *testing.T) {
	h := New(Config{}, nil, nil)
	for _, route := range []struct{ method, path string }{
		{"GET", "/.well-known/agent-card.json"},
		{"POST", "/a2a"},
	} {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
		rr := httptest.NewRecorder()
		switch route.path {
		case "/a2a":
			h.A2ARPC(rr, req)
		default:
			h.A2AAgentCard(rr, req)
		}
		if rr.Code != http.StatusNotImplemented || !strings.Contains(rr.Body.String(), "a2a_disabled") {
			t.Errorf("%s %s with A2A unwired: code=%d body=%q, want 501 a2a_disabled", route.method, route.path, rr.Code, rr.Body.String())
		}
	}
}

func TestA2AAgentCardServing(t *testing.T) {
	_, _, mux := setupA2A(t)

	req := httptest.NewRequest("GET", "/.well-known/agent-card.json", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Header().Get("ETag") == "" {
		t.Fatalf("card: code=%d etag=%q", rr.Code, rr.Header().Get("ETag"))
	}
	var card map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &card); err != nil || card["name"] != "Test Fleet" {
		t.Fatalf("card body: %v %q", err, rr.Body.String())
	}

	// Conditional refetch: the ETag round-trips as a 304.
	req = httptest.NewRequest("GET", "/.well-known/agent-card.json", nil)
	req.Header.Set("If-None-Match", rr.Header().Get("ETag"))
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, req)
	if rr2.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: code=%d, want 304", rr2.Code)
	}
}

func TestA2AVersionAndAuthGating(t *testing.T) {
	_, keyMgr, mux := setupA2A(t)
	rawKey, _ := mintTaskKey(t, keyMgr, "gating")

	// Missing A2A-Version → spec-literal -32009 (absent means 0.3).
	body := `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}`
	req := httptest.NewRequest("POST", "/a2a", strings.NewReader(body))
	req.Header.Set("X-API-Key", rawKey)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	var env rpcEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("no envelope: %q", rr.Body.String())
	}
	wantRPCError(t, &env, -32009, "VERSION_NOT_SUPPORTED")

	// No credential → transport-layer 401, not a JSON-RPC envelope.
	if rr, env := rpc(t, mux, "", "GetTask", map[string]any{"id": "x"}); rr.Code != http.StatusUnauthorized || env != nil {
		t.Errorf("unauthenticated: code=%d body=%q, want 401", rr.Code, rr.Body.String())
	}
	// A malformed key is indistinguishable from no key.
	if rr, _ := rpc(t, mux, "fleet_task_not_a_real_key_at_all_00", "GetTask", map[string]any{"id": "x"}); rr.Code != http.StatusUnauthorized {
		t.Errorf("bad key: code=%d, want 401", rr.Code)
	}
}

func TestA2AMethodGating(t *testing.T) {
	_, keyMgr, mux := setupA2A(t)
	rawKey, _ := mintTaskKey(t, keyMgr, "methods")

	_, env := rpc(t, mux, rawKey, "NoSuchMethod", map[string]any{})
	wantRPCError(t, env, -32601, "METHOD_NOT_FOUND")

	// Declared-off capabilities answer their SPECIFIC errors (spec §3.3.4),
	// never MethodNotFound and never HTTP 501.
	for _, m := range []string{
		"CreateTaskPushNotificationConfig", "GetTaskPushNotificationConfig",
		"ListTaskPushNotificationConfigs", "DeleteTaskPushNotificationConfig",
	} {
		_, env := rpc(t, mux, rawKey, m, map[string]any{})
		wantRPCError(t, env, -32003, "PUSH_NOTIFICATION_NOT_SUPPORTED")
	}
	_, env = rpc(t, mux, rawKey, "GetExtendedAgentCard", map[string]any{})
	wantRPCError(t, env, -32004, "UNSUPPORTED_OPERATION")
}

func TestA2ASendMessageCreatesGovernedTask(t *testing.T) {
	store, keyMgr, mux := setupA2A(t)
	rawKey, keyID := mintTaskKey(t, keyMgr, "creator")

	_, env := rpc(t, mux, rawKey, "SendMessage", map[string]any{
		"message": userMessage("Summarize the Q3 numbers and attach a report.", ""),
	})
	if env == nil || env.Error != nil {
		t.Fatalf("SendMessage failed: %+v", env)
	}
	// The unary result is the SendMessageResponse oneof: {"task": {...}}.
	var result struct {
		Task *struct {
			ID        string `json:"id"`
			ContextID string `json:"contextId"`
			Status    struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"task"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil || result.Task == nil {
		t.Fatalf("result is not the task oneof: %s", env.Result)
	}
	if result.Task.Status.State != "TASK_STATE_SUBMITTED" || result.Task.ContextID != result.Task.ID {
		t.Errorf("task shape wrong: %+v", result.Task)
	}

	// The row went through the governed pipeline with real attribution and the
	// operator-pinned persona — never caller configuration.
	row, err := store.GetTask(uuid.MustParse(result.Task.ID))
	if err != nil || row == nil {
		t.Fatalf("created task not in storage: %v", err)
	}
	if row.CreatedByKeyID == nil || *row.CreatedByKeyID != keyID {
		t.Errorf("CreatedByKeyID = %v, want %s (ADR-0043 attribution)", row.CreatedByKeyID, keyID)
	}
	if row.Persona != "qa-bot" {
		t.Errorf("Persona = %q, want the operator-pinned qa-bot", row.Persona)
	}

	// Validation runs — the same validateTaskCreate as POST /tasks.
	_, env = rpc(t, mux, rawKey, "SendMessage", map[string]any{"message": userMessage("hi", "")})
	wantRPCError(t, env, -32602, "INVALID_PARAMS")

	// Non-text parts are refused with the content-type error, honest against
	// the card's defaultInputModes.
	_, env = rpc(t, mux, rawKey, "SendMessage", map[string]any{
		"message": map[string]any{
			"messageId": "m-file", "role": "ROLE_USER",
			"parts": []map[string]any{{"url": "https://example.com/doc.pdf", "mediaType": "application/pdf"}},
		},
	})
	wantRPCError(t, env, -32005, "CONTENT_TYPE_NOT_SUPPORTED")

	// This deployment declares no tenant.
	_, env = rpc(t, mux, rawKey, "SendMessage", map[string]any{
		"tenant": "t1", "message": userMessage("A perfectly valid prompt.", ""),
	})
	wantRPCError(t, env, -32602, "INVALID_PARAMS")

	// A push-notification config on the send is the capability error, not a
	// silently dropped field.
	_, env = rpc(t, mux, rawKey, "SendMessage", map[string]any{
		"message":       userMessage("A perfectly valid prompt.", ""),
		"configuration": map[string]any{"taskPushNotificationConfig": map[string]any{"url": "https://example.com/hook"}},
	})
	wantRPCError(t, env, -32003, "PUSH_NOTIFICATION_NOT_SUPPORTED")

	// A readonly key cannot create: definitive transport-layer 403.
	_, readonlyRaw, err := keyMgr.CreateTypedKey("ro", apikeys.KeyTypeReadonly, nil, 0, nil, "")
	_ = readonlyRaw
	if err != nil {
		t.Fatal(err)
	}
	roKey, roRaw, err := keyMgr.CreateTypedKey("ro2", apikeys.KeyTypeReadonly, nil, 0, nil, "")
	_ = roKey
	if err != nil {
		t.Fatal(err)
	}
	if rr, _ := rpc(t, mux, roRaw, "SendMessage", map[string]any{"message": userMessage("A perfectly valid prompt.", "")}); rr.Code != http.StatusForbidden {
		t.Errorf("readonly create: code=%d, want 403", rr.Code)
	}
}

func TestA2AReadsAreCreatorScoped(t *testing.T) {
	store, keyMgr, mux := setupA2A(t)
	rawA, keyA := mintTaskKey(t, keyMgr, "alice")
	rawB, _ := mintTaskKey(t, keyMgr, "bob")

	// Key A's task, seeded through the wire.
	_, env := rpc(t, mux, rawA, "SendMessage", map[string]any{"message": userMessage("Task belonging to key A.", "")})
	if env == nil || env.Error != nil {
		t.Fatalf("create: %+v", env)
	}
	var created struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(env.Result, &created); err != nil {
		t.Fatal(err)
	}
	taskID := created.Task.ID

	// The creator reads it back.
	_, env = rpc(t, mux, rawA, "GetTask", map[string]any{"id": taskID})
	if env == nil || env.Error != nil {
		t.Fatalf("creator GetTask: %+v", env)
	}

	// Key B gets TaskNotFound — not 403 — so existence never leaks (A2A
	// §3.3.2 = ADR-0043).
	_, env = rpc(t, mux, rawB, "GetTask", map[string]any{"id": taskID})
	wantRPCError(t, env, -32001, "TASK_NOT_FOUND")

	// So do a malformed id and a random one: indistinguishable.
	_, env = rpc(t, mux, rawB, "GetTask", map[string]any{"id": "not-a-uuid"})
	wantRPCError(t, env, -32001, "TASK_NOT_FOUND")
	_, env = rpc(t, mux, rawB, "GetTask", map[string]any{"id": uuid.NewString()})
	wantRPCError(t, env, -32001, "TASK_NOT_FOUND")

	// ListTasks: B sees an empty page, A sees one, the admin key sees all.
	_, env = rpc(t, mux, rawB, "ListTasks", map[string]any{})
	if env == nil || env.Error != nil {
		t.Fatalf("B ListTasks: %+v", env)
	}
	var listB struct {
		Tasks         []json.RawMessage `json:"tasks"`
		TotalSize     int               `json:"totalSize"`
		NextPageToken *string           `json:"nextPageToken"`
	}
	if err := json.Unmarshal(env.Result, &listB); err != nil {
		t.Fatal(err)
	}
	if listB.TotalSize != 0 || len(listB.Tasks) != 0 {
		t.Errorf("key B sees %d tasks, want 0", listB.TotalSize)
	}
	if listB.NextPageToken == nil || *listB.NextPageToken != "" {
		t.Errorf("nextPageToken must be present and empty on the last page, got %v", listB.NextPageToken)
	}

	_, env = rpc(t, mux, rawA, "ListTasks", map[string]any{})
	var listA struct {
		TotalSize int `json:"totalSize"`
	}
	if err := json.Unmarshal(env.Result, &listA); err != nil || listA.TotalSize != 1 {
		t.Errorf("key A sees %d tasks, want 1", listA.TotalSize)
	}

	_, env = rpc(t, mux, "admin-key", "ListTasks", map[string]any{})
	if err := json.Unmarshal(env.Result, &listA); err != nil || listA.TotalSize != 1 {
		t.Errorf("admin sees %d tasks, want 1 (fleet-wide)", listA.TotalSize)
	}

	// Attribution sanity for the SQL-side filter.
	row, _ := store.GetTask(uuid.MustParse(taskID))
	if row == nil || row.CreatedByKeyID == nil || *row.CreatedByKeyID != keyA {
		t.Fatalf("seeded task attribution wrong: %+v", row)
	}
}

func TestA2AListTasksPaginationAndFilters(t *testing.T) {
	_, keyMgr, mux := setupA2A(t)
	rawKey, _ := mintTaskKey(t, keyMgr, "pager")
	for i := 0; i < 3; i++ {
		if _, env := rpc(t, mux, rawKey, "SendMessage", map[string]any{
			"message": userMessage(fmt.Sprintf("Pagination fixture number %d.", i), ""),
		}); env == nil || env.Error != nil {
			t.Fatalf("create %d: %+v", i, env)
		}
	}

	var page struct {
		Tasks         []json.RawMessage `json:"tasks"`
		TotalSize     int               `json:"totalSize"`
		PageSize      int               `json:"pageSize"`
		NextPageToken string            `json:"nextPageToken"`
	}
	_, env := rpc(t, mux, rawKey, "ListTasks", map[string]any{"pageSize": 2})
	if err := json.Unmarshal(env.Result, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 2 || page.TotalSize != 3 || page.NextPageToken == "" {
		t.Fatalf("page 1 wrong: %+v", page)
	}
	_, env = rpc(t, mux, rawKey, "ListTasks", map[string]any{"pageSize": 2, "pageToken": page.NextPageToken})
	if err := json.Unmarshal(env.Result, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 1 || page.NextPageToken != "" {
		t.Fatalf("page 2 wrong: %+v", page)
	}

	// Status filter: everything is SUBMITTED; COMPLETED matches nothing;
	// REJECTED (a state fleet never stores) is a legitimate empty set.
	_, env = rpc(t, mux, rawKey, "ListTasks", map[string]any{"status": "TASK_STATE_SUBMITTED"})
	if err := json.Unmarshal(env.Result, &page); err != nil || page.TotalSize != 3 {
		t.Errorf("SUBMITTED filter: %+v (%v)", page, err)
	}
	_, env = rpc(t, mux, rawKey, "ListTasks", map[string]any{"status": "TASK_STATE_COMPLETED"})
	if err := json.Unmarshal(env.Result, &page); err != nil || page.TotalSize != 0 {
		t.Errorf("COMPLETED filter: %+v (%v)", page, err)
	}
	_, env = rpc(t, mux, rawKey, "ListTasks", map[string]any{"status": "TASK_STATE_REJECTED"})
	if err := json.Unmarshal(env.Result, &page); err != nil || page.TotalSize != 0 {
		t.Errorf("REJECTED filter: %+v (%v)", page, err)
	}

	// Refusals: junk status, out-of-range pageSize, junk pageToken, and the
	// unsupported timestamp filter (refused loudly, never silently dropped).
	for name, params := range map[string]map[string]any{
		"status":    {"status": "RUNNING"},
		"pageSize":  {"pageSize": 101},
		"pageToken": {"pageToken": "zzz"},
		"timestamp": {"statusTimestampAfter": "2026-01-01T00:00:00Z"},
	} {
		_, env := rpc(t, mux, rawKey, "ListTasks", params)
		if env == nil || env.Error == nil || env.Error.Code != -32602 {
			t.Errorf("%s: %+v, want -32602", name, env)
		}
	}
}

func TestA2ACancelTask(t *testing.T) {
	store, keyMgr, mux := setupA2A(t)
	rawA, keyA := mintTaskKey(t, keyMgr, "canceller")
	rawB, _ := mintTaskKey(t, keyMgr, "other")

	_, env := rpc(t, mux, rawA, "SendMessage", map[string]any{"message": userMessage("A task to cancel.", "")})
	var created struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(env.Result, &created); err != nil {
		t.Fatal(err)
	}
	taskID := created.Task.ID

	// Another key cannot even see it.
	_, env = rpc(t, mux, rawB, "CancelTask", map[string]any{"id": taskID})
	wantRPCError(t, env, -32001, "TASK_NOT_FOUND")

	// The creator cancels its own task — the deliberate A2A-scoped extension
	// over the REST surface (see a2aCancelTask).
	_, env = rpc(t, mux, rawA, "CancelTask", map[string]any{"id": taskID})
	if env == nil || env.Error != nil {
		t.Fatalf("cancel: %+v", env)
	}
	var out struct {
		Status struct {
			State   string `json:"state"`
			Message *struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		} `json:"status"`
	}
	if err := json.Unmarshal(env.Result, &out); err != nil || out.Status.State != "TASK_STATE_CANCELED" {
		t.Fatalf("cancel result: %s (%v)", env.Result, err)
	}
	if out.Status.Message == nil || !strings.Contains(out.Status.Message.Parts[0].Text, "stopped by") {
		t.Errorf("cancel should carry the stop attribution, got %+v", out.Status.Message)
	}

	// Idempotent: cancelling again reports the same CANCELED task.
	_, env = rpc(t, mux, rawA, "CancelTask", map[string]any{"id": taskID})
	if env == nil || env.Error != nil {
		t.Fatalf("second cancel: %+v", env)
	}

	// A task in a DIFFERENT terminal state is NotCancelable.
	done := &models.Task{
		ID: uuid.New(), Prompt: "already finished", Status: models.TaskStatusSuccess,
		CreatedAt: time.Now().UTC(), CreatedByKeyID: &keyA, Timezone: "UTC",
	}
	if _, err := store.AddTask(done); err != nil {
		t.Fatal(err)
	}
	_, env = rpc(t, mux, rawA, "CancelTask", map[string]any{"id": done.ID.String()})
	wantRPCError(t, env, -32002, "TASK_NOT_CANCELABLE")

	// SubscribeToTask on a terminal task: the spec's UnsupportedOperation.
	_, env = rpc(t, mux, rawA, "SubscribeToTask", map[string]any{"id": done.ID.String()})
	wantRPCError(t, env, -32004, "UNSUPPORTED_OPERATION")
}

func TestA2AInputRequiredRoundTrip(t *testing.T) {
	store, keyMgr, mux := setupA2A(t)
	rawA, keyA := mintTaskKey(t, keyMgr, "answerer")

	paused := &models.Task{
		ID: uuid.New(), Prompt: "needs a human answer", Status: models.TaskStatusPausedAwaitingInput,
		PendingQuestion: "Deploy to staging or prod?", CreatedAt: time.Now().UTC(),
		CreatedByKeyID: &keyA, Timezone: "UTC",
	}
	if _, err := store.AddTask(paused); err != nil {
		t.Fatal(err)
	}
	// pending_question is a post-transition column the insert path deliberately
	// excludes (task_columns.go registry) — in production only the pause writer
	// sets it. Seed it the way the DB would have.
	if _, err := store.DB().Conn().ExecContext(context.Background(),
		"UPDATE tasks SET pending_question = $1 WHERE id = $2", paused.PendingQuestion, paused.ID); err != nil {
		t.Fatal(err)
	}

	// GetTask reports INPUT_REQUIRED with the question in status.message.
	_, env := rpc(t, mux, rawA, "GetTask", map[string]any{"id": paused.ID.String()})
	var got struct {
		Status struct {
			State   string `json:"state"`
			Message *struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		} `json:"status"`
	}
	if err := json.Unmarshal(env.Result, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.State != "TASK_STATE_INPUT_REQUIRED" || got.Status.Message == nil ||
		got.Status.Message.Parts[0].Text != paused.PendingQuestion {
		t.Fatalf("paused task shape wrong: %s", env.Result)
	}

	// A follow-up SendMessage with the taskId answers it through the resume seam.
	_, env = rpc(t, mux, rawA, "SendMessage", map[string]any{
		"message": userMessage("prod, please", paused.ID.String()),
	})
	if env == nil || env.Error != nil {
		t.Fatalf("answer: %+v", env)
	}
	row, err := store.GetTask(paused.ID)
	if err != nil || row == nil {
		t.Fatal(err)
	}
	if row.Status != models.TaskStatusPending {
		t.Errorf("answered task status = %s, want pending (re-queued)", row.Status)
	}

	// Answering a task that is not paused is refused.
	_, env = rpc(t, mux, rawA, "SendMessage", map[string]any{
		"message": userMessage("another answer", paused.ID.String()),
	})
	wantRPCError(t, env, -32004, "UNSUPPORTED_OPERATION")
}

// TestA2AStreamingLifecycle drives SubscribeToTask over a live server: the
// stream MUST open with the Task snapshot, carry a statusUpdate per
// transition, and CLOSE at terminal state — closure is the completion signal
// (no v0.3 `final` flag). Transitions are made in the DB, proving the row —
// not the in-memory buffer — is the event source (nothing lost to eviction).
func TestA2AStreamingLifecycle(t *testing.T) {
	store, keyMgr, mux := setupA2A(t)
	rawA, keyA := mintTaskKey(t, keyMgr, "streamer")

	task := &models.Task{
		ID: uuid.New(), Prompt: "watch me run", Status: models.TaskStatusPending,
		CreatedAt: time.Now().UTC(), CreatedByKeyID: &keyA, Timezone: "UTC",
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 9, "method": "SubscribeToTask",
		"params": map[string]any{"id": task.ID.String()},
	})
	req, _ := http.NewRequest("POST", srv.URL+"/a2a", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	req.Header.Set("X-API-Key", rawA)
	resp, err := srv.Client().Do(req) //nolint:bodyclose // closed by the deferred Close below; the reader goroutine confuses bodyclose's escape analysis
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// Drive the lifecycle in the DB while reading frames.
	go func() {
		time.Sleep(300 * time.Millisecond)
		if _, err := store.UpdateTasksStatusBatch([]uuid.UUID{task.ID}, models.TaskStatusPending, models.TaskStatusRunning); err != nil {
			t.Errorf("to running: %v", err)
		}
		time.Sleep(1500 * time.Millisecond)
		if _, err := store.UpdateTasksStatusBatch([]uuid.UUID{task.ID}, models.TaskStatusRunning, models.TaskStatusSuccess); err != nil {
			t.Errorf("to success: %v", err)
		}
	}()

	type frame struct {
		kind  string
		state string
	}
	var frames []frame
	deadline := time.After(15 * time.Second)
	lines := make(chan string)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
read:
	for {
		select {
		case <-deadline:
			t.Fatalf("stream did not close; frames so far: %+v", frames)
		case line, open := <-lines:
			if !open {
				break read // EOF: the server closed at terminal state.
			}
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue // blank separators and ": keepalive" comments
			}
			var env struct {
				ID     json.RawMessage `json:"id"`
				Result struct {
					Task *struct {
						Status struct {
							State string `json:"state"`
						} `json:"status"`
					} `json:"task"`
					StatusUpdate *struct {
						Status struct {
							State string `json:"state"`
						} `json:"status"`
					} `json:"statusUpdate"`
					ArtifactUpdate *struct{} `json:"artifactUpdate"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(data), &env); err != nil {
				t.Fatalf("bad frame %q: %v", data, err)
			}
			if string(env.ID) != "9" {
				t.Errorf("frame id = %s, want the request id 9", env.ID)
			}
			switch {
			case env.Result.Task != nil:
				frames = append(frames, frame{"task", env.Result.Task.Status.State})
			case env.Result.StatusUpdate != nil:
				frames = append(frames, frame{"statusUpdate", env.Result.StatusUpdate.Status.State})
			case env.Result.ArtifactUpdate != nil:
				frames = append(frames, frame{"artifactUpdate", ""})
			}
		}
	}

	if len(frames) < 3 {
		t.Fatalf("want ≥3 frames (snapshot, WORKING, COMPLETED), got %+v", frames)
	}
	if frames[0].kind != "task" || frames[0].state != "TASK_STATE_SUBMITTED" {
		t.Errorf("stream must OPEN with the Task snapshot, got %+v", frames[0])
	}
	last := frames[len(frames)-1]
	if last.kind != "statusUpdate" || last.state != "TASK_STATE_COMPLETED" {
		t.Errorf("stream must END with the terminal statusUpdate, got %+v", last)
	}
	sawWorking := false
	for _, f := range frames {
		if f.kind == "statusUpdate" && f.state == "TASK_STATE_WORKING" {
			sawWorking = true
		}
	}
	if !sawWorking {
		t.Errorf("missing the WORKING transition: %+v", frames)
	}
}
