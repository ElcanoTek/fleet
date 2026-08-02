package httpapi

// #785 flow tests: queue-not-cancel, drain-as-separate-turn, Stop covering
// queued work, idempotent submission, and mid-turn steering end to end
// against the real Postgres store.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/ratelimit"
	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// gatedEngine blocks each RunTurn until the test releases it, honoring the
// #798 commit contract and (when steer=true) the #785 steer seam.
type gatedEngine struct {
	started chan struct{} // one signal per RunTurn start
	release chan struct{} // one token per RunTurn completion
	steer   bool

	turns     atomic.Int32
	cancelled atomic.Int32
}

func (f *gatedEngine) RunTurn(ctx context.Context, in TurnInput, sink agent.EventSink) (*TurnResult, error) {
	f.turns.Add(1)
	select {
	case f.started <- struct{}{}:
	default:
	}

	newHistory := []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"` + in.UserMessage + `"}`)},
	}
	if in.CommitUser != nil {
		if err := in.CommitUser(ctx, newHistory[0]); err != nil {
			return nil, err
		}
	}
	sink.Emit("turn.started", map[string]any{"persona": in.Persona})

	if f.steer && in.SteerSource != nil {
		// Poll the boundary like the real PrepareStep until the steer arrives.
		deadline := time.After(3 * time.Second)
		for {
			if msg, ok := in.SteerSource.Poll(); ok {
				if err := in.SteerSource.Acknowledge(ctx, msg.ID); err == nil {
					newHistory = append(newHistory, agent.HistoryEntry{
						Role: "user", Type: "text", Content: json.RawMessage(`{"text":"` + msg.Text + `"}`),
					})
				}
				break
			}
			select {
			case <-deadline:
				// no steer arrived; proceed
			case <-time.After(10 * time.Millisecond):
				continue
			}
			break
		}
	}

	select {
	case <-f.release:
	case <-ctx.Done():
		f.cancelled.Add(1)
	}

	newHistory = append(newHistory, agent.HistoryEntry{
		Role: "assistant", Type: "text", Content: json.RawMessage(`{"text":"reply to: ` + in.UserMessage + `"}`),
	})
	if in.CommitTerminal != nil {
		if err := in.CommitTerminal(newHistory[1:], ctx.Err() != nil); err != nil {
			return nil, err
		}
	}
	sink.Emit("turn.completed", map[string]any{"model": in.Model})
	return &TurnResult{FinalText: "done", NewHistory: newHistory, Cancelled: ctx.Err() != nil}, nil
}

func (f *gatedEngine) Summarize(context.Context, SummarizeInput) (*SummarizeResult, error) {
	return &SummarizeResult{}, nil
}
func (f *gatedEngine) SuggestTitle(context.Context, string, string) string { return "" }
func (f *gatedEngine) ExtractMemories(context.Context, string, string, []string) []agent.ExtractedFact {
	return nil
}
func (f *gatedEngine) SuggestRecurringTask(context.Context, string, []string) (*agent.RecurringTaskProposal, error) {
	return nil, nil
}
func (f *gatedEngine) SuggestLibraryPrompt(context.Context, string) (*agent.LibraryPromptDraft, error) {
	return nil, nil
}
func (f *gatedEngine) MCPBroker() agentcore.MCPBroker               { return nil }
func (f *gatedEngine) MCPCatalog() []mcp.ServerTool                 { return nil }
func (f *gatedEngine) SandboxPool() *sandbox.Pool                   { return nil }
func (f *gatedEngine) MCPServerCatalog() []agent.OptionalServerInfo { return nil }
func (f *gatedEngine) ListPersonas() ([]string, error)              { return nil, nil }
func (f *gatedEngine) ProviderHealth() []agentcore.ModelHealth      { return nil }

func postChatJSON(t *testing.T, s *Server, user string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/chat", bytes.NewReader(raw))
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", user)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	return w
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestQueue_SecondSubmitQueuesThenDrainsAsSeparateTurn(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4)}
	s.agent = eng

	// Turn 1 starts and blocks.
	go postChatJSON(t, s, user, map[string]any{"message": "first question", "conversation_id": conv.ID})
	<-eng.started

	// Turn 2 arrives while busy: queued, never a cancel.
	w := postChatJSON(t, s, user, map[string]any{"message": "second question", "conversation_id": conv.ID, "input_id": "cli-2"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("busy submit: status=%d body=%s", w.Code, w.Body.String())
	}
	if eng.cancelled.Load() != 0 {
		t.Fatal("running turn was cancelled by a queued submission")
	}

	// Release both turns; the queued input drains as its own turn.
	eng.release <- struct{}{}
	eng.release <- struct{}{}
	waitFor(t, "queued turn to drain", func() bool { return eng.turns.Load() == 2 })
	waitFor(t, "both exchanges persisted", func() bool {
		h, herr := s.store.LoadHistory(context.Background(), conv.ID)
		if herr != nil {
			return false
		}
		var text string
		for _, e := range h {
			text += string(e.Content)
		}
		return strings.Contains(text, "first question") && strings.Contains(text, "second question")
	})
	waitFor(t, "queue row completed", func() bool {
		items, _ := s.store.ListQueuedInputs(context.Background(), user, conv.ID)
		return len(items) == 0
	})
}

func TestQueue_StopCoversQueuedWork(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4)}
	s.agent = eng

	go postChatJSON(t, s, user, map[string]any{"message": "long task", "conversation_id": conv.ID})
	<-eng.started
	if w := postChatJSON(t, s, user, map[string]any{"message": "follow-up", "conversation_id": conv.ID}); w.Code != http.StatusAccepted {
		t.Fatalf("queue submit: %d", w.Code)
	}

	// Stop (default scope=all): cancels the active turn AND the queued row.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/conversations/"+conv.ID+"/cancel", strings.NewReader(`{}`))
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", user)
	wc := httptest.NewRecorder()
	s.Routes().ServeHTTP(wc, req)
	if wc.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", wc.Code)
	}
	waitFor(t, "active turn cancelled", func() bool { return eng.cancelled.Load() == 1 })
	eng.release <- struct{}{} // let the (cancelled) turn finish its bookkeeping

	waitFor(t, "queued row cancelled, no drain", func() bool {
		items, _ := s.store.ListQueuedInputs(context.Background(), user, conv.ID)
		return len(items) == 0
	})
	time.Sleep(100 * time.Millisecond)
	if eng.turns.Load() != 1 {
		t.Fatalf("cancelled queue row still drained: %d turns", eng.turns.Load())
	}
}

func TestQueue_IdempotentSubmission(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4)}
	s.agent = eng

	go postChatJSON(t, s, user, map[string]any{"message": "task", "conversation_id": conv.ID})
	<-eng.started

	w1 := postChatJSON(t, s, user, map[string]any{"message": "again", "conversation_id": conv.ID, "input_id": "same-key"})
	w2 := postChatJSON(t, s, user, map[string]any{"message": "again", "conversation_id": conv.ID, "input_id": "same-key"})
	if w1.Code != http.StatusAccepted || w2.Code != http.StatusOK {
		t.Fatalf("statuses = %d/%d, want 202 then 200", w1.Code, w2.Code)
	}
	items, _ := s.store.ListQueuedInputs(context.Background(), user, conv.ID)
	if len(items) != 1 {
		t.Fatalf("rows = %d, want 1 (idempotent)", len(items))
	}
	eng.release <- struct{}{}
	eng.release <- struct{}{}
}

func TestQueue_SteerInjectsMidTurnExactlyOnce(t *testing.T) {
	s := serverFixture(t)
	const user = "bob@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4), steer: true}
	s.agent = eng

	go postChatJSON(t, s, user, map[string]any{"message": "start work", "conversation_id": conv.ID})
	<-eng.started

	w := postChatJSON(t, s, user, map[string]any{"message": "steer: also include Q3", "conversation_id": conv.ID, "mode": "steer", "input_id": "steer-1"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("steer submit: %d %s", w.Code, w.Body.String())
	}

	// The engine's boundary poll accepts + acknowledges, then we release.
	waitFor(t, "steer row injected", func() bool {
		items, _ := s.store.ListQueuedInputs(context.Background(), user, conv.ID)
		for _, it := range items {
			if it.ClientInputID == "steer-1" && it.State == "injected" {
				return true
			}
		}
		return false
	})
	eng.release <- struct{}{}

	// The steered text is committed exactly once, and the row completes with
	// the turn's canonical commit — never draining as a second turn.
	waitFor(t, "steer text persisted once", func() bool {
		h, herr := s.store.LoadHistory(context.Background(), conv.ID)
		if herr != nil {
			return false
		}
		n := 0
		for _, e := range h {
			if strings.Contains(string(e.Content), "steer: also include Q3") {
				n++
			}
		}
		return n == 1
	})
	waitFor(t, "steer row completed", func() bool {
		items, _ := s.store.ListQueuedInputs(context.Background(), user, conv.ID)
		return len(items) == 0
	})
	time.Sleep(100 * time.Millisecond)
	if eng.turns.Load() != 1 {
		t.Fatalf("steered input also drained as a turn: %d turns", eng.turns.Load())
	}
}

// failFirstEngine fails RunTurn before any commit for the first N turns, then
// behaves like gatedEngine — the drained-turn pre-commit failure scenario.
type failFirstEngine struct {
	gatedEngine
	failuresLeft atomic.Int32
}

func (f *failFirstEngine) RunTurn(ctx context.Context, in TurnInput, sink agent.EventSink) (*TurnResult, error) {
	if f.failuresLeft.Add(-1) >= 0 {
		f.turns.Add(1)
		select {
		case f.started <- struct{}{}:
		default:
		}
		return nil, context.DeadlineExceeded // fails BEFORE CommitUser
	}
	return f.gatedEngine.RunTurn(ctx, in, sink)
}

func TestQueue_DrainedTurnPreCommitFailureRequeuesNotCompletes(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &failFirstEngine{gatedEngine: gatedEngine{started: make(chan struct{}, 8), release: make(chan struct{}, 8)}}
	s.agent = eng

	// Turn 1 succeeds normally (no scripted failure yet).
	go postChatJSON(t, s, user, map[string]any{"message": "first", "conversation_id": conv.ID})
	<-eng.started
	// Queue a follow-up, then make the NEXT (drained) turn fail pre-commit.
	if w := postChatJSON(t, s, user, map[string]any{"message": "precious follow-up", "conversation_id": conv.ID, "input_id": "keep-me"}); w.Code != http.StatusAccepted {
		t.Fatalf("queue submit: %d", w.Code)
	}
	eng.failuresLeft.Store(1)
	eng.release <- struct{}{} // finish turn 1; the drain launches the failing turn

	// The 202-acknowledged input must come back to 'queued' — never a silent
	// 'completed' with its text absent from history.
	waitFor(t, "failed drained turn re-queues the row", func() bool {
		items, _ := s.store.ListQueuedInputs(context.Background(), user, conv.ID)
		for _, it := range items {
			if it.ClientInputID == "keep-me" && it.State == "queued" {
				return true
			}
		}
		return false
	})
	// And the bounded re-kick eventually drains it successfully.
	eng.release <- struct{}{}
	waitFor(t, "re-kicked row eventually persists", func() bool {
		h, herr := s.store.LoadHistory(context.Background(), conv.ID)
		if herr != nil {
			return false
		}
		for _, e := range h {
			if strings.Contains(string(e.Content), "precious follow-up") {
				return true
			}
		}
		return false
	})
}

func TestQueue_DrainWorksAtConcurrencyCapOne(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	// The regression: with cap=1 the completion tail drained BEFORE the
	// completing turn released its own slot — every drain failed Acquire and
	// nothing ever re-kicked.
	s.concurrent = ratelimit.NewConcurrencyLimiter(1)
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 8), release: make(chan struct{}, 8)}
	s.agent = eng

	go postChatJSON(t, s, user, map[string]any{"message": "first", "conversation_id": conv.ID})
	<-eng.started
	if w := postChatJSON(t, s, user, map[string]any{"message": "second", "conversation_id": conv.ID}); w.Code != http.StatusAccepted {
		t.Fatalf("queue submit: %d", w.Code)
	}
	eng.release <- struct{}{}
	eng.release <- struct{}{}
	waitFor(t, "queued turn drains at cap=1", func() bool { return eng.turns.Load() == 2 })
}
