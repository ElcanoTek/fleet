package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/google/uuid"
)

// fakeTaskMemoryStore is an in-memory TaskMemoryStore for the remember/recall
// tool tests. It records the caps passed to Upsert so the tool wiring is checked.
type fakeTaskMemoryStore struct {
	data          map[string]string
	lastMaxKeys   int
	lastMaxValueB int
	upsertErr     error
	listErr       error
}

func newFakeTMS() *fakeTaskMemoryStore { return &fakeTaskMemoryStore{data: map[string]string{}} }

func (f *fakeTaskMemoryStore) UpsertTaskMemory(_ context.Context, _ uuid.UUID, key, value string, maxKeys, maxValueBytes int) error {
	f.lastMaxKeys, f.lastMaxValueB = maxKeys, maxValueBytes
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.data[key] = value
	return nil
}

func (f *fakeTaskMemoryStore) GetTaskMemory(_ context.Context, _ uuid.UUID, key string) (string, error) {
	v, ok := f.data[key]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func (f *fakeTaskMemoryStore) ListTaskMemories(_ context.Context, _ uuid.UUID) ([]TaskMemory, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]TaskMemory, 0, len(f.data))
	for k, v := range f.data {
		out = append(out, TaskMemory{Key: k, Value: v})
	}
	return out, nil
}

func callTool(t *testing.T, tool fantasy.AgentTool, args any) fantasy.ToolResponse {
	t.Helper()
	raw, _ := json.Marshal(args)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: string(raw)})
	if err != nil {
		t.Fatalf("tool %s returned error: %v", tool.Info().Name, err)
	}
	return resp
}

func TestRememberTool_CommitsWithCaps(t *testing.T) {
	store := newFakeTMS()
	taskID := uuid.New()
	tool := NewRememberTool(store, taskID, TaskMemoryConfig{MaxKeys: 100, MaxValueBytes: 4096})
	if tool.Info().Name != "remember" {
		t.Fatalf("expected tool name 'remember', got %q", tool.Info().Name)
	}
	resp := callTool(t, tool, RememberParams{Key: "last_seen_price", Value: "42.17"})
	if resp.IsError {
		t.Fatalf("remember should succeed, got error response: %+v", resp)
	}
	if store.data["last_seen_price"] != "42.17" {
		t.Fatalf("value not committed: %+v", store.data)
	}
	if store.lastMaxKeys != 100 || store.lastMaxValueB != 4096 {
		t.Fatalf("caps not threaded to store: keys=%d valueB=%d", store.lastMaxKeys, store.lastMaxValueB)
	}
	// Empty key is rejected before the store is touched.
	store2 := newFakeTMS()
	tool2 := NewRememberTool(store2, taskID, TaskMemoryConfig{})
	if resp := callTool(t, tool2, RememberParams{Key: "", Value: "x"}); !resp.IsError {
		t.Fatal("empty key must be an error response")
	}
}

func TestRecallTool_AllAndSingle(t *testing.T) {
	store := newFakeTMS()
	store.data["a"] = "1"
	store.data["b"] = "2"
	taskID := uuid.New()
	tool := NewRecallTool(store, taskID)

	// No key → JSON object of all memories.
	resp := callTool(t, tool, RecallParams{})
	var obj map[string]string
	if err := json.Unmarshal([]byte(resp.Content), &obj); err != nil {
		t.Fatalf("recall(all) should return a JSON object, got %q (%v)", resp.Content, err)
	}
	if obj["a"] != "1" || obj["b"] != "2" {
		t.Fatalf("recall(all) wrong: %+v", obj)
	}
	// Single key → its value.
	if got := callTool(t, tool, RecallParams{Key: "a"}); got.Content != "1" {
		t.Fatalf("recall(a) = %q, want 1", got.Content)
	}
	// Missing key → graceful "no memory" (not an error response).
	if got := callTool(t, tool, RecallParams{Key: "missing"}); got.IsError || !strings.Contains(got.Content, "no memory") {
		t.Fatalf("recall(missing) should be a graceful note, got %+v", got)
	}
}

func TestRememberRecall_StoreErrors(t *testing.T) {
	taskID := uuid.New()

	// remember surfaces a store upsert error as an error response.
	es := newFakeTMS()
	es.upsertErr = errors.New("disk full")
	if got := callTool(t, NewRememberTool(es, taskID, TaskMemoryConfig{}), RememberParams{Key: "k", Value: "v"}); !got.IsError {
		t.Fatal("remember must return an error response when the store upsert fails")
	}

	// recall(all) surfaces a list error as an error response.
	ls := newFakeTMS()
	ls.listErr = errors.New("query failed")
	if got := callTool(t, NewRecallTool(ls, taskID), RecallParams{}); !got.IsError {
		t.Fatal("recall(all) must return an error response when the store list fails")
	}
}
