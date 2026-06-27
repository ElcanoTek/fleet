package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// fakeTurnEngine is a turnEngine that blocks RunTurn until the turn context is
// cancelled, then returns a PARTIAL, Cancelled result — exactly what the real
// interactive driver returns when a run is stopped mid-turn. It lets the SSE /
// cancel / persistence lifecycle be exercised with no live LLM. Only RunTurn is
// used by the cancel path; the rest satisfy the interface.
type fakeTurnEngine struct {
	started      chan struct{} // closed once RunTurn is entered
	partialText  string
	emitOnCancel bool // emit a turn.cancelled SSE frame before returning
}

func (f *fakeTurnEngine) RunTurn(ctx context.Context, in TurnInput, sink agent.EventSink) (*TurnResult, error) {
	if f.started != nil {
		close(f.started)
	}
	// Stream a little partial output so a disconnecting client could have seen
	// it, then block until the run is cancelled (Stop button → turnCancel).
	sink.Emit("text.delta", map[string]any{"text": f.partialText})
	<-ctx.Done()
	if f.emitOnCancel {
		sink.Emit("turn.cancelled", map[string]any{})
	}
	// The real driver returns a partial TurnResult with Cancelled=true and the
	// work done so far, NOT an error, so the HTTP layer persists it.
	return &TurnResult{
		FinalText: f.partialText,
		NewHistory: []agent.HistoryEntry{
			{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"` + in.UserMessage + `"}`)},
			{Role: "assistant", Type: "text", Content: json.RawMessage(`{"text":"` + f.partialText + `"}`)},
		},
		Cancelled: true,
	}, nil
}

func (f *fakeTurnEngine) Summarize(context.Context, SummarizeInput) (*SummarizeResult, error) {
	return &SummarizeResult{}, nil
}
func (f *fakeTurnEngine) SuggestTitle(context.Context, string, string) string { return "" }
func (f *fakeTurnEngine) MCPClient() *mcp.Client                              { return nil }
func (f *fakeTurnEngine) SandboxPool() *sandbox.Pool                          { return nil }
func (f *fakeTurnEngine) MCPServerCatalog() []agent.OptionalServerInfo        { return nil }
func (f *fakeTurnEngine) ListPersonas() ([]string, error)                     { return nil, nil }
func (f *fakeTurnEngine) ProviderHealth() []agentcore.ModelHealth             { return nil }

// TestChatCancel_PersistsPartialTurn pins the interactive cancel contract: when
// a turn is stopped mid-flight (the Stop button → POST /cancel → turnCancel),
// the driver's run is cancelled and the partial work is still persisted to
// history via a BACKGROUND context (the turn outlives the cancelled turnCtx).
// This is the fleet analog of cutlass's "partial work persisted on
// cancellation" — here over the SSE/persistence seam the unified driver added.
func TestChatCancel_PersistsPartialTurn(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	// Pre-create the conversation so postChat takes the existing-conversation
	// branch and we know the ID to cancel + read history from.
	conv, err := s.store.CreateConversation(t.Context(), user, "hi", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	started := make(chan struct{})
	s.agent = &fakeTurnEngine{started: started, partialText: "partial answer so far", emitOnCancel: true}

	// Fire the chat POST in its own goroutine; its SSE Attach blocks until the
	// turn finishes. We deliberately give it a context we DON'T cancel — the
	// turn must end via the Stop button, not via the HTTP request dying.
	body, _ := json.Marshal(map[string]any{"message": "do the thing", "conversation_id": conv.ID})
	postDone := make(chan struct{})
	go func() {
		defer close(postDone)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/chat", bytes.NewReader(body))
		req.Header.Set("X-Chat-Server-Token", "tok")
		req.Header.Set("X-User-Email", user)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
	}()

	// Wait until the turn is actually running inside RunTurn.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("RunTurn never started")
	}

	// Capture the turn's context so we can prove the Stop button cancels it.
	entry, ok := s.getInflight(conv.ID)
	if !ok {
		t.Fatal("no in-flight entry registered for the running turn")
	}

	// Stop button: owner-scoped cancel endpoint.
	h := s.Routes()
	rr := do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/cancel", nil, user)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("cancel: status %d, want 204", rr.Code)
	}
	_ = entry // the cancel fired the registered turnCancel; RunTurn unblocks below

	// The POST handler returns once the cancelled turn drains.
	select {
	case <-postDone:
	case <-time.After(3 * time.Second):
		t.Fatal("chat POST did not return after cancel")
	}

	// Wait for the turn goroutine (incl. its background persist + buffer
	// Finish) to fully drain so the assertions race nothing and t.Cleanup
	// doesn't close the store under a late write.
	waitForCond(t, 3*time.Second, func() bool {
		e, ok := s.getInflight(conv.ID)
		return !ok || !e.IsRunning()
	})

	// The partial work was persisted to history despite the cancellation, via
	// the background persist context (turnCtx was already cancelled).
	hist, err := s.store.LoadHistory(t.Context(), conv.ID)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("persisted history len = %d, want 2 (user + partial assistant)", len(hist))
	}
	if hist[0].Role != "user" || hist[1].Role != "assistant" {
		t.Errorf("history roles = [%s, %s], want [user, assistant]", hist[0].Role, hist[1].Role)
	}
}

// TestChatClientDisconnect_TurnSurvives pins the key fleet driver divergence:
// unlike a one-shot CLI where the request IS the run, the interactive turn is
// detached from the HTTP request lifecycle. A client disconnecting mid-turn
// (request context cancelled) must NOT cancel the run — the turn keeps going so
// a later reattach can resume it. Only the explicit Stop button cancels.
func TestChatClientDisconnect_TurnSurvives(t *testing.T) {
	s := serverFixture(t)
	const user = "bob@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "hi", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	started := make(chan struct{})
	fake := &fakeTurnEngine{started: started, partialText: "still working"}
	s.agent = fake

	// Drive the POST with a CANCELLABLE request context — we cancel it to
	// simulate the browser tab closing / network blip.
	reqCtx, disconnect := context.WithCancel(context.Background())
	body, _ := json.Marshal(map[string]any{"message": "long task", "conversation_id": conv.ID})
	postDone := make(chan struct{})
	go func() {
		defer close(postDone)
		req := httptest.NewRequestWithContext(reqCtx, http.MethodPost, "/chat", bytes.NewReader(body))
		req.Header.Set("X-Chat-Server-Token", "tok")
		req.Header.Set("X-User-Email", user)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("RunTurn never started")
	}

	// Grab the turn context, then simulate the client disconnecting.
	entry, ok := s.getInflight(conv.ID)
	if !ok {
		t.Fatal("no in-flight entry registered")
	}
	disconnect() // browser closed; the SSE Attach unblocks with context.Canceled

	// The POST handler returns (the Attach saw the disconnect), but the turn
	// itself must still be running — its context is NOT derived from the
	// request.
	select {
	case <-postDone:
	case <-time.After(2 * time.Second):
		t.Fatal("chat POST did not return after client disconnect")
	}

	// The turn is still in-flight: its context is alive and the entry reports
	// running. Give it a moment to (incorrectly) die if it were request-bound.
	time.Sleep(100 * time.Millisecond)
	if !entry.IsRunning() {
		t.Fatal("turn was marked finished by a client disconnect (should outlive the request)")
	}

	// Now stop it for real and wait for the goroutine to FULLY drain — both the
	// turn finishing and its background persist landing — before the test ends,
	// so t.Cleanup doesn't close the store out from under a late write.
	if !s.cancelInflight(conv.ID) {
		t.Fatal("expected the still-running turn to be cancellable")
	}
	waitForCond(t, 3*time.Second, func() bool {
		e, ok := s.getInflight(conv.ID)
		return !ok || !e.IsRunning()
	})
}

// waitForCond polls cond until it returns true or the deadline elapses.
func waitForCond(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %v", d)
	}
}
