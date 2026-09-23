package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	lastModel      string
	turns          int
	providerHealth []agentcore.ModelHealth
}

func (f *fakeEngine) RunTurn(ctx context.Context, in TurnInput, sink agent.EventSink) (*TurnResult, error) {
	f.mu.Lock()
	f.lastHistory = in.History
	f.lastModel = in.Model
	f.turns++
	f.mu.Unlock()

	newHistory := []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"` + in.UserMessage + `"}`)},
		// A tool_call + tool_result pair so the audit-ledger derivation
		// (deriveToolCallEntries) in runTurnAsync has something to record —
		// this is what proves the in-process write path fires end to end.
		{Role: "assistant", Type: "tool_call", Content: json.RawMessage(`{"id":"call_1","name":"bash","input":"{\"command\":\"ls\"}"}`)},
		{Role: "tool", Type: "tool_result", Content: json.RawMessage(`{"id":"call_1","name":"bash","text":"ok","is_err":false}`)},
		{Role: "assistant", Type: "text", Content: json.RawMessage(`{"text":"fake reply"}`)},
	}

	// Honor the engine's #798 commit contract exactly like agent.Manager: the
	// user entry commits before any work, the terminal projection commits
	// BEFORE turn.completed is emitted, and a failure is a turn error.
	if in.CommitUser != nil {
		if err := in.CommitUser(ctx, newHistory[0]); err != nil {
			return nil, err
		}
	}

	// Stream the vocabulary the SSE layer + frontend depend on.
	sink.Emit("turn.started", map[string]any{"persona": in.Persona})
	sink.Emit("tool.call", map[string]any{"name": "bash", "id": "call_1"})
	sink.Emit("tool.result", map[string]any{"id": "call_1", "text": "ok"})
	sink.Emit("text.delta", map[string]any{"text": "fake reply"})

	if in.CommitTerminal != nil {
		if err := in.CommitTerminal(newHistory[1:], false); err != nil {
			return nil, err
		}
	}
	sink.Emit("turn.completed", map[string]any{"model": in.Model})

	return &TurnResult{
		FinalText:  "fake reply",
		Model:      in.Model,
		NewHistory: newHistory,
	}, nil
}

func (f *fakeEngine) Summarize(context.Context, SummarizeInput) (*SummarizeResult, error) {
	return &SummarizeResult{}, nil
}

// SuggestTitle returns "" so runTurnAsync skips the auto-title UpdateTitle path,
// keeping the fake store surface minimal.
func (f *fakeEngine) SuggestTitle(context.Context, string, string) string { return "" }
func (f *fakeEngine) ExtractMemories(context.Context, string, string, []string) []agent.ExtractedFact {
	return nil
}
func (f *fakeEngine) SuggestRecurringTask(context.Context, string, []string) (*agent.RecurringTaskProposal, error) {
	return nil, nil
}
func (f *fakeEngine) SuggestLibraryPrompt(context.Context, agent.LibraryPromptInput) (*agent.LibraryPromptDraft, error) {
	return nil, nil
}
func (f *fakeEngine) MCPBroker() agentcore.MCPBroker { return nil }
func (f *fakeEngine) MCPCatalog() []mcp.ServerTool   { return nil }
func (f *fakeEngine) OpenApprovalRemoteMCPScope(context.Context, string, string, string) (*agent.RemoteMCPOverlay, error) {
	return nil, nil
}

func (f *fakeEngine) OpenApprovalMCPScope(context.Context, agentcore.MCPSelection, string) (*agent.MCPScope, error) {
	return nil, nil
}
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
	setModels  int
	turnEvents int
	// deleteAllUnpinned counts DELETE /conversations fall-throughs so
	// #1110's malformed-body test can assert the wipe never ran.
	deleteAllUnpinned int
	toolCalls         []store.ToolCallEntry
	queue             []store.InputQueueRow
	acceptedSeq       atomic.Int64
	// releaseFailures / bindFailures make that many ReleaseDirectInput /
	// BindInputTurn calls fail first.
	releaseFailures, bindFailures, settleFailures int
	// bindLostAcks makes that many binds commit and then report an error
	// (the database acknowledgement was lost).
	bindLostAcks int
	// onClaim runs after a direct claim is stored; onMemories when turn
	// preparation reads memories (both outside the fake's lock).
	onClaim    func(store.InputQueueRow)
	onMemories func()
	// onCreate runs on each CreateConversation call, before it creates.
	onCreate func()
	keyLocks sync.Map
}

func newFakeChatStore() *fakeChatStore {
	return &fakeChatStore{
		convs:   map[string]*store.Conversation{},
		history: map[string][]agent.HistoryEntry{},
	}
}

// Connector prefs (unified connector UX): the fake has no prefs, meaning
// operator defaults everywhere — the turn path reads this on every run.
func (s *fakeChatStore) ListConnectorPrefs(_ context.Context, _ string) (map[string]store.ConnectorPref, error) {
	return nil, nil
}

// User skills (docs/SKILLS.md phase 2): the fake has none — the turn path
// lists them on every run.
func (s *fakeChatStore) ListUserSkills(_ context.Context, _ string) ([]store.UserSkill, error) {
	return nil, nil
}

// Shared file library (docs/SHARED-FILES.md): the fake has none — the turn
// path lists it on every run for the prompt block, and an empty library
// appends nothing.
func (s *fakeChatStore) ListSharedFiles(_ context.Context) ([]store.SharedFile, error) {
	return nil, nil
}

func (s *fakeChatStore) CreateConversation(_ context.Context, userEmail, title, persona, model string, lockdown bool) (*store.Conversation, error) {
	if s.onCreate != nil {
		s.onCreate()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created++
	id := "conv-1"
	if s.created > 1 {
		id = fmt.Sprintf("conv-%d", s.created)
	}
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

func (s *fakeChatStore) AppendHistory(_ context.Context, convID string, entries []agent.HistoryEntry) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appends++
	s.history[convID] = append(s.history[convID], entries...)
	// Synthetic ascending ids, mirroring the real RETURNING order.
	ids := make([]int64, len(entries))
	for i := range ids {
		ids[i] = int64(len(s.history[convID]) - len(entries) + i + 1)
	}
	return ids, nil
}

// Durable turn journal + gated projection (#798): the fake routes both commit
// paths into the same in-memory history AppendHistory feeds, so history and
// ordering assertions cover the new commit-before-terminal flow.
func (s *fakeChatStore) CommitUserMessage(ctx context.Context, convID, _ string, entry agent.HistoryEntry) (int64, error) {
	ids, err := s.AppendHistory(ctx, convID, []agent.HistoryEntry{entry})
	if err != nil {
		return 0, err
	}
	return ids[0], nil
}

func (s *fakeChatStore) CommitTurnHistory(ctx context.Context, convID, _ string, entries []agent.HistoryEntry) ([]int64, error) {
	return s.AppendHistory(ctx, convID, entries)
}

func (s *fakeChatStore) InsertTurnJournal(context.Context, store.TurnJournalRow) error { return nil }

func (s *fakeChatStore) ListMemories(context.Context, string) ([]store.Memory, error) {
	if s.onMemories != nil {
		s.onMemories()
	}
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
func (s *fakeChatStore) PurgeTerminalInputs(context.Context, time.Duration) (int, error) {
	return 0, nil
}
func (s *fakeChatStore) SweepTurnEvents(context.Context, time.Duration) (int, error) {
	return 0, nil
}
func (s *fakeChatStore) SweepOrphanWorkspaces(context.Context, string) (int, error) { return 0, nil }

// SetModel records the per-turn model override (#568) so tests can assert that
// a rejected lockdown override never reaches the store.
func (s *fakeChatStore) SetModel(_ context.Context, _, convID, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setModels++
	if c := s.convs[convID]; c != nil {
		c.Model = model
	}
	return nil
}
func (s *fakeChatStore) SetRuntime(context.Context, string, string, string) error { return nil }
func (s *fakeChatStore) SetConversationMCPAccounts(context.Context, string, string, map[string]string) error {
	return nil
}

func (s *fakeChatStore) SetOptionalMCPServers(context.Context, string, string, []string) error {
	return nil
}
func (s *fakeChatStore) UpdateTitle(context.Context, string, string, string) error { return nil }

// Bulk conversation operations (#279) — default no-ops; the /chat turn path
// never touches these, so a nil-safe stub keeps the always-on fake compiling.
func (s *fakeChatStore) DeleteByIDs(_ context.Context, _ string, ids []string) (int, error) {
	return len(ids), nil
}
func (s *fakeChatStore) DeleteAllMatching(_ context.Context, _, _ string) (int, error) {
	return 0, nil
}
func (s *fakeChatStore) DeleteAllUnpinned(_ context.Context, _ string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteAllUnpinned++
	return 0, nil
}
func (s *fakeChatStore) BulkPatch(_ context.Context, _ string, ids []string, _ *bool, _ []string) (int, error) {
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
	// Two commits per turn since #798: the user entry before the first
	// provider call, then the terminal projection before turn.completed.
	if appends != 2 {
		t.Errorf("history commits = %d, want 2 (user pre-run + terminal projection)", appends)
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
	eventually(t, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.finishes == 1
	}, "FinishTurn was not called (turn never sealed)")
}

// eventuallyTimeout bounds how long eventually polls before failing. A single
// constant (rather than a per-call parameter that every caller passes the same
// value for) keeps the helper's contract uniform across the package's async
// assertions.
const eventuallyTimeout = 2 * time.Second

// eventually polls cond until it is true or eventuallyTimeout elapses.
func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(eventuallyTimeout)
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

// decodeLockdownRefusal reads the self-correcting body of a refused lockdown
// model override (#1588) and asserts its machine-readable marker — the field a
// client keys its retry off.
func decodeLockdownRefusal(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("refusal body is not JSON (%v): %s", err, w.Body.String())
	}
	if body["code"] != lockdownModelRefusalCode {
		t.Fatalf("refusal code = %v, want %q: %s", body["code"], lockdownModelRefusalCode, w.Body.String())
	}
	return body
}

// TestPostChat_LockdownModelOverrideGuard is the #568 regression: the per-turn
// model override in postChat's existing-conversation branch must pass the SAME
// lockdown allow-list guard as PATCH /conversations/{id}/model and
// conversation create. A disallowed override on a lockdown conversation is a
// 400 that neither persists the model nor runs the turn.
func TestPostChat_LockdownModelOverrideGuard(t *testing.T) {
	seed := func(st *fakeChatStore, lockdown bool) {
		st.convs["conv-1"] = &store.Conversation{
			ID: "conv-1", UserEmail: "u@x.com", Title: "t",
			Persona: "generic", Model: "a/b", Lockdown: lockdown,
		}
	}

	// A disallowed slug never reaches the store and never runs — and the
	// caller is told, rather than silently served a turn on some other model.
	t.Run("disallowed override never persists and never runs", func(t *testing.T) {
		engine := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, engine, st)
		srv.cfg.LockdownAllowedModels = []string{"a/b", "c/d"}
		seed(st, true)

		w := postChatRequest(t, srv, map[string]any{
			"conversation_id": "conv-1",
			"model":           "evil/unvetted-model",
			"message":         "hello",
		})
		decodeLockdownRefusal(t, w)
		st.mu.Lock()
		model, setModels := st.convs["conv-1"].Model, st.setModels
		st.mu.Unlock()
		if setModels != 0 || model != "a/b" {
			t.Errorf("disallowed override reached the store: SetModel calls = %d, stored model = %q", setModels, model)
		}
		engine.mu.Lock()
		turns := engine.turns
		engine.mu.Unlock()
		if turns != 0 {
			t.Errorf("a refused override must not run a turn: turns=%d", turns)
		}
	})

	t.Run("allow-listed override on lockdown conversation still works", func(t *testing.T) {
		engine := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, engine, st)
		srv.cfg.LockdownAllowedModels = []string{"a/b", "c/d"}
		seed(st, true)

		w := postChatRequest(t, srv, map[string]any{
			"conversation_id": "conv-1",
			"model":           "c/d",
			"message":         "hello",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		st.mu.Lock()
		model := st.convs["conv-1"].Model
		st.mu.Unlock()
		if model != "c/d" {
			t.Errorf("allow-listed override not persisted: stored model = %q, want c/d", model)
		}
	})

	// The persisted model fell off the allow-list (operator narrowed it, or the
	// tier defaults moved on an upgrade / admin override): the turn must still
	// run — on the lockdown default, persisted — instead of 400ing forever.
	t.Run("lockdown conversation on a delisted model moves to the lockdown default", func(t *testing.T) {
		engine := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, engine, st)
		srv.cfg.LockdownAllowedModels = []string{"c/d", "e/f"}
		seed(st, true) // persisted model a/b, no longer allowed

		w := postChatRequest(t, srv, map[string]any{
			"conversation_id": "conv-1",
			"message":         "hello",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		st.mu.Lock()
		model, setModels := st.convs["conv-1"].Model, st.setModels
		st.mu.Unlock()
		if model != "c/d" || setModels != 1 {
			t.Errorf("delisted lockdown model not moved to the lockdown default: stored model = %q, SetModel calls = %d", model, setModels)
		}
		engine.mu.Lock()
		turns, turnModel := engine.turns, engine.lastModel
		engine.mu.Unlock()
		if turns != 1 || turnModel != "c/d" {
			t.Errorf("turn should have run once on the lockdown default: turns=%d model=%q", turns, turnModel)
		}
	})

	// The web echoes the stored model on every turn. That echo of the stale
	// persisted slug must not be mistaken for an override TO the delisted
	// model — it is the exact case the migration exists for.
	t.Run("client echoing the delisted persisted model is migrated, not rejected", func(t *testing.T) {
		engine := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, engine, st)
		srv.cfg.LockdownAllowedModels = []string{"c/d"}
		seed(st, true) // persisted a/b

		w := postChatRequest(t, srv, map[string]any{
			"conversation_id": "conv-1",
			"model":           "a/b", // what the web sends: the conversation's own stored model
			"message":         "hello",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		st.mu.Lock()
		model := st.convs["conv-1"].Model
		st.mu.Unlock()
		engine.mu.Lock()
		turnModel := engine.lastModel
		engine.mu.Unlock()
		if model != "c/d" || turnModel != "c/d" {
			t.Errorf("echoed delisted model should migrate to the lockdown default: stored=%q turn=%q", model, turnModel)
		}
	})

	// The refusal is self-correcting (#1588): it names the conversation's own
	// model, so the caller — an API client reading the error, or a browser
	// holding a slug the allow-list has moved past — can adopt it and resend
	// instead of looping on a "no" it can only escape by reloading.
	t.Run("the refusal names the conversation's model for the client to adopt", func(t *testing.T) {
		engine := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, engine, st)
		srv.cfg.LockdownAllowedModels = []string{"a/b", "c/d"}
		seed(st, true) // stored a/b, which is allowed

		w := postChatRequest(t, srv, map[string]any{
			"conversation_id": "conv-1",
			"model":           "evil/unvetted-model",
			"message":         "hello",
		})
		if got := decodeLockdownRefusal(t, w)["model"]; got != "a/b" {
			t.Errorf("refusal model = %v, want the conversation's own a/b: %s", got, w.Body.String())
		}

		// And the retry the body invites actually works.
		w = postChatRequest(t, srv, map[string]any{
			"conversation_id": "conv-1",
			"model":           "a/b",
			"message":         "hello",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("the corrected retry must run: status = %d: %s", w.Code, w.Body.String())
		}
		engine.mu.Lock()
		turns, turnModel := engine.turns, engine.lastModel
		engine.mu.Unlock()
		if turns != 1 || turnModel != "a/b" {
			t.Errorf("the corrected retry must run exactly one turn on a/b: turns=%d model=%q", turns, turnModel)
		}
	})

	// The stored model is delisted too, so echoing it back would send a
	// retrying client straight into a second refusal. The correction is the
	// lockdown default — the slug reconcileLockdownModelCtx would migrate this
	// conversation to on its next launch anyway.
	t.Run("a delisted stored model is corrected to the lockdown default", func(t *testing.T) {
		engine := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, engine, st)
		srv.cfg.LockdownAllowedModels = []string{"c/d", "e/f"}
		seed(st, true) // stored a/b, disallowed

		w := postChatRequest(t, srv, map[string]any{
			"conversation_id": "conv-1",
			"model":           "evil/unvetted-model",
			"message":         "hello",
		})
		if got := decodeLockdownRefusal(t, w)["model"]; got != "c/d" {
			t.Errorf("refusal model = %v, want the lockdown default c/d: %s", got, w.Body.String())
		}
	})

	// With no literal slug anywhere on the allow-list there is nothing safe to
	// name: the refusal carries no correction rather than a slug that would be
	// refused again, and the caller has to choose.
	t.Run("refused with no correction when the allow-list names no literal slug", func(t *testing.T) {
		engine := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, engine, st)
		srv.cfg.LockdownAllowedModels = []string{"z/*"} // globs only: no migration target
		seed(st, true)                                  // stored a/b, disallowed

		w := postChatRequest(t, srv, map[string]any{
			"conversation_id": "conv-1",
			"model":           "evil/unvetted-model",
			"message":         "hello",
		})
		if got, ok := decodeLockdownRefusal(t, w)["model"]; ok {
			t.Errorf("refusal must not name a model it would refuse, got %v: %s", got, w.Body.String())
		}
	})

	// A list that starts with a glob still has a literal slug further along:
	// that is the lockdown default, not the glob.
	t.Run("glob-first allow-list migrates to its first literal slug", func(t *testing.T) {
		engine := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, engine, st)
		srv.cfg.LockdownAllowedModels = []string{"c/*", "e/f", "g/h"}
		seed(st, true)

		w := postChatRequest(t, srv, map[string]any{
			"conversation_id": "conv-1",
			"message":         "hello",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		st.mu.Lock()
		model := st.convs["conv-1"].Model
		st.mu.Unlock()
		if model != "e/f" {
			t.Errorf("glob-first list should migrate to the first literal slug e/f, got %q", model)
		}
	})

	// A glob-only list has no literal slug to move to: leave it to the guard.
	t.Run("glob-only allow-list does not invent a model", func(t *testing.T) {
		engine := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, engine, st)
		srv.cfg.LockdownAllowedModels = []string{"c/*"}
		seed(st, true)

		postChatRequest(t, srv, map[string]any{
			"conversation_id": "conv-1",
			"message":         "hello",
		})
		st.mu.Lock()
		model, setModels := st.convs["conv-1"].Model, st.setModels
		st.mu.Unlock()
		if model != "a/b" || setModels != 0 {
			t.Errorf("glob-only list must not rewrite the model: stored=%q SetModel calls=%d", model, setModels)
		}
	})

	t.Run("non-lockdown conversation unaffected", func(t *testing.T) {
		engine := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, engine, st)
		srv.cfg.LockdownAllowedModels = []string{"a/b"}
		seed(st, false)

		w := postChatRequest(t, srv, map[string]any{
			"conversation_id": "conv-1",
			"model":           "any/model-at-all",
			"message":         "hello",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		st.mu.Lock()
		model := st.convs["conv-1"].Model
		st.mu.Unlock()
		if model != "any/model-at-all" {
			t.Errorf("non-lockdown override not persisted: stored model = %q", model)
		}
	})
}

// In-memory input queue (#785): the fake mirrors the store's state machine so
// the busy-path, drain, and steer flows are exercised without Postgres.
func (s *fakeChatStore) EnqueueInput(_ context.Context, r store.InputQueueRow) (store.InputQueueRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.queue {
		if it.ConversationID == r.ConversationID && it.ClientInputID == r.ClientInputID {
			return it, false, nil
		}
	}
	now := time.Now().Unix()
	r.State = store.InputStateQueued
	r.Position = int64(len(s.queue) + 1)
	r.CreatedAt, r.UpdatedAt, r.AcceptedSeq = now, now, s.acceptedSeq.Add(1)
	s.queue = append(s.queue, r)
	return r, true, nil
}

func (s *fakeChatStore) AcceptedInputSeq() int64 { return s.acceptedSeq.Load() }

func (s *fakeChatStore) CountPendingInputs(_ context.Context, convID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, it := range s.queue {
		if it.ConversationID == convID && it.State == store.InputStateQueued {
			n++
		}
	}
	return n, nil
}

func (s *fakeChatStore) ListQueuedInputs(_ context.Context, _, convID string) ([]store.InputQueueRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.InputQueueRow
	for _, it := range s.queue {
		if it.ConversationID == convID && it.Mode != store.InputModeDirect && it.State != store.InputStateCompleted && it.State != store.InputStateCancelled {
			out = append(out, it)
		}
	}
	return out, nil
}

func (s *fakeChatStore) ClaimNextQueuedInput(_ context.Context, convID, turnID string) (*store.InputQueueRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.queue {
		if s.queue[i].ConversationID == convID && s.queue[i].State == store.InputStateQueued {
			s.queue[i].State = store.InputStateRunning
			s.queue[i].TurnID = turnID
			row := s.queue[i]
			return &row, nil
		}
	}
	return nil, nil
}

func (s *fakeChatStore) MarkInputInjected(_ context.Context, id, turnID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.queue {
		if s.queue[i].ID == id && s.queue[i].State == store.InputStateQueued {
			s.queue[i].State = store.InputStateInjected
			s.queue[i].TurnID = turnID
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeChatStore) MarkInputTerminal(_ context.Context, id, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.queue {
		if s.queue[i].ID == id {
			s.queue[i].State = state
		}
	}
	return nil
}

func (s *fakeChatStore) MarkClaimedInputTerminal(_ context.Context, id, claimTurnID, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.queue {
		if s.queue[i].ID == id && s.queue[i].State == store.InputStateRunning && s.queue[i].TurnID == claimTurnID {
			s.queue[i].State = state
		}
	}
	return nil
}

func (s *fakeChatStore) CompleteInjectedInputs(_ context.Context, turnID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.queue {
		if s.queue[i].TurnID == turnID && s.queue[i].State == store.InputStateInjected {
			s.queue[i].State = store.InputStateCompleted
		}
	}
	return nil
}

func (s *fakeChatStore) CancelQueuedInputs(_ context.Context, _, convID string, upTo int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.queue {
		if s.queue[i].ConversationID == convID && s.queue[i].State == store.InputStateQueued && s.queue[i].AcceptedSeq <= upTo {
			s.queue[i].State = store.InputStateCancelled
			n++
		}
	}
	return n, nil
}

func (s *fakeChatStore) RemoveQueuedInput(_ context.Context, _, convID, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.queue {
		if s.queue[i].ID == id && s.queue[i].ConversationID == convID && s.queue[i].State == store.InputStateQueued {
			s.queue[i].State = store.InputStateCancelled
			return true, nil
		}
	}
	return false, nil
}

// Direct-turn idempotency claims (migration 064): same key space as the
// queue, mode 'direct', never listed or drained.
func (s *fakeChatStore) ClaimDirectInput(_ context.Context, r store.InputQueueRow) (store.InputQueueRow, bool, error) {
	s.mu.Lock()
	for _, it := range s.queue {
		if it.ConversationID == r.ConversationID && it.ClientInputID == r.ClientInputID {
			s.mu.Unlock()
			return it, false, nil
		}
	}
	now := time.Now().Unix()
	r.Mode, r.State = store.InputModeDirect, store.InputStateRunning
	r.Position = int64(len(s.queue) + 1)
	r.CreatedAt, r.UpdatedAt, r.AcceptedSeq = now, now, s.acceptedSeq.Add(1)
	s.queue = append(s.queue, r)
	hook := s.onClaim
	s.mu.Unlock()
	if hook != nil {
		hook(r)
	}
	return r, true, nil
}

func (s *fakeChatStore) ReleaseDirectInput(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.releaseFailures > 0 {
		s.releaseFailures--
		return errors.New("fake: release failed")
	}
	kept := s.queue[:0]
	for _, it := range s.queue {
		if it.ID == id && it.Mode == store.InputModeDirect && it.State == store.InputStateRunning {
			if it.TurnID == "" {
				continue // unbound: dropped
			}
			it.State = store.InputStateCancelled // bound before the launch was aborted
		}
		kept = append(kept, it)
	}
	s.queue = kept
	return nil
}

// SettleDirectInput mirrors the store: the fake treats any settled turn as
// committed, and a claim settled with no turn as never run.
func (s *fakeChatStore) SettleDirectInput(_ context.Context, id, turnID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settleFailures > 0 {
		s.settleFailures--
		return errors.New("fake: settle failed")
	}
	for i := range s.queue {
		if s.queue[i].ID == id && s.queue[i].Mode == store.InputModeDirect && s.queue[i].State == store.InputStateRunning {
			s.queue[i].State = store.InputStateCompleted
			if turnID == "" {
				s.queue[i].State = store.InputStateCancelled
			}
		}
	}
	return nil
}

func (s *fakeChatStore) BindInputTurn(_ context.Context, id, turnID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindFailures > 0 {
		s.bindFailures--
		return errors.New("fake: bind failed")
	}
	if s.bindLostAcks > 0 {
		s.bindLostAcks--
		for i := range s.queue {
			if s.queue[i].ID == id {
				s.queue[i].TurnID = turnID
			}
		}
		return errors.New("fake: bind acknowledgement lost")
	}
	for i := range s.queue {
		if s.queue[i].ID == id {
			s.queue[i].TurnID = turnID
		}
	}
	return nil
}

// LockInputKey mirrors the store's per-(user, key) lock in-process.
func (s *fakeChatStore) LockInputKey(_ context.Context, userEmail, key string) (func(), error) {
	v, _ := s.keyLocks.LoadOrStore(userEmail+"\x00"+key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock, nil
}

func (s *fakeChatStore) LookupInputForUser(_ context.Context, userEmail, clientID string) (*store.InputQueueRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.queue) - 1; i >= 0; i-- {
		if s.queue[i].UserEmail == userEmail && s.queue[i].ClientInputID == clientID {
			row := s.queue[i]
			return &row, nil
		}
	}
	return nil, nil
}

func (s *fakeChatStore) LookupInput(_ context.Context, convID, clientID string) (*store.InputQueueRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.queue {
		if it.ConversationID == convID && it.ClientInputID == clientID {
			row := it
			return &row, nil
		}
	}
	return nil, nil
}

// SettleTurnInputs mirrors the store's commit-state reconciliation: the fake
// treats any turn whose engine committed (history rows exist) as committed.
func (s *fakeChatStore) SettleTurnInputs(_ context.Context, turnID, drainedID string) (int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	requeued, cancelled := 0, 0
	for i := range s.queue {
		if drainedID != "" && s.queue[i].ID == drainedID && s.queue[i].State == store.InputStateRunning {
			s.queue[i].State = store.InputStateCompleted
		}
		if s.queue[i].TurnID == turnID && s.queue[i].State == store.InputStateInjected {
			s.queue[i].State = store.InputStateCompleted
		}
	}
	return requeued, cancelled, nil
}

func (s *fakeChatStore) PromoteQueuedInput(_ context.Context, _, convID, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.queue {
		if s.queue[i].ID == id && s.queue[i].ConversationID == convID && s.queue[i].State == store.InputStateQueued {
			s.queue[i].Position = -1
			return true, nil
		}
	}
	return false, nil
}

// A direct claim whose turn never launched must not outlive a failed release:
// left behind with no turn, it would answer every resend of its key "already
// running" until the next boot. The release is retried until it lands.
func TestReleaseDirectInput_RetriesAFailedRelease(t *testing.T) {
	prev := directReleaseBackoff
	directReleaseBackoff = 5 * time.Millisecond
	t.Cleanup(func() { directReleaseBackoff = prev })

	st := newFakeChatStore()
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	row, claimed, err := st.ClaimDirectInput(t.Context(), store.InputQueueRow{
		ID: "claim-1", ConversationID: "conv-1", UserEmail: "u@x.com", ClientInputID: "key-1",
	})
	if err != nil || !claimed {
		t.Fatalf("claim: %+v %v %v", row, claimed, err)
	}
	st.mu.Lock()
	st.releaseFailures = 3
	st.mu.Unlock()

	srv.releaseDirectInput("claim-1")

	deadline := time.Now().Add(5 * time.Second)
	for {
		st.mu.Lock()
		left := len(st.queue)
		st.mu.Unlock()
		if left == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the direct claim was never released after a failed release")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// shortDirectPauses shortens the in-place release/bind retry pauses.
func shortDirectPauses(t *testing.T) {
	t.Helper()
	prevPause, prevBackoff := directReleasePause, directReleaseBackoff
	directReleasePause, directReleaseBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { directReleasePause, directReleaseBackoff = prevPause, prevBackoff })
}

func (s *fakeChatStore) directRows() []store.InputQueueRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.InputQueueRow
	for _, it := range s.queue {
		if it.Mode == store.InputModeDirect {
			out = append(out, it)
		}
	}
	return out
}

// A claim that cannot be bound to its turn fails closed: unbound, a crash
// would settle it "never ran" and a resend could run the input a second time.
func TestDirectClaim_BindFailureFailsClosed(t *testing.T) {
	shortDirectPauses(t)
	eng := &fakeEngine{}
	st := newFakeChatStore()
	st.bindFailures = directBindAttempts
	srv := newDefaultChatServer(t, eng, st)

	w := postChatRequest(t, srv, map[string]any{"message": "send the report", "persona": "generic", "input_id": "bind-1"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	eng.mu.Lock()
	turns := eng.turns
	eng.mu.Unlock()
	if turns != 0 {
		t.Fatalf("the turn ran with an unbound claim (%d turns)", turns)
	}
	if rows := st.directRows(); len(rows) != 0 {
		t.Fatalf("the unbound claim was not released: %+v", rows)
	}
}

// Losing the registration race hands the input to the queue only once its
// claim is released; a release that cannot be confirmed fails the submission
// instead of acknowledging a row that no turn will ever run.
func TestDirectClaim_RaceLoserWithoutReleaseFails(t *testing.T) {
	shortDirectPauses(t)
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	st.releaseFailures = 1000
	st.onClaim = func(r store.InputQueueRow) {
		// Another surface's turn registers between the busy check and ours.
		srv.registerTurn(r.ConversationID, func() {})
	}

	w := postChatRequest(t, srv, map[string]any{"message": "send the report", "persona": "generic", "input_id": "race-1"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

// A Stop scope=all that begins after a direct claim was accepted covers it,
// whether it lands before the gate check or while the turn is prepared: the
// claim is cancelled and the turn never runs.
func TestDirectClaim_StopBeforeLaunchCancelsIt(t *testing.T) {
	for _, when := range []string{"after the claim", "during preparation"} {
		t.Run(when, func(t *testing.T) {
			eng := &fakeEngine{}
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, eng, st)
			var convID string
			stop := func() {
				srv.beginStopSweep(convID)
				srv.endStopSweep(convID)
			}
			st.onClaim = func(r store.InputQueueRow) {
				convID = r.ConversationID
				if when == "after the claim" {
					stop()
				}
			}
			if when == "during preparation" {
				st.onMemories = stop
			}

			w := postChatRequest(t, srv, map[string]any{"message": "send the report", "persona": "generic", "input_id": "stop-1"})
			if w.Code != http.StatusConflict {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			eng.mu.Lock()
			turns := eng.turns
			eng.mu.Unlock()
			if turns != 0 {
				t.Fatalf("a stopped claim ran (%d turns)", turns)
			}
			if rows := st.directRows(); len(rows) != 1 || rows[0].State != store.InputStateCancelled {
				t.Fatalf("claim = %+v, want one cancelled row", rows)
			}
		})
	}
}

func (s *fakeChatStore) createdConversations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.created
}

// A Stop naming an input by its key stops it on whichever side of turn
// registration it lands: a claim still being prepared is refused when it
// registers, a key marked before its submission arrives never launches, and
// a running turn for the key is cancelled.
func TestCancelByInputKey(t *testing.T) {
	t.Run("claim still being prepared", func(t *testing.T) {
		eng := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, eng, st)
		var convID string
		st.onClaim = func(r store.InputQueueRow) { convID = r.ConversationID }
		st.onMemories = func() { srv.cancelInput(context.Background(), "u@x.com", convID, "key-p") }
		w := postChatRequest(t, srv, map[string]any{"message": "send the report", "persona": "generic", "input_id": "key-p"})
		if w.Code != http.StatusConflict {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		if eng.turns != 0 {
			t.Fatalf("a stopped input ran (%d turns)", eng.turns)
		}
		if rows := st.directRows(); len(rows) != 1 || rows[0].State != store.InputStateCancelled {
			t.Fatalf("claim = %+v, want one cancelled row", rows)
		}
	})
	t.Run("submission still in transit", func(t *testing.T) {
		eng := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, eng, st)
		conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
		srv.cancelInput(context.Background(), "u@x.com", conv.ID, "key-t")
		w := postChatRequest(t, srv, map[string]any{"message": "send the report", "conversation_id": conv.ID, "input_id": "key-t"})
		if w.Code != http.StatusConflict || eng.turns != 0 {
			t.Fatalf("status %d, turns %d: a Stop that arrived first must refuse the late submission", w.Code, eng.turns)
		}
	})
	t.Run("expired mark", func(t *testing.T) {
		eng := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, eng, st)
		conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
		srv.inflightMu.Lock()
		srv.cancelledInputs = map[string]time.Time{inputKeyMark(conv.ID, "key-old"): time.Now().Add(-cancelledInputTTL - time.Minute)}
		srv.inflightMu.Unlock()
		w := postChatRequest(t, srv, map[string]any{"message": "send the report", "conversation_id": conv.ID, "input_id": "key-old"})
		if w.Code != http.StatusOK || eng.turns != 1 {
			t.Fatalf("status %d, turns %d: a Stop mark past its TTL must not refuse the key", w.Code, eng.turns)
		}
	})
	t.Run("running turn", func(t *testing.T) {
		eng := &gatedEngine{started: make(chan struct{}, 1), release: make(chan struct{}, 1)}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, eng, st)
		done := make(chan struct{})
		go func() {
			postChatRequest(t, srv, map[string]any{"message": "send the report", "persona": "generic", "input_id": "key-r"})
			close(done)
		}()
		<-eng.started
		srv.cancelInput(context.Background(), "u@x.com", "conv-1", "key-r")
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the running turn for the key was not cancelled")
		}
		if eng.cancelled.Load() != 1 {
			t.Fatalf("cancelled = %d, want the key's turn cancelled", eng.cancelled.Load())
		}
	})
}

// A direct claim whose settlement fails at turn end is settled on retry, not
// left "running" to answer every resend of its key "already running".
func TestDirectClaim_SettleIsRetried(t *testing.T) {
	shortDirectPauses(t)
	st := newFakeChatStore()
	st.settleFailures = 2
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	if w := postChatRequest(t, srv, map[string]any{"message": "hi", "persona": "generic", "input_id": "settle-1"}); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rows := st.directRows(); len(rows) == 1 && rows[0].State == store.InputStateCompleted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("claim never settled: %+v", st.directRows())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Two concurrent first submissions of one key (no conversation yet) run once:
// the second waits on the key's lock, then finds the first one's claim.
func TestFirstSubmission_ConcurrentSendsRunOnce(t *testing.T) {
	eng := &fakeEngine{}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, eng, st)
	body := map[string]any{"message": "send the report", "persona": "generic", "input_id": "first-1"}
	// Each conversation creation waits (bounded) for the other request to
	// get there too. Unserialized, both have looked the key up and found
	// nothing, so each creates a conversation and claims the key in it.
	// Serialized, the second is still waiting on the key's lock, so the
	// first times out here and claims, and the second then finds that claim.
	var arrived atomic.Int32
	st.onCreate = func() {
		arrived.Add(1)
		deadline := time.Now().Add(300 * time.Millisecond)
		for arrived.Load() < 2 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	second := make(chan int, 1)
	go func() { second <- postChatRequest(t, srv, body).Code }()
	if w := postChatRequest(t, srv, body); w.Code != http.StatusOK {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	if code := <-second; code != http.StatusOK {
		t.Fatalf("second: %d", code)
	}
	eng.mu.Lock()
	turns := eng.turns
	eng.mu.Unlock()
	if turns != 1 || st.createdConversations() != 1 {
		t.Fatalf("turns %d, conversations %d: the key ran more than once", turns, st.createdConversations())
	}
}

// A bind that committed but whose acknowledgement was lost still aborts the
// launch; the claim it left bound is settled cancelled (nothing ran), not left
// "running" to answer every resend of its key "already running".
func TestDirectClaim_BoundClaimOfAnAbortedLaunchIsSettled(t *testing.T) {
	shortDirectPauses(t)
	eng := &fakeEngine{}
	st := newFakeChatStore()
	st.bindLostAcks = directBindAttempts
	srv := newDefaultChatServer(t, eng, st)

	w := postChatRequest(t, srv, map[string]any{"message": "send the report", "persona": "generic", "input_id": "lost-ack-1"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	eng.mu.Lock()
	turns := eng.turns
	eng.mu.Unlock()
	if turns != 0 {
		t.Fatalf("the aborted launch ran (%d turns)", turns)
	}
	if rows := st.directRows(); len(rows) != 1 || rows[0].State != store.InputStateCancelled {
		t.Fatalf("claim = %+v, want one cancelled row", rows)
	}
}
