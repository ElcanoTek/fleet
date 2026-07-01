package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/store"
)

// This file is the always-on (provider-free, DB-free) coverage issue #49 asks
// for: it drives the real POST /chat handler — history replay → RunTurn → SSE →
// persistence — with MockMode=false, against an in-memory chatStore and a
// recording turnEngine. The real Manager.RunTurn assembly is covered separately
// and provider-free in internal/agent (manager_runturn_test.go); here the focus
// is the handler glue + the persisted transcript, neither of which the standard
// `go test` exercised before (they were behind FLEET_TEST_DATABASE_URL).

// fakeEngine is a turnEngine that records the TurnInput it receives (so a test
// can assert history replay) and streams a fixed event vocabulary, returning the
// turn's new history. It needs no model, sandbox, or network.
type fakeEngine struct {
	mu             sync.Mutex
	lastHistory    []agent.HistoryEntry
	turns          int
	providerHealth []agentcore.ModelHealth
}

func (f *fakeEngine) RunTurn(_ context.Context, in TurnInput, sink agent.EventSink) (*TurnResult, error) {
	f.mu.Lock()
	f.lastHistory = in.History
	f.turns++
	f.mu.Unlock()

	// Stream the vocabulary the SSE layer + frontend depend on.
	sink.Emit("turn.started", map[string]any{"persona": in.Persona})
	sink.Emit("tool.call", map[string]any{"name": "bash", "id": "call_1"})
	sink.Emit("tool.result", map[string]any{"id": "call_1", "text": "ok"})
	sink.Emit("text.delta", map[string]any{"text": "fake reply"})
	sink.Emit("turn.completed", map[string]any{"model": in.Model})

	return &TurnResult{
		FinalText: "fake reply",
		Model:     in.Model,
		NewHistory: []agent.HistoryEntry{
			{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"` + in.UserMessage + `"}`)},
			// A tool_call + tool_result pair so the audit-ledger derivation
			// (deriveToolCallEntries) in runTurnAsync has something to record —
			// this is what proves the in-process write path fires end to end.
			{Role: "assistant", Type: "tool_call", Content: json.RawMessage(`{"id":"call_1","name":"bash","input":"{\"command\":\"ls\"}"}`)},
			{Role: "tool", Type: "tool_result", Content: json.RawMessage(`{"id":"call_1","name":"bash","text":"ok","is_err":false}`)},
			{Role: "assistant", Type: "text", Content: json.RawMessage(`{"text":"fake reply"}`)},
		},
	}, nil
}

func (f *fakeEngine) Summarize(context.Context, SummarizeInput) (*SummarizeResult, error) {
	return &SummarizeResult{}, nil
}

// SuggestTitle returns "" so runTurnAsync skips the auto-title UpdateTitle path,
// keeping the fake store surface minimal.
func (f *fakeEngine) SuggestTitle(context.Context, string, string) string { return "" }
func (f *fakeEngine) ExtractMemories(context.Context, string, string, []string) []string {
	return nil
}
func (f *fakeEngine) MCPClient() *mcp.Client                       { return nil }
func (f *fakeEngine) SandboxPool() *sandbox.Pool                   { return nil }
func (f *fakeEngine) MCPServerCatalog() []agent.OptionalServerInfo { return nil }
func (f *fakeEngine) ListPersonas() ([]string, error)              { return nil, nil }
func (f *fakeEngine) ProviderHealth() []agentcore.ModelHealth      { return f.providerHealth }

// fakeChatStore is an in-memory chatStore. It embeds a nil *store.Store so it
// satisfies the (wide) interface for free; only the handful of methods the
// /chat turn path touches are overridden. Any un-overridden method panics on the
// nil embed — a deliberate tripwire that the test path stayed within the modeled
// surface.
type fakeChatStore struct {
	*store.Store // nil; promotes every chatStore method so the type satisfies it

	mu         sync.Mutex
	convs      map[string]*store.Conversation
	history    map[string][]agent.HistoryEntry
	turnRows   int
	appends    int
	recorded   int
	finishes   int
	created    int
	turnEvents int
	toolCalls  []store.ToolCallEntry
}

func newFakeChatStore() *fakeChatStore {
	return &fakeChatStore{
		convs:   map[string]*store.Conversation{},
		history: map[string][]agent.HistoryEntry{},
	}
}

func (s *fakeChatStore) CreateConversation(_ context.Context, userEmail, title, persona, model string, lockdown bool) (*store.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created++
	id := "conv-1"
	conv := &store.Conversation{ID: id, UserEmail: userEmail, Title: title, Persona: persona, Model: model, Lockdown: lockdown}
	s.convs[id] = conv
	return conv, nil
}

func (s *fakeChatStore) Get(_ context.Context, _, convID string) (*store.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.convs[convID], nil
}

func (s *fakeChatStore) LoadHistory(_ context.Context, convID string) ([]agent.HistoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.HistoryEntry(nil), s.history[convID]...), nil
}

func (s *fakeChatStore) AppendHistory(_ context.Context, convID string, entries []agent.HistoryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appends++
	s.history[convID] = append(s.history[convID], entries...)
	return nil
}

func (s *fakeChatStore) ListMemories(context.Context, string) ([]store.Memory, error) {
	return nil, nil
}

func (s *fakeChatStore) RecordTurn(context.Context, store.TurnMetric) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recorded++
	return nil
}

func (s *fakeChatStore) CreateTurn(context.Context, string, string, int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turnRows++
	return nil
}

func (s *fakeChatStore) InsertTurnEvents(_ context.Context, events []store.TurnEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turnEvents += len(events)
	return nil
}

func (s *fakeChatStore) FinishTurn(context.Context, string, store.TurnStatus, int64, bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishes++
	return nil
}

func (s *fakeChatStore) RecordToolCalls(_ context.Context, entries []store.ToolCallEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolCalls = append(s.toolCalls, entries...)
	return nil
}

func (s *fakeChatStore) ListToolCalls(_ context.Context, convID, toolFilter string, fromUnix int64, limit int) ([]store.ToolCallEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.ToolCallEntry, 0, len(s.toolCalls))
	for _, e := range s.toolCalls {
		if e.ConversationID != convID {
			continue
		}
		if toolFilter != "" && e.ToolName != toolFilter {
			continue
		}
		if fromUnix > 0 && e.StartedAt < fromUnix {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Sweeps + per-turn overrides the path may touch — all no-ops.
func (s *fakeChatStore) SweepExpired(context.Context, time.Duration, int) (int, int, error) {
	return 0, 0, nil
}
func (s *fakeChatStore) SweepOrphanWorkspaces(context.Context, string) (int, error) { return 0, nil }
func (s *fakeChatStore) SetModel(context.Context, string, string, string) error     { return nil }
func (s *fakeChatStore) SetRuntime(context.Context, string, string, string) error   { return nil }
func (s *fakeChatStore) SetOptionalMCPServers(context.Context, string, string, []string) error {
	return nil
}
func (s *fakeChatStore) UpdateTitle(context.Context, string, string, string) error { return nil }

// Bulk conversation operations (#279) — default no-ops; the /chat turn path
// never touches these, so a nil-safe stub keeps the always-on fake compiling.
func (s *fakeChatStore) DeleteByIDs(_ context.Context, _ string, ids []string) (int, error) {
	return len(ids), nil
}
func (s *fakeChatStore) DeleteAllMatching(_ context.Context, _ string, _, _ string) (int, error) {
	return 0, nil
}
func (s *fakeChatStore) BulkPatch(_ context.Context, _ string, ids []string, _ *bool, _ *string, _ []string) (int, error) {
	return len(ids), nil
}

func newDefaultChatServer(t *testing.T, engine turnEngine, st chatStore) *Server {
	t.Helper()
	cfg := &config.Config{
		SharedToken:        "tok",
		PersonaDefault:     "generic",
		ConversationTTL:    14,
		UnpinnedCap:        50,
		MockMode:           false, // exercise the real RunTurn path, not runMockTurn
		EmailAttachmentDir: t.TempDir(),
	}
	srv := New(cfg, engine, st)
	srv.isMember = allowAllMembers
	return srv
}

func postChatRequest(t *testing.T, srv *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/chat", bytes.NewReader(raw))
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", "u@x.com")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, req)
	return w
}

// TestChatTurnPersistsTranscript_NoDBNoProvider drives a full /chat turn with
// MockMode=false against the in-memory store + recording engine, asserting the
// SSE event vocabulary AND that the handler persisted the turn (conversation,
// turn row, appended history, recorded metrics, sealed turn) — all by default,
// with no provider and no DB env var.
func TestChatTurnPersistsTranscript_NoDBNoProvider(t *testing.T) {
	engine := &fakeEngine{}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, engine, st)

	w := postChatRequest(t, srv, map[string]any{
		"message": "hello there",
		"persona": "generic",
		"model":   "anthropic/claude-opus-4.8",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	for _, want := range []string{
		"event: conversation",
		"event: turn.started",
		"event: tool.call",
		"event: tool.result",
		"event: text.delta",
		"event: turn.completed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE stream missing %q\n\n%s", want, body)
		}
	}

	// Persistence glue. CreateConversation/CreateTurn (in the handler) and
	// AppendHistory/RecordTurn (in runTurnAsync, before the deferred buffer seal)
	// have all completed by the time ServeHTTP returns — the seal closes the SSE
	// subscriber, which is what unblocks the response.
	st.mu.Lock()
	created, turnRows, appends, recorded := st.created, st.turnRows, st.appends, st.recorded
	hist := append([]agent.HistoryEntry(nil), st.history["conv-1"]...)
	toolCalls := append([]store.ToolCallEntry(nil), st.toolCalls...)
	st.mu.Unlock()

	if created != 1 {
		t.Errorf("CreateConversation calls = %d, want 1", created)
	}
	if turnRows != 1 {
		t.Errorf("CreateTurn calls = %d, want 1", turnRows)
	}
	if appends != 1 {
		t.Errorf("AppendHistory calls = %d, want 1", appends)
	}
	if recorded != 1 {
		t.Errorf("RecordTurn calls = %d, want 1", recorded)
	}
	// The appended transcript must carry the assistant reply (last entry).
	if len(hist) == 0 || hist[len(hist)-1].Role != "assistant" {
		t.Fatalf("persisted history = %+v, want assistant reply last", hist)
	}
	// The tool-call audit ledger (#224) must have captured the turn's one tool
	// call — proof the write path in runTurnAsync fires on the default path.
	if len(toolCalls) != 1 {
		t.Fatalf("RecordToolCalls entries = %d, want 1: %+v", len(toolCalls), toolCalls)
	}
	if toolCalls[0].ToolName != "bash" || toolCalls[0].ConversationID != "conv-1" {
		t.Errorf("audit entry wrong: %+v", toolCalls[0])
	}
	if toolCalls[0].TurnID == "" {
		t.Errorf("audit entry missing turn id: %+v", toolCalls[0])
	}

	// FinishTurn (the turn-event ledger seal) runs in the buffer's persister flow
	// AFTER subscribers are closed, so it is eventual relative to the response.
	eventually(t, 2*time.Second, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.finishes == 1
	}, "FinishTurn was not called (turn never sealed)")
}

// eventually polls cond until it is true or the timeout elapses.
func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestChatSecondTurnReplaysHistory proves the handler's history-replay glue: a
// second turn on the same conversation must hand the prior turn's persisted
// transcript to RunTurn as TurnInput.History.
func TestChatSecondTurnReplaysHistory(t *testing.T) {
	engine := &fakeEngine{}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, engine, st)

	if w := postChatRequest(t, srv, map[string]any{
		"message": "first turn",
		"model":   "anthropic/claude-opus-4.8",
	}); w.Code != http.StatusOK {
		t.Fatalf("turn 1 status %d: %s", w.Code, w.Body.String())
	}

	if w := postChatRequest(t, srv, map[string]any{
		"conversation_id": "conv-1",
		"message":         "second turn",
		"model":           "anthropic/claude-opus-4.8",
	}); w.Code != http.StatusOK {
		t.Fatalf("turn 2 status %d: %s", w.Code, w.Body.String())
	}

	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.turns != 2 {
		t.Fatalf("engine saw %d turns, want 2", engine.turns)
	}
	// Turn 2 must have replayed turn 1's full transcript: the user message, the
	// tool_call + tool_result pair, and the assistant reply (see fakeEngine).
	if len(engine.lastHistory) != 4 {
		t.Fatalf("turn 2 replayed %d history entries, want 4 (turn 1's user+tool_call+tool_result+assistant)", len(engine.lastHistory))
	}
	if engine.lastHistory[0].Role != "user" || engine.lastHistory[len(engine.lastHistory)-1].Role != "assistant" {
		t.Errorf("replayed history roles = %q…%q, want user…assistant",
			engine.lastHistory[0].Role, engine.lastHistory[len(engine.lastHistory)-1].Role)
	}
}
