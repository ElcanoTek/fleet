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
	// bindLostAcks / claimLostAcks make that many binds / direct claims
	// commit and then report an error (the acknowledgement was lost).
	bindLostAcks, claimLostAcks int
	// committedTurns names the turns whose user entry committed (the input
	// ran), for CancelStoppedDrain's guard.
	committedTurns map[string]bool
	// stoppedDrainFailures makes that many CancelStoppedDrain calls fail.
	stoppedDrainFailures int
	// stopRequested lists MarkInputStopRequested calls (conv/key);
	// stopRequestFailures makes that many fail first.
	stopRequested       []string
	stopRequestFailures int
	// beforeCancelUnlaunched runs (under mu) as CancelUnlaunchedInput starts.
	beforeCancelUnlaunched func()
	// onClaim runs after a direct claim is stored; onMemories when turn
	// preparation reads memories (both outside the fake's lock).
	onClaim    func(store.InputQueueRow)
	onMemories func()
	// beforeClaim runs when a direct claim is about to be stored.
	beforeClaim func()
	// cancelKeyFailures makes that many CancelInputKey calls fail.
	cancelKeyFailures int
	// releaseLostAcks makes that many ReleaseDirectInput calls commit and
	// then report an error (the acknowledgement lost).
	releaseLostAcks int
	// onCreate runs on each CreateConversation call, before it creates.
	onCreate func()
	// claimAfterEnqueue marks each enqueued row claimed (running) at once.
	claimAfterEnqueue bool
	// memoriesErr makes ListMemories (turn preparation) fail.
	memoriesErr error
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
	if s.memoriesErr != nil {
		return nil, s.memoriesErr
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
func (s *fakeChatStore) SetArchived(_ context.Context, _, convID string, archived bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.convs[convID]; c != nil {
		if archived {
			now := time.Now().Unix()
			c.ArchivedAt = &now
		} else {
			c.ArchivedAt = nil
		}
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
	if s.claimAfterEnqueue {
		// A drain claims the row the moment it commits.
		s.queue[len(s.queue)-1].State = store.InputStateRunning
	}
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
	if before := s.beforeClaim; before != nil {
		s.mu.Unlock()
		before()
		s.mu.Lock()
	}
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
	lost := s.claimLostAcks > 0
	if lost {
		s.claimLostAcks--
	}
	s.mu.Unlock()
	if lost {
		return store.InputQueueRow{}, false, errors.New("fake: claim acknowledgement lost")
	}
	if hook != nil {
		hook(r)
	}
	return r, true, nil
}

func (s *fakeChatStore) ReleaseDirectInput(_ context.Context, id string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.releaseFailures > 0 {
		s.releaseFailures--
		return errors.New("fake: release failed")
	}
	lost := s.releaseLostAcks > 0
	if lost {
		s.releaseLostAcks--
		defer func() { err = errors.New("fake: release acknowledgement lost") }()
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

func (s *fakeChatStore) CancelInputKey(_ context.Context, r store.InputQueueRow) (store.InputQueueRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelKeyFailures > 0 {
		s.cancelKeyFailures--
		return store.InputQueueRow{}, false, errors.New("fake: cancel key failed")
	}
	for _, it := range s.queue {
		if it.ConversationID == r.ConversationID && it.ClientInputID == r.ClientInputID {
			return it, false, nil
		}
	}
	r.Mode, r.State = store.InputModeDirect, store.InputStateCancelled
	s.queue = append(s.queue, r)
	return r, true, nil
}

func (s *fakeChatStore) CancelStoppedSteer(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.queue {
		if s.queue[i].ID == id && (s.queue[i].State == store.InputStateInjected || s.queue[i].State == store.InputStateQueued) {
			s.queue[i].State = store.InputStateCancelled
			return true, nil
		}
	}
	return false, nil
}

// MarkInputStopRequested records the durable Stop intent; the fake's
// settlement never re-queues, so only the call itself is observable.
func (s *fakeChatStore) MarkInputStopRequested(_ context.Context, convID, clientID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopRequestFailures > 0 {
		s.stopRequestFailures--
		return errors.New("fake: mark stop requested failed")
	}
	s.stopRequested = append(s.stopRequested, convID+"/"+clientID)
	return nil
}

func (s *fakeChatStore) CancelStoppedDrain(_ context.Context, id, turnID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stoppedDrainFailures > 0 {
		s.stoppedDrainFailures--
		return false, errors.New("fake: cancel stopped drain failed")
	}
	for i := range s.queue {
		it := s.queue[i]
		if it.ID != id || it.Mode == store.InputModeDirect {
			continue
		}
		running := it.State == store.InputStateRunning
		if it.State == store.InputStateQueued || (running && strings.HasPrefix(it.TurnID, store.ClaimTurnPrefix)) ||
			(running && it.TurnID == turnID && !s.committedTurns[turnID]) {
			s.queue[i].State = store.InputStateCancelled
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeChatStore) CancelUnlaunchedInput(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.beforeCancelUnlaunched != nil {
		s.beforeCancelUnlaunched()
	}
	for i := range s.queue {
		it := s.queue[i]
		unbound := (it.Mode == store.InputModeDirect && it.TurnID == "") || strings.HasPrefix(it.TurnID, store.ClaimTurnPrefix)
		if it.ID == id && it.State == store.InputStateRunning && unbound {
			s.queue[i].State = store.InputStateCancelled
			return true, nil
		}
	}
	return false, nil
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

func (s *fakeChatStore) BindInputTurn(_ context.Context, id, turnID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindFailures > 0 {
		s.bindFailures--
		return false, errors.New("fake: bind failed")
	}
	bound := false
	for i := range s.queue {
		if s.queue[i].ID == id && (s.queue[i].State == store.InputStateRunning || s.queue[i].State == store.InputStateInjected) {
			s.queue[i].TurnID = turnID
			bound = true
		}
	}
	if s.bindLostAcks > 0 {
		s.bindLostAcks--
		return false, errors.New("fake: bind acknowledgement lost")
	}
	return bound, nil
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

// A claim whose release cannot be confirmed is settled "cancelled" (nothing
// ran), never deleted in the background: the delete may already have
// committed with its acknowledgement lost, and a concurrent resend may have
// been told the claim is running, so the key must keep a record. Both cases —
// the release really failed (the claim is still there), and it committed
// unacknowledged (the claim is gone) — leave the key held by a cancelled row.
func TestReleaseDirectInput_UnconfirmedReleaseSettlesCancelled(t *testing.T) {
	for _, lostAck := range []bool{false, true} {
		t.Run(map[bool]string{false: "release failed", true: "release committed, ack lost"}[lostAck], func(t *testing.T) {
			shortDirectPauses(t)
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, &fakeEngine{}, st)
			conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
			row, claimed, err := st.ClaimDirectInput(t.Context(), store.InputQueueRow{
				ID: "claim-1", ConversationID: conv.ID, UserEmail: "u@x.com", ClientInputID: "key-1",
			})
			if err != nil || !claimed {
				t.Fatalf("claim: %+v %v %v", row, claimed, err)
			}
			st.mu.Lock()
			if lostAck {
				st.releaseLostAcks = 1000
			} else {
				st.releaseFailures = 1000
			}
			st.mu.Unlock()

			if srv.releaseDirectInput("u@x.com", conv.ID, &directClaim{id: "claim-1", key: "key-1"}) {
				t.Fatal("an unconfirmed release was reported as released")
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				got, _ := st.LookupInput(context.Background(), conv.ID, "key-1")
				if got != nil && got.State == store.InputStateCancelled {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("key row = %+v, want it held by a cancelled row", got)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
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
	if rows := st.directRows(); len(rows) != 1 || rows[0].State != store.InputStateCancelled {
		t.Fatalf("claim = %+v, want it settled cancelled (did not run), never left running", rows)
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
		st.onMemories = func() { srv.stopInput(context.Background(), "u@x.com", convID, "key-p") }
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
		srv.stopInput(context.Background(), "u@x.com", conv.ID, "key-t")
		w := postChatRequest(t, srv, map[string]any{"message": "send the report", "conversation_id": conv.ID, "input_id": "key-t"})
		if !strings.Contains(w.Body.String(), `"state":"cancelled"`) || eng.turns != 0 {
			t.Fatalf("status %d %s, turns %d: a Stop that arrived first must refuse the late submission", w.Code, w.Body.String(), eng.turns)
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
		srv.stopInput(context.Background(), "u@x.com", "conv-1", "key-r")
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
	body := map[string]any{"message": "send the report", "persona": "generic", "input_id": "first-1", "input_id_scope": "user"}
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

// A claim that committed but reported an error (its acknowledgement lost) runs
// no turn, so it is settled "did not run" rather than left "running" to answer
// every resend of its key "already running".
func TestDirectClaim_LostClaimAckIsSettled(t *testing.T) {
	eng := &fakeEngine{}
	st := newFakeChatStore()
	st.claimLostAcks = 1
	srv := newDefaultChatServer(t, eng, st)
	w := postChatRequest(t, srv, map[string]any{"message": "send the report", "persona": "generic", "input_id": "claim-lost-1"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if rows := st.directRows(); len(rows) != 1 || rows[0].State != store.InputStateCancelled {
		t.Fatalf("claim = %+v, want it settled cancelled", rows)
	}
	// The resend is told the input did not run (so a fresh key may be sent),
	// not "already running".
	w = postChatRequest(t, srv, map[string]any{"message": "send the report", "conversation_id": "conv-1", "input_id": "claim-lost-1"})
	if !strings.Contains(w.Body.String(), `"state":"cancelled"`) || eng.turns != 0 {
		t.Fatalf("resend: %d %s, turns %d", w.Code, w.Body.String(), eng.turns)
	}
}

// Without input_id_scope "user", keys stay conversation-scoped as documented:
// a client that numbers keys per conversation reusing "1" for a new
// conversation starts that conversation, not another one's replay.
func TestFirstSubmission_KeysStayConversationScopedByDefault(t *testing.T) {
	eng := &fakeEngine{}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, eng, st)
	for i := range 2 {
		w := postChatRequest(t, srv, map[string]any{"message": "hello", "persona": "generic", "input_id": "1"})
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"queued":true`) {
			t.Fatalf("submission %d: %d %.200s — want a new conversation's turn, not a replay", i, w.Code, w.Body.String())
		}
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if eng.turns != 2 || st.createdConversations() != 2 {
		t.Fatalf("turns %d, conversations %d: want two conversations, each run", eng.turns, st.createdConversations())
	}
}

// A Stop by key that lands before its queued row exists leaves only the
// in-memory mark; the row is withdrawn when it is inserted, so it cannot
// outwait the mark behind a long turn and run after the confirmed Stop.
func TestCancelByInputKey_LateQueuedRowIsWithdrawn(t *testing.T) {
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
	_, _, tok, _ := srv.registerTurn(conv.ID, func() {}) // another surface's long turn
	defer srv.finishTurn(conv.ID, tok)
	srv.stopInput(context.Background(), "u@x.com", conv.ID, "late-1")

	w := postChatRequest(t, srv, map[string]any{"message": "later", "conversation_id": conv.ID, "input_id": "late-1"})
	if !strings.Contains(w.Body.String(), `"state":"cancelled"`) {
		t.Fatalf("ack %d %s: want the late row withdrawn", w.Code, w.Body.String())
	}
	row, _ := st.LookupInput(context.Background(), conv.ID, "late-1")
	if row == nil || row.State != store.InputStateCancelled {
		t.Fatalf("row = %+v, want cancelled", row)
	}
}

// A Stop by key whose queued row a drain claimed between the insert and the
// withdrawal still cancels it durably, so the drain's bind finds it no longer
// running and does not launch it (the in-memory mark alone expires).
func TestCancelByInputKey_LateRowClaimedByADrainIsCancelled(t *testing.T) {
	st := newFakeChatStore()
	st.claimAfterEnqueue = true
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
	_, _, tok, _ := srv.registerTurn(conv.ID, func() {})
	defer srv.finishTurn(conv.ID, tok)
	srv.stopInput(context.Background(), "u@x.com", conv.ID, "late-2")

	w := postChatRequest(t, srv, map[string]any{"message": "later", "conversation_id": conv.ID, "input_id": "late-2"})
	if !strings.Contains(w.Body.String(), `"state":"cancelled"`) {
		t.Fatalf("ack %d %s: want the claimed late row cancelled", w.Code, w.Body.String())
	}
	if row, _ := st.LookupInput(context.Background(), conv.ID, "late-2"); row == nil || row.State != store.InputStateCancelled {
		t.Fatalf("row = %+v, want cancelled", row)
	}
}

// A drained row cancelled after its claim is not launched: the bind finds it
// no longer running.
func TestQueuedLaunch_CancelledRowIsNotLaunched(t *testing.T) {
	eng := &fakeEngine{}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, eng, st)
	conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
	st.mu.Lock()
	st.queue = append(st.queue, store.InputQueueRow{ID: "r-c", ConversationID: conv.ID, UserEmail: "u@x.com", ClientInputID: "k", Mode: store.InputModeQueued, State: store.InputStateCancelled, TurnID: "placeholder"})
	st.mu.Unlock()
	gen, _ := srv.stopGateForRow(conv.ID, 0)
	var released atomic.Bool
	ok := srv.startTurn(nil, nil, "u@x.com", conv, chatRequest{ConversationID: conv.ID, Message: "later"},
		&queuedLaunch{rowID: "r-c", claimTurnID: "placeholder", sweepGen: gen, inputKey: "k"}, func() { released.Store(true) }, nil)
	if !ok {
		t.Fatal("startTurn reported a lost registration race")
	}
	time.Sleep(50 * time.Millisecond)
	eng.mu.Lock()
	turns := eng.turns
	eng.mu.Unlock()
	if turns != 0 || !released.Load() {
		t.Fatalf("turns %d, slot released %v: a cancelled row must not launch", turns, released.Load())
	}
}

// input_id is indexed, so an oversized one is refused up front instead of
// failing as a database error mid-submission.
func TestChat_OversizedInputIDIsRefused(t *testing.T) {
	eng := &fakeEngine{}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, eng, st)
	w := postChatRequest(t, srv, map[string]any{"message": "hi", "persona": "generic", "input_id": strings.Repeat("k", maxInputIDLen+1)})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if eng.turns != 0 || len(st.directRows()) != 0 || st.createdConversations() != 0 {
		t.Fatalf("an oversized key reached the store: turns %d, rows %d, convs %d", eng.turns, len(st.directRows()), st.createdConversations())
	}
	if w := postChatRequest(t, srv, map[string]any{"message": "hi", "persona": "generic", "input_id": strings.Repeat("k", maxInputIDLen)}); w.Code != http.StatusOK {
		t.Fatalf("a key at the limit was refused: %d", w.Code)
	}
}

// A claim whose turn fails in preparation is settled "did not run", not
// deleted: a concurrent resend may already have been told it is running, and
// its next look must find an outcome rather than a missing row.
func TestDirectClaim_PrepFailureLeavesAnOutcome(t *testing.T) {
	st := newFakeChatStore()
	st.memoriesErr = errors.New("fake: memories unavailable")
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	if w := postChatRequest(t, srv, map[string]any{"message": "send the report", "persona": "generic", "input_id": "prep-1"}); w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if rows := st.directRows(); len(rows) != 1 || rows[0].State != store.InputStateCancelled {
		t.Fatalf("claim = %+v, want it settled cancelled", rows)
	}
}

// Stop-by-key marks are bounded however many distinct keys are stopped, and an
// oversized key is refused at the endpoint.
func TestCancelByInputKey_MarksAreBounded(t *testing.T) {
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	for i := range maxCancelledInputs + 100 {
		srv.cancelInputTurn("conv-x", fmt.Sprintf("k-%d", i))
	}
	srv.inflightMu.Lock()
	n := len(srv.cancelledInputs)
	srv.inflightMu.Unlock()
	if n > maxCancelledInputs {
		t.Fatalf("marks = %d, want at most %d", n, maxCancelledInputs)
	}
	conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
	raw, _ := json.Marshal(map[string]any{"scope": "turn", "input_id": strings.Repeat("k", maxInputIDLen+1)})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/conversations/"+conv.ID+"/cancel", bytes.NewReader(raw))
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", "u@x.com")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized cancel key: %d %s", w.Code, w.Body.String())
	}
}

// A Stop by key that lands while a direct claim is being prepared cancels the
// claim durably, so the launch is refused (409) even if the in-memory mark is
// gone by the time the turn registers — evicted by many other Stops, or past
// its TTL.
func TestCancelByInputKey_UnboundClaimIsCancelledDurably(t *testing.T) {
	eng := &fakeEngine{}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, eng, st)
	var convID string
	st.onClaim = func(r store.InputQueueRow) { convID = r.ConversationID }
	st.onMemories = func() {
		srv.stopInput(context.Background(), "u@x.com", convID, "key-e")
		srv.inflightMu.Lock()
		srv.cancelledInputs = nil // the mark is evicted before the turn registers
		srv.inflightMu.Unlock()
	}
	w := postChatRequest(t, srv, map[string]any{"message": "send the report", "persona": "generic", "input_id": "key-e"})
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
}

// A direct launch dropped after its turn registered — its claim cancelled by
// a Stop, or its bind failing closed — finishes a registered turn that never
// runs, so it re-kicks the queue itself: an input queued behind it deferred
// to that turn, and no completion tail will drain it.
func TestDirectLaunchDroppedAfterRegistration_RekicksTheQueue(t *testing.T) {
	for _, stopped := range []bool{true, false} {
		t.Run(map[bool]string{true: "stopped", false: "bind failed"}[stopped], func(t *testing.T) {
			shortDirectPauses(t)
			eng := &fakeEngine{}
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, eng, st)
			var convID string
			var once sync.Once
			st.onClaim = func(r store.InputQueueRow) { convID = r.ConversationID }
			st.onMemories = func() {
				once.Do(func() { // the first launch only
					st.mu.Lock()
					defer st.mu.Unlock()
					for i := range st.queue {
						if st.queue[i].ClientInputID == "key-x" && stopped {
							st.queue[i].State = store.InputStateCancelled // a Stop cancelled the claim
						}
					}
					if !stopped {
						st.bindFailures = directBindAttempts
					}
					st.queue = append(st.queue, store.InputQueueRow{ID: "r-next", ConversationID: convID, UserEmail: "u@x.com", ClientInputID: "key-next", Message: "next", Attachments: "[]", Mode: store.InputModeQueued, State: store.InputStateQueued, AcceptedSeq: 1 << 40})
				})
			}
			w := postChatRequest(t, srv, map[string]any{"message": "send the report", "persona": "generic", "input_id": "key-x"})
			want := http.StatusConflict
			if !stopped {
				want = http.StatusInternalServerError
			}
			if w.Code != want {
				t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body.String())
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				eng.mu.Lock()
				turns := eng.turns
				eng.mu.Unlock()
				if turns == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the input queued behind the dropped launch did not run (%d turns)", turns)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// A Stop by key never marks a bound claim cancelled: its turn may have run,
// and a resend of the key reading "cancelled" would run it a second time.
func TestCancelByInputKey_BoundClaimIsNotMarkedCancelled(t *testing.T) {
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
	st.mu.Lock()
	st.queue = append(st.queue, store.InputQueueRow{ID: "d-b", ConversationID: conv.ID, UserEmail: "u@x.com", ClientInputID: "key-b", Mode: store.InputModeDirect, State: store.InputStateRunning, TurnID: "turn-ran"})
	st.mu.Unlock()
	if _, ok := srv.stopInput(context.Background(), "u@x.com", conv.ID, "key-b"); !ok {
		t.Fatal("stopInput reported a store failure")
	}
	if row, _ := st.LookupInput(context.Background(), conv.ID, "key-b"); row == nil || row.State != store.InputStateRunning {
		t.Fatalf("row = %+v, want the bound claim left to its turn's settlement", row)
	}
}

// A Stop by key that lands after a drain claimed the row but before its turn
// registered cancels the row durably, so the launch is refused even if the
// in-memory mark is gone by then.
func TestCancelByInputKey_DrainedRowIsCancelledDurably(t *testing.T) {
	eng := &fakeEngine{}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, eng, st)
	conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
	claim := store.ClaimTurnPrefix + "drain-1"
	st.mu.Lock()
	st.queue = append(st.queue, store.InputQueueRow{ID: "r-d", ConversationID: conv.ID, UserEmail: "u@x.com", ClientInputID: "key-d", Mode: store.InputModeQueued, State: store.InputStateRunning, TurnID: claim})
	st.mu.Unlock()
	gen, _ := srv.stopGateForRow(conv.ID, 0)
	if _, ok := srv.stopInput(context.Background(), "u@x.com", conv.ID, "key-d"); !ok {
		t.Fatal("stopInput reported a store failure")
	}
	srv.inflightMu.Lock()
	srv.cancelledInputs = nil // the mark is evicted before the turn registers
	srv.inflightMu.Unlock()
	var released atomic.Bool
	srv.startTurn(nil, nil, "u@x.com", conv, chatRequest{ConversationID: conv.ID, Message: "later"},
		&queuedLaunch{rowID: "r-d", claimTurnID: claim, sweepGen: gen, inputKey: "key-d"}, func() { released.Store(true) }, nil)
	time.Sleep(50 * time.Millisecond)
	eng.mu.Lock()
	turns := eng.turns
	eng.mu.Unlock()
	if turns != 0 {
		t.Fatalf("a stopped drained row ran (%d turns)", turns)
	}
	if row, _ := st.LookupInput(context.Background(), conv.ID, "key-d"); row == nil || row.State != store.InputStateCancelled {
		t.Fatalf("row = %+v, want cancelled", row)
	}
}

// A drained launch refused at its bind (a Stop cancelled the row) finishes
// a registered turn that never runs, so no completion tail drains the queue:
// the launch re-kicks it itself, or an input queued behind the refused one —
// which deferred to that registered turn — would wait for an unrelated kick.
func TestCancelByInputKey_RefusedDrainRekicksTheQueue(t *testing.T) {
	eng := &fakeEngine{}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, eng, st)
	conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
	claim := store.ClaimTurnPrefix + "drain-1"
	st.mu.Lock()
	st.queue = append(st.queue,
		store.InputQueueRow{ID: "r-d", ConversationID: conv.ID, UserEmail: "u@x.com", ClientInputID: "key-d", Mode: store.InputModeQueued, State: store.InputStateCancelled, TurnID: claim},
		store.InputQueueRow{ID: "r-next", ConversationID: conv.ID, UserEmail: "u@x.com", ClientInputID: "key-next", Message: "next", Attachments: "[]", Mode: store.InputModeQueued, State: store.InputStateQueued, AcceptedSeq: 1})
	st.mu.Unlock()
	gen, _ := srv.stopGateForRow(conv.ID, 0)
	srv.startTurn(nil, nil, "u@x.com", conv, chatRequest{ConversationID: conv.ID, Message: "later"},
		&queuedLaunch{rowID: "r-d", claimTurnID: claim, sweepGen: gen, inputKey: "key-d"}, func() {}, nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		eng.mu.Lock()
		turns := eng.turns
		eng.mu.Unlock()
		if turns == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the input queued behind the refused launch did not run (%d turns)", turns)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if row, _ := st.LookupInput(context.Background(), conv.ID, "key-d"); row == nil || row.State != store.InputStateCancelled {
		t.Fatalf("refused row = %+v, want cancelled", row)
	}
}

// A Stop by key that lands before its submission holds the key takes the key
// with a cancelled row, so the late submission is answered "cancelled" and
// never runs even when the in-memory mark was evicted in between — whether
// it arrives on an idle conversation or behind another surface's turn.
func TestCancelByInputKey_KeyIsTakenBeforeTheSubmission(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "busy"}[busy], func(t *testing.T) {
			eng := &fakeEngine{}
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, eng, st)
			conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
			if busy {
				_, _, tok, _ := srv.registerTurn(conv.ID, func() {})
				defer srv.finishTurn(conv.ID, tok)
			}
			if _, ok := srv.stopInput(context.Background(), "u@x.com", conv.ID, "key-x"); !ok {
				t.Fatal("stopInput reported a store failure")
			}
			srv.inflightMu.Lock()
			srv.cancelledInputs = nil // evicted before the submission lands
			srv.inflightMu.Unlock()
			w := postChatRequest(t, srv, map[string]any{"message": "send the report", "conversation_id": conv.ID, "input_id": "key-x"})
			if !strings.Contains(w.Body.String(), `"state":"cancelled"`) {
				t.Fatalf("ack %d %s: want the late submission answered cancelled", w.Code, w.Body.String())
			}
			eng.mu.Lock()
			turns := eng.turns
			eng.mu.Unlock()
			if turns != 0 {
				t.Fatalf("a stopped input ran (%d turns)", turns)
			}
			if row, _ := st.LookupInput(context.Background(), conv.ID, "key-x"); row == nil || row.State != store.InputStateCancelled {
				t.Fatalf("row = %+v, want the key held cancelled", row)
			}
		})
	}
}

// A resend of an accepted input is answered before the request touches the
// conversation: it does not roll the model back to the original request's,
// nor un-archive a conversation archived since.
func TestReplay_DoesNotMutateTheConversation(t *testing.T) {
	eng := &fakeEngine{}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, eng, st)
	conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "model/y", false)
	archived := time.Now().Unix()
	st.mu.Lock()
	conv.ArchivedAt = &archived
	st.queue = append(st.queue, store.InputQueueRow{ID: "d-k", ConversationID: conv.ID, UserEmail: "u@x.com", ClientInputID: "key-k", Mode: store.InputModeDirect, State: store.InputStateCompleted, TurnID: "t-1"})
	st.mu.Unlock()

	w := postChatRequest(t, srv, map[string]any{"message": "send the report", "conversation_id": conv.ID, "model": "model/x", "input_id": "key-k"})
	if !strings.Contains(w.Body.String(), `"state":"completed"`) {
		t.Fatalf("ack %d %s: want the replay", w.Code, w.Body.String())
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if got := st.convs[conv.ID]; got.Model != "model/y" || got.ArchivedAt == nil || eng.turns != 0 {
		t.Fatalf("model %q archived %v turns %d: a replay must leave the conversation alone", got.Model, got.ArchivedAt != nil, eng.turns)
	}
}

// A caller declaring user-unique keys finds its accepted input whichever
// conversation accepted it: a resend posted to another conversation is
// answered with that input, never run there a second time.
func TestReplay_UserScopedKeyIsFoundAcrossConversations(t *testing.T) {
	for _, scope := range []string{"user", ""} {
		t.Run("scope="+scope, func(t *testing.T) {
			eng := &fakeEngine{}
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, eng, st)
			a, _ := st.CreateConversation(context.Background(), "u@x.com", "a", "generic", "", false)
			b, _ := st.CreateConversation(context.Background(), "u@x.com", "b", "generic", "", false)
			st.mu.Lock()
			st.queue = append(st.queue, store.InputQueueRow{ID: "d-u", ConversationID: a.ID, UserEmail: "u@x.com", ClientInputID: "key-u", Mode: store.InputModeDirect, State: store.InputStateCompleted, TurnID: "t-1"})
			st.mu.Unlock()
			w := postChatRequest(t, srv, map[string]any{"message": "send the report", "conversation_id": b.ID, "input_id": "key-u", "input_id_scope": scope})
			eng.mu.Lock()
			turns := eng.turns
			eng.mu.Unlock()
			if scope == "user" {
				if turns != 0 || !strings.Contains(w.Body.String(), `"conversation_id":"`+a.ID+`"`) {
					t.Fatalf("ack %d %s, turns %d: want the input found in its own conversation", w.Code, w.Body.String(), turns)
				}
				return
			}
			if turns != 1 {
				t.Fatalf("turns %d: a conversation-scoped key is new in another conversation", turns)
			}
		})
	}
}

// A direct claim released for the queue (it lost the race to another turn)
// and then refused by it is settled "cancelled", not left without a row: a
// concurrent resend already told "running" finds an outcome.
func TestDirectClaim_RaceLoserRefusedByTheQueueIsSettled(t *testing.T) {
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	st.onClaim = func(r store.InputQueueRow) {
		srv.registerTurn(r.ConversationID, func() {}) // another surface's turn wins the race
		st.mu.Lock()
		for i := range maxPendingInputs { // and the queue is full
			st.queue = append(st.queue, store.InputQueueRow{ID: fmt.Sprintf("q-%d", i), ConversationID: r.ConversationID, UserEmail: "u@x.com", ClientInputID: fmt.Sprintf("k-%d", i), Mode: store.InputModeQueued, State: store.InputStateQueued})
		}
		st.mu.Unlock()
	}
	w := postChatRequest(t, srv, map[string]any{"message": "send the report", "persona": "generic", "input_id": "race-q"})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	st.mu.Lock()
	var row *store.InputQueueRow
	for i := range st.queue {
		if st.queue[i].ClientInputID == "race-q" {
			row = &st.queue[i]
		}
	}
	st.mu.Unlock()
	if row == nil || row.State != store.InputStateCancelled {
		t.Fatalf("key row = %+v, want it settled cancelled", row)
	}
}

// A targeted Stop reports whether it stopped anything: 204 when the named
// turn was running and is cancelled, 409 when it had already ended, so a
// caller that watched the turn reads its real outcome instead of calling it
// cancelled.
func TestCancelByTurnID_ReportsANoOp(t *testing.T) {
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
	var cancelled atomic.Bool
	var buf *turnBuffer
	buf, turnID, tok, _ := srv.registerTurn(conv.ID, func() {
		cancelled.Store(true)
		buf.Emit("turn.cancelled", map[string]any{}) // a real turn reports its stop
	})
	defer srv.finishTurn(conv.ID, tok)
	cancel := func(turn string) int {
		raw, _ := json.Marshal(map[string]any{"scope": "turn", "turn_id": turn})
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/conversations/"+conv.ID+"/cancel", bytes.NewReader(raw))
		req.Header.Set("X-Chat-Server-Token", "tok")
		req.Header.Set("X-User-Email", "u@x.com")
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, req)
		return w.Code
	}
	if code := cancel("turn-that-ended"); code != http.StatusConflict || cancelled.Load() {
		t.Fatalf("stop of an ended turn: %d (cancelled=%v), want 409 and nothing stopped", code, cancelled.Load())
	}
	if code := cancel(turnID); code != http.StatusNoContent || !cancelled.Load() {
		t.Fatalf("stop of the running turn: %d (cancelled=%v), want 204", code, cancelled.Load())
	}
}

// Two concurrent sends of one user-unique key into different conversations
// are serialized: the second finds the first one's claim and is answered
// with it, rather than both claiming the key (the database's uniqueness is
// per conversation) and running the input twice.
func TestUserScopedKey_ConcurrentSendsIntoTwoConversationsRunOnce(t *testing.T) {
	eng := &fakeEngine{}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, eng, st)
	a, _ := st.CreateConversation(context.Background(), "u@x.com", "a", "generic", "", false)
	b, _ := st.CreateConversation(context.Background(), "u@x.com", "b", "generic", "", false)
	body := func(conv string) map[string]any {
		return map[string]any{"message": "send the report", "conversation_id": conv, "input_id": "user-k", "input_id_scope": "user"}
	}
	var once atomic.Bool
	second := make(chan *httptest.ResponseRecorder, 1)
	st.beforeClaim = func() {
		if !once.CompareAndSwap(false, true) {
			return
		}
		// The first send has done its lookup and is about to claim: the
		// second send starts now, into the other conversation.
		go func() { second <- postChatRequest(t, srv, body(b.ID)) }()
		time.Sleep(100 * time.Millisecond)
	}
	postChatRequest(t, srv, body(a.ID))
	w := <-second
	eng.mu.Lock()
	turns := eng.turns
	eng.mu.Unlock()
	if turns != 1 || !strings.Contains(w.Body.String(), `"conversation_id":"`+a.ID+`"`) {
		t.Fatalf("turns %d, second answer %d %s: want one run, the second answered with the first", turns, w.Code, w.Body.String())
	}
}

// A Stop by key reports an input that had already finished (409, nothing was
// stopped): a completed row, or a row bound to a turn that is no longer
// registered (it ended; settlement pending). One it did stop is a 204.
func TestCancelByInputKey_ReportsAFinishedInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  store.InputQueueRow
		want int
	}{
		{"completed", store.InputQueueRow{Mode: store.InputModeDirect, State: store.InputStateCompleted, TurnID: "t-1"}, http.StatusConflict},
		{"bound, turn ended", store.InputQueueRow{Mode: store.InputModeDirect, State: store.InputStateRunning, TurnID: "t-1"}, http.StatusConflict},
		{"queued", store.InputQueueRow{Mode: store.InputModeQueued, State: store.InputStateQueued}, http.StatusNoContent},
		{"never ran", store.InputQueueRow{Mode: store.InputModeDirect, State: store.InputStateCancelled}, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, &fakeEngine{}, st)
			conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
			tc.row.ID, tc.row.ConversationID, tc.row.UserEmail, tc.row.ClientInputID = "r-1", conv.ID, "u@x.com", "key-f"
			st.mu.Lock()
			st.queue = append(st.queue, tc.row)
			st.mu.Unlock()
			raw, _ := json.Marshal(map[string]any{"scope": "turn", "input_id": "key-f"})
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/conversations/"+conv.ID+"/cancel", bytes.NewReader(raw))
			req.Header.Set("X-Chat-Server-Token", "tok")
			req.Header.Set("X-User-Email", "u@x.com")
			w := httptest.NewRecorder()
			srv.Routes().ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status %d (%s), want %d", w.Code, w.Body.String(), tc.want)
			}
		})
	}
}

// The settlement of a race loser the queue refused is retried: a failed
// write must not leave the acknowledged key with no row.
func TestDirectClaim_RefusedRaceLoserSettlementIsRetried(t *testing.T) {
	shortDirectPauses(t)
	st := newFakeChatStore()
	st.cancelKeyFailures = 2
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	st.onClaim = func(r store.InputQueueRow) {
		srv.registerTurn(r.ConversationID, func() {})
		st.mu.Lock()
		for i := range maxPendingInputs {
			st.queue = append(st.queue, store.InputQueueRow{ID: fmt.Sprintf("q-%d", i), ConversationID: r.ConversationID, UserEmail: "u@x.com", ClientInputID: fmt.Sprintf("k-%d", i), Mode: store.InputModeQueued, State: store.InputStateQueued})
		}
		st.mu.Unlock()
	}
	if w := postChatRequest(t, srv, map[string]any{"message": "send the report", "persona": "generic", "input_id": "race-r"}); w.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st.mu.Lock()
		var state string
		for _, it := range st.queue {
			if it.ClientInputID == "race-r" {
				state = it.State
			}
		}
		st.mu.Unlock()
		if state == store.InputStateCancelled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("key state = %q, want it settled cancelled after the retries", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A Stop by key that finds the input already finished leaves no mark behind:
// once the finished row is purged, a later submission reusing the key is new
// and must run, not be cancelled by a stale mark.
func TestCancelByInputKey_FinishedInputLeavesNoMark(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  inputStop
	}{
		{store.InputStateCompleted, inputFinished},
		{store.InputStateCancelled, inputStopped}, // already cancelled: the row refuses it
	} {
		t.Run(tc.state, func(t *testing.T) {
			eng := &fakeEngine{}
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, eng, st)
			conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
			st.mu.Lock()
			st.queue = append(st.queue, store.InputQueueRow{ID: "r-done", ConversationID: conv.ID, UserEmail: "u@x.com", ClientInputID: "key-old", Mode: store.InputModeDirect, State: tc.state, TurnID: "t-1"})
			st.mu.Unlock()
			if res, ok := srv.stopInput(context.Background(), "u@x.com", conv.ID, "key-old"); res != tc.want || !ok {
				t.Fatalf("stopInput = %v ok %v, want %v", res, ok, tc.want)
			}
			st.mu.Lock()
			st.queue = nil // retention purges the terminal row
			st.mu.Unlock()
			w := postChatRequest(t, srv, map[string]any{"message": "again", "conversation_id": conv.ID, "input_id": "key-old"})
			eng.mu.Lock()
			turns := eng.turns
			eng.mu.Unlock()
			if w.Code != http.StatusOK || turns != 1 {
				t.Fatalf("status %d, turns %d: a reuse of the key after its row was purged must run", w.Code, turns)
			}
		})
	}
}

// A Stop by key that reads an unbound direct claim which is then released
// (the registration loser re-queueing it) before the guarded cancel lands
// reads the key again rather than infer the claim was bound to an ended turn:
// it takes the freed key with a cancelled row and keeps the mark, so the
// loser's re-queued copy is refused. A key whose row keeps changing is
// reported unconfirmed, the mark still in force.
func TestCancelByInputKey_ReleasedClaimIsReadAgain(t *testing.T) {
	for _, tc := range []struct {
		name           string
		drained, churn bool
	}{
		{"direct claim released", false, false},
		{"drained claim un-claimed", true, false},
		{"keeps changing", false, true},
	} {
		churn := tc.churn
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, &fakeEngine{}, st)
			conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
			claim := func(id string) store.InputQueueRow {
				r := store.InputQueueRow{ID: id, ConversationID: conv.ID, UserEmail: "u@x.com", ClientInputID: "key-r", Mode: store.InputModeDirect, State: store.InputStateRunning}
				if tc.drained {
					r.Mode, r.TurnID = store.InputModeQueued, store.ClaimTurnPrefix+id
				}
				return r
			}
			n := 0
			st.mu.Lock()
			st.queue = append(st.queue, claim("c-0"))
			st.beforeCancelUnlaunched = func() { // runs under st.mu
				n++
				if tc.drained {
					st.queue[0].State, st.queue[0].TurnID = store.InputStateQueued, "" // the lost race un-claims it
					return
				}
				st.queue = nil // released
				if churn {
					st.queue = append(st.queue, claim(fmt.Sprintf("c-%d", n))) // and claimed afresh
				}
			}
			st.mu.Unlock()
			res, ok := srv.stopInput(context.Background(), "u@x.com", conv.ID, "key-r")
			want := inputStopped
			if churn {
				want = inputStopUnconfirmed
			}
			if !ok || res != want {
				t.Fatalf("stopInput = %v ok %v, want %v", res, ok, want)
			}
			if !srv.inputKeyStopped(conv.ID, "key-r") {
				t.Fatal("the mark was cleared: the loser's re-queued copy would run")
			}
			if !churn {
				if row, _ := st.LookupInput(context.Background(), conv.ID, "key-r"); row == nil || row.State != store.InputStateCancelled {
					t.Fatalf("row = %+v, want the key's row cancelled (a freed key taken, a re-queued row withdrawn)", row)
				}
			}
		})
	}
}

// A Stop by key of a steer already injected into a turn stops that turn if it
// is still running (204, the row cancelled so settlement cannot re-queue it);
// if the turn had already ended, nothing was stopped (409) and the row is
// left to the turn's own settlement to record whether the steer ran.
func TestCancelByInputKey_InjectedSteer(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(map[bool]string{true: "turn running", false: "turn already ended"}[running], func(t *testing.T) {
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, &fakeEngine{}, st)
			conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
			turnID := "turn-gone"
			var stopped atomic.Bool
			if running {
				var tok uint64
				var buf *turnBuffer
				buf, turnID, tok, _ = srv.registerTurn(conv.ID, func() {
					stopped.Store(true)
					buf.Emit("turn.cancelled", map[string]any{}) // the cancel reaches it mid-run
				})
				defer srv.finishTurn(conv.ID, tok)
			}
			st.mu.Lock()
			st.queue = append(st.queue, store.InputQueueRow{ID: "s-1", ConversationID: conv.ID, UserEmail: "u@x.com", ClientInputID: "steer-k", Mode: store.InputModeSteer, State: store.InputStateInjected, TurnID: turnID})
			st.mu.Unlock()
			res, ok := srv.stopInput(context.Background(), "u@x.com", conv.ID, "steer-k")
			finished := res == inputFinished
			row, _ := st.LookupInput(context.Background(), conv.ID, "steer-k")
			if !ok || finished == running || stopped.Load() != running {
				t.Fatalf("result %v ok %v stopped %v; want finished=%v", res, ok, stopped.Load(), !running)
			}
			want := store.InputStateCancelled
			if !running {
				want = store.InputStateInjected // untouched: the settlement records the outcome
			}
			if row == nil || row.State != want {
				t.Fatalf("row = %+v, want state %s", row, want)
			}
		})
	}
}

// A Stop by key of a drained row already bound to its running turn cancels
// the row durably once the turn confirms the stop — or the turn's settlement,
// finding no committed user entry, would return it to the queue and a later
// drain would run the stopped input. The row is left to the settlement when
// the input ran (its user entry committed), when the turn completed anyway,
// and when the stop is unconfirmed.
func TestCancelByInputKey_StoppedDrainIsNeverRequeued(t *testing.T) {
	for _, tc := range []struct {
		name      string
		frame     string // "" = no terminal frame in time
		committed bool
		want      string
		res       inputStop
	}{
		{"stopped before the user entry", "turn.cancelled", false, store.InputStateCancelled, inputStopped},
		{"stopped after the input ran", "turn.cancelled", true, store.InputStateRunning, inputStopped},
		{"completed anyway", "turn.completed", false, store.InputStateRunning, inputFinished},
		{"unconfirmed", "", false, store.InputStateRunning, inputStopUnconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, &fakeEngine{}, st)
			conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
			var buf *turnBuffer
			buf, turnID, tok, _ := srv.registerTurn(conv.ID, func() {
				if tc.frame != "" {
					buf.Emit(tc.frame, map[string]any{})
				}
			})
			defer srv.finishTurn(conv.ID, tok)
			srv.inflightMu.Lock()
			e := srv.inflight[conv.ID]
			e.inputKey = "key-dr"
			srv.inflight[conv.ID] = e
			srv.inflightMu.Unlock()
			st.mu.Lock()
			st.committedTurns = map[string]bool{turnID: tc.committed}
			st.queue = append(st.queue, store.InputQueueRow{ID: "r-dr", ConversationID: conv.ID, UserEmail: "u@x.com", ClientInputID: "key-dr", Mode: store.InputModeQueued, State: store.InputStateRunning, TurnID: turnID})
			st.mu.Unlock()
			res, ok := srv.stopInput(context.Background(), "u@x.com", conv.ID, "key-dr")
			if !ok || res != tc.res {
				t.Fatalf("stopInput = %v, %v; want %v", res, ok, tc.res)
			}
			if row, _ := st.LookupInput(context.Background(), conv.ID, "key-dr"); row == nil || row.State != tc.want {
				t.Fatalf("row = %+v, want state %s", row, tc.want)
			}
		})
	}
	t.Run("store failure", func(t *testing.T) {
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, &fakeEngine{}, st)
		conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
		var buf *turnBuffer
		buf, turnID, tok, _ := srv.registerTurn(conv.ID, func() { buf.Emit("turn.cancelled", map[string]any{}) })
		defer srv.finishTurn(conv.ID, tok)
		srv.inflightMu.Lock()
		e := srv.inflight[conv.ID]
		e.inputKey = "key-df"
		srv.inflight[conv.ID] = e
		srv.inflightMu.Unlock()
		st.mu.Lock()
		st.stoppedDrainFailures = 1
		st.queue = append(st.queue, store.InputQueueRow{ID: "r-df", ConversationID: conv.ID, UserEmail: "u@x.com", ClientInputID: "key-df", Mode: store.InputModeQueued, State: store.InputStateRunning, TurnID: turnID})
		st.mu.Unlock()
		if _, ok := srv.stopInput(context.Background(), "u@x.com", conv.ID, "key-df"); ok {
			t.Fatal("a failed cancel of the stopped drain was reported ok")
		}
	})
}

// A Stop by key of an injected steer records the steer on the turn it
// cancels — so the settlement cancels it rather than re-queue it even when
// the stop is unconfirmed — and never on a turn that already ended: that Stop
// stopped nothing, and the settlement alone records whether the steer ran.
func TestCancelSteerTurn_FlagsOnlyATurnItCancels(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(map[bool]string{true: "running", false: "already ended"}[running], func(t *testing.T) {
			srv := newDefaultChatServer(t, &fakeEngine{}, newFakeChatStore())
			var buf *turnBuffer
			buf, turnID, tok, _ := srv.registerTurn("conv-s", func() {}) // no terminal frame: unconfirmed
			defer srv.finishTurn("conv-s", tok)
			want := turnStopUnconfirmed
			if !running {
				buf.Finish()
				want = turnNotStopped
			}
			if got := srv.cancelSteerTurn("conv-s", turnID, "steer-row"); got != want {
				t.Fatalf("cancelSteerTurn = %v, want %v", got, want)
			}
			if got := buf.stoppedSteerIDs(); running != (len(got) == 1 && got[0] == "steer-row") || (!running && len(got) != 0) {
				t.Fatalf("stopped steers = %v, want flagged=%v", got, running)
			}
		})
	}
}

// A turn that already emitted a terminal frame of its own — turn.error or
// turn.model_required, while post-turn work keeps its buffer unsealed — is
// not stopped by a later Stop: the cancel would be a no-op, so the Stop
// reports it finished (409) rather than take the failure for its own
// cancellation. A Stop by key still flags the turn, so its settlement does
// not re-queue the input it named.
func TestStop_TurnThatAlreadyFailedIsNotReportedStopped(t *testing.T) {
	for _, frame := range []string{"turn.error", "turn.model_required", "turn.cancelled"} {
		for _, keyed := range []bool{true, false} {
			t.Run(frame+map[bool]string{true: "/by key", false: "/by turn"}[keyed], func(t *testing.T) {
				srv := newDefaultChatServer(t, &fakeEngine{}, newFakeChatStore())
				var cancelled atomic.Bool
				buf, turnID, tok, _ := srv.registerTurn("conv-f", func() { cancelled.Store(true) })
				defer srv.finishTurn("conv-f", tok)
				srv.inflightMu.Lock()
				e := srv.inflight["conv-f"]
				e.inputKey = "k-f"
				srv.inflight["conv-f"] = e
				srv.inflightMu.Unlock()
				buf.Emit(frame, map[string]any{}) // it ended on its own; the buffer stays open
				var got turnStop
				if keyed {
					got = srv.cancelInputTurn("conv-f", "k-f")
				} else {
					got = srv.cancelInflightTurn("conv-f", turnID)
				}
				if got != turnNotStopped || cancelled.Load() {
					t.Fatalf("Stop = %v (cancel sent %v), want turnNotStopped with no cancel", got, cancelled.Load())
				}
				if keyed && !buf.stoppedByKey.Load() {
					t.Fatal("a keyed Stop of an ended, unsettled turn left its input to be re-queued")
				}
			})
		}
	}
}

// cancelErrEngine runs each turn until it is cancelled, then fails with
// err (the cancellation itself when err is nil) — an engine that reports a
// Stop landing in preflight as an error rather than a turn.cancelled.
type cancelErrEngine struct {
	fakeEngine
	err     error
	started chan struct{}
}

func (f *cancelErrEngine) RunTurn(ctx context.Context, _ TurnInput, _ agent.EventSink) (*TurnResult, error) {
	close(f.started)
	<-ctx.Done()
	if f.err != nil {
		return nil, f.err
	}
	return nil, ctx.Err()
}

// A Stop that makes the engine fail with the cancellation is advertised as
// turn.cancelled, so the Stop is confirmed (only turn.cancelled confirms
// one); an engine failure of its own, even one reached after a Stop, is
// still turn.error.
func TestStopCausedEngineErrorIsAdvertisedCancelled(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		frame string
		want  turnStop
	}{
		{"the cancellation", nil, "turn.cancelled", turnStopped},
		{"a failure of its own", errors.New("sandbox unavailable"), "turn.error", turnNotStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &cancelErrEngine{err: tc.err, started: make(chan struct{})}
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, eng, st)
			conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
			go postChatRequest(t, srv, map[string]any{"message": "go", "conversation_id": conv.ID})
			<-eng.started
			entry, ok := srv.getInflight(conv.ID)
			if !ok {
				t.Fatal("no inflight turn")
			}
			if got := srv.cancelInflightTurn(conv.ID, entry.turnID); got != tc.want {
				t.Fatalf("Stop = %v, want %v", got, tc.want)
			}
			if got := entry.buf.terminalOutcome(); got != tc.frame {
				t.Fatalf("terminal frame %q, want %q", got, tc.frame)
			}
		})
	}
}

// A Stop by key writes its intent on the key's row durably BEFORE it cancels
// the turn, so the turn's settlement (and boot recovery, if the process dies
// first) cancels an uncommitted row rather than re-queue it. A failed write
// still stops the turn — the in-memory record covers this process — but the
// Stop is not reported as landed.
func TestStopByKey_RecordsItsIntentBeforeTheCancel(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "recorded", true: "record fails"}[fails], func(t *testing.T) {
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, &fakeEngine{}, st)
			conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
			if fails {
				st.stopRequestFailures = 1
			}
			var recordedFirst, cancelled atomic.Bool
			var buf *turnBuffer
			buf, _, tok, _ := srv.registerTurn(conv.ID, func() {
				st.mu.Lock()
				recordedFirst.Store(len(st.stopRequested) == 1 && st.stopRequested[0] == conv.ID+"/k-i")
				st.mu.Unlock()
				cancelled.Store(true)
				buf.Emit("turn.cancelled", map[string]any{})
			})
			defer srv.finishTurn(conv.ID, tok)
			srv.inflightMu.Lock()
			e := srv.inflight[conv.ID]
			e.inputKey = "k-i"
			srv.inflight[conv.ID] = e
			srv.inflightMu.Unlock()
			st.mu.Lock()
			st.queue = append(st.queue, store.InputQueueRow{ID: "r-i", ConversationID: conv.ID, UserEmail: "u@x.com", ClientInputID: "k-i", Mode: store.InputModeQueued, State: store.InputStateRunning, TurnID: e.turnID})
			st.mu.Unlock()
			_, ok := srv.stopInput(context.Background(), "u@x.com", conv.ID, "k-i")
			if !cancelled.Load() {
				t.Fatal("the turn was not stopped")
			}
			if ok == fails {
				t.Fatalf("ok = %v, want %v", ok, !fails)
			}
			if !fails && !recordedFirst.Load() {
				t.Fatal("the turn was cancelled before the Stop's intent was recorded")
			}
		})
	}
}

// The in-memory Stop records refuse a sealed buffer: the settlement reads
// them after the turn's Finish, so one landing after the seal would never be
// seen, and the Stop must treat the turn as ended instead.
func TestStopRecords_RefuseASealedBuffer(t *testing.T) {
	buf := newTurnBuffer("c", "t")
	if !buf.markStoppedByKey() || !buf.addStoppedSteer("s-1") {
		t.Fatal("an open buffer refused a Stop record")
	}
	buf.Finish()
	if buf.markStoppedByKey() || buf.addStoppedSteer("s-2") {
		t.Fatal("a sealed buffer accepted a Stop record its settlement may already have read")
	}
	if got := buf.stoppedSteerIDs(); len(got) != 1 || got[0] != "s-1" {
		t.Fatalf("stopped steers = %v", got)
	}
}

// A Stop confirms the cancel against the turn's own terminal frame, read as
// soon as it is emitted: "running" is checked before the cancel, and a turn
// that completes in between emits turn.completed — even while post-turn work
// keeps its buffer unsealed — so the Stop reports it finished, not stopped.
// A turn the cancel reaches mid-run emits turn.cancelled — the only frame
// that confirms a Stop; one that fails on its own in between (turn.error,
// turn.model_required) stopped for its own reasons, so the Stop stopped
// nothing. A turn that emits no terminal frame in time is unconfirmed, never
// assumed stopped.
func TestStop_ConfirmsTheCancelAgainstTheTurnsEnd(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame string // "" = no terminal frame in time
		want  turnStop
	}{
		{"completes anyway, still unsealed", "turn.completed", turnNotStopped},
		{"fails on its own after the check", "turn.error", turnNotStopped},
		{"model required after the check", "turn.model_required", turnNotStopped},
		{"cancelled", "turn.cancelled", turnStopped},
		{"no terminal frame yet", "", turnStopUnconfirmed},
	} {
		for _, keyed := range []bool{true, false} {
			t.Run(tc.name+map[bool]string{true: "/by key", false: "/by turn"}[keyed], func(t *testing.T) {
				st := newFakeChatStore()
				srv := newDefaultChatServer(t, &fakeEngine{}, st)
				conv, _ := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
				var buf *turnBuffer
				buf, turnID, tok, _ := srv.registerTurn(conv.ID, func() {
					if tc.frame != "" {
						buf.Emit(tc.frame, map[string]any{}) // never sealed: post-turn work continues
					}
				})
				defer srv.finishTurn(conv.ID, tok)
				srv.inflightMu.Lock()
				e := srv.inflight[conv.ID]
				e.inputKey = "k-c"
				srv.inflight[conv.ID] = e
				srv.inflightMu.Unlock()
				var got turnStop
				if keyed {
					got = srv.cancelInputTurn(conv.ID, "k-c")
				} else {
					got = srv.cancelInflightTurn(conv.ID, turnID)
				}
				if got != tc.want {
					t.Fatalf("Stop = %v, want %v", got, tc.want)
				}
			})
		}
	}
}
