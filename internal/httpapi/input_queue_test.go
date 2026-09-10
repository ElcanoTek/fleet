package httpapi

// #785 flow tests: queue-not-cancel, drain-as-separate-turn, Stop covering
// queued work, idempotent submission, and mid-turn steering end to end
// against the real Postgres store.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/ratelimit"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/store"
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
func (f *gatedEngine) SuggestLibraryPrompt(context.Context, agent.LibraryPromptInput) (*agent.LibraryPromptDraft, error) {
	return nil, nil
}
func (f *gatedEngine) MCPBroker() agentcore.MCPBroker { return nil }
func (f *gatedEngine) MCPCatalog() []mcp.ServerTool   { return nil }
func (f *gatedEngine) OpenApprovalRemoteMCPScope(context.Context, string, string, string) (*agent.RemoteMCPOverlay, error) {
	return nil, nil
}

func (f *gatedEngine) OpenApprovalMCPScope(context.Context, agentcore.MCPSelection, string) (*agent.MCPScope, error) {
	return nil, nil
}
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

// sweepHoldStore forces the race Stop scope=all has to win: its queue sweep
// (CancelQueuedInputs) does not proceed until the cancelled turn has finished
// and settled — the point right before that turn's completion tail-calls
// maybeDrainQueue. Without the stopSweeps interlock the drain then races the
// sweep for the still-queued follow-up; with it, the drain defers. The
// wrapper also asserts the interlock is actually held while the sweep runs,
// which is the synchronous, database-independent form of that guarantee.
type sweepHoldStore struct {
	chatStore
	t       *testing.T
	srv     *Server
	settled chan struct{}
	once    sync.Once
}

func (w *sweepHoldStore) SettleTurnInputs(ctx context.Context, turnID, drainedID string) (int, int, error) {
	w.once.Do(func() { close(w.settled) })
	return w.chatStore.SettleTurnInputs(ctx, turnID, drainedID)
}

func (w *sweepHoldStore) CancelQueuedInputs(ctx context.Context, userEmail, convID string, before int64) (int, error) {
	if !w.srv.stopSweepPending(convID) {
		w.t.Error("Stop swept the queue without holding the drain interlock: a cancelled turn's tail-call drain could claim the FIFO head first")
	}
	select {
	case <-w.settled:
	case <-time.After(5 * time.Second):
		w.t.Error("sweep held 5s without the cancelled turn settling: Stop must cancel the active turn before it sweeps")
	}
	return w.chatStore.CancelQueuedInputs(ctx, userEmail, convID, before)
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
	// Stop scope=all cancels the active turn first (the model must stop at
	// once) and sweeps the queue second. The cancelled turn's completion
	// tail-calls maybeDrainQueue, which must be held off while the sweep is
	// in flight so it cannot claim the FIFO head from under the sweep. The
	// -race lane lost that race on main; this wrapper pins the sweep behind
	// the turn's settlement so the test exercises the losing order
	// deterministically.
	hold := &sweepHoldStore{chatStore: s.store, t: t, srv: s, settled: make(chan struct{})}
	s.store = hold

	go postChatJSON(t, s, user, map[string]any{"message": "long task", "conversation_id": conv.ID})
	<-eng.started
	if w := postChatJSON(t, s, user, map[string]any{"message": "follow-up", "conversation_id": conv.ID}); w.Code != http.StatusAccepted {
		t.Fatalf("queue submit: %d", w.Code)
	}
	if items, _ := s.store.ListQueuedInputs(context.Background(), user, conv.ID); len(items) != 1 {
		t.Fatalf("precondition: want 1 queued row, got %d", len(items))
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

// TestQueue_StopSweepInterlockDefersDrain pins the mechanism behind
// TestQueue_StopCoversQueuedWork: while a Stop scope=all sweep is in flight,
// maybeDrainQueue must not claim, and when the sweep ends the deferred drain
// must run so a row accepted after the Stop still launches.
func TestQueue_StopSweepInterlockDefersDrain(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4)}
	s.agent = eng

	// A row accepted after the Stop began, queued with no turn running —
	// exactly the state a cancelled turn's tail-call drain finds when it
	// races the Stop's sweep. (A row accepted BEFORE the Stop is refused by
	// stopGateForRow regardless; the interlock is what keeps the drain from
	// claiming anything from under the sweep.)
	s.beginStopSweep(conv.ID)
	if _, _, err := s.store.EnqueueInput(t.Context(), store.InputQueueRow{
		ID: "q-1", ConversationID: conv.ID, UserEmail: user, ClientInputID: "cli-q-1",
		Message: "follow-up", Attachments: "[]", Mode: store.InputModeQueued,
	}); err != nil {
		t.Fatal(err)
	}
	s.maybeDrainQueue(conv.ID)
	time.Sleep(100 * time.Millisecond)
	if n := eng.turns.Load(); n != 0 {
		t.Fatalf("drain launched %d turn(s) while a Stop sweep was pending", n)
	}
	if items, _ := s.store.ListQueuedInputs(context.Background(), user, conv.ID); len(items) != 1 {
		t.Fatalf("row should still be queued while the sweep is pending, got %d", len(items))
	}

	// Lifting the interlock re-kicks the drain: the row runs without
	// waiting for another submission.
	s.endStopSweep(conv.ID)
	<-eng.started
	eng.release <- struct{}{}
	waitFor(t, "deferred row drained", func() bool { return eng.turns.Load() == 1 })
}

// sweepPausingStore pauses the drain inside its turn preparation (the
// memories load) — i.e. after launchQueuedTurn has decided the row is
// post-Stop and before startTurn reaches registration — and lets the test act
// while it is paused. Pure channel handoff: nothing flows from the store call
// into the server.
type sweepPausingStore struct {
	chatStore
	reached chan struct{} // closed when the drain enters ListMemories
	proceed chan struct{} // closed by the test to let the drain continue
	once    sync.Once
}

func (w *sweepPausingStore) ListMemories(ctx context.Context, userEmail string) ([]store.Memory, error) {
	w.once.Do(func() {
		close(w.reached)
		<-w.proceed
	})
	return w.chatStore.ListMemories(ctx, userEmail)
}

// launchPausedInPrep claims the conversation's FIFO head and launches it,
// returning once the drain is paused in turn preparation: the row is claimed
// (invisible to any sweep) and decided post-Stop, but not yet registered.
// The returned channel yields launchQueuedTurn's result after the test lets
// the drain proceed.
func launchPausedInPrep(t *testing.T, s *Server, convID string) (*sweepPausingStore, <-chan bool) {
	t.Helper()
	row, err := s.store.ClaimNextQueuedInput(t.Context(), convID, "claim-placeholder")
	if err != nil || row == nil {
		t.Fatalf("claim: row=%v err=%v", row, err)
	}
	pause := &sweepPausingStore{chatStore: s.store, reached: make(chan struct{}), proceed: make(chan struct{})}
	s.store = pause
	launched := make(chan bool, 1)
	go func() { launched <- s.launchQueuedTurn(convID, row) }()
	select {
	case <-pause.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("drain never reached turn preparation")
	}
	return pause, launched
}

// expectGateCancelled lets a paused drain proceed and asserts the registration
// gate refused it: no turn started and the claimed row ended cancelled — not
// un-claimed, which would let it launch on the post-sweep re-kick.
func expectGateCancelled(t *testing.T, s *Server, eng *gatedEngine, convID, clientID string, pause *sweepPausingStore, launched <-chan bool) {
	t.Helper()
	close(pause.proceed)
	select {
	case ok := <-launched:
		if !ok {
			t.Fatal("launchQueuedTurn reported a lost registerTurn race; the generation gate must handle the row itself")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("launchQueuedTurn did not return")
	}
	time.Sleep(150 * time.Millisecond)
	if n := eng.turns.Load(); n != 0 {
		t.Fatalf("row claimed before a Stop launched %d turn(s)", n)
	}
	got, err := s.store.LookupInput(t.Context(), convID, clientID)
	if err != nil || got == nil {
		t.Fatalf("lookup: row=%v err=%v", got, err)
	}
	if got.State != store.InputStateCancelled {
		t.Fatalf("row state = %q, want %q: the gate must cancel the row, not leave it to re-launch", got.State, store.InputStateCancelled)
	}
}

// TestQueue_StopSweepGatesClaimedRowAtRegistration covers the drain that
// passed maybeDrainQueue's interlock check and stopGateForRow before a Stop
// began, with the FIFO head already claimed: the sweep can no longer see that
// row, so registration must refuse it (the Stop generation moved) while the
// sweep is still in flight, and the row must stay cancelled after the sweep
// ends and re-kicks the drain.
func TestQueue_StopSweepGatesClaimedRowAtRegistration(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4)}
	s.agent = eng

	if _, _, err := s.store.EnqueueInput(t.Context(), store.InputQueueRow{
		ID: "q-1", ConversationID: conv.ID, UserEmail: user, ClientInputID: "cli-q-1",
		Message: "follow-up", Attachments: "[]", Mode: store.InputModeQueued,
	}); err != nil {
		t.Fatal(err)
	}
	pause, launched := launchPausedInPrep(t, s, conv.ID)
	// The Stop begins after the claim and the drain's decision; its sweep is
	// still in flight when the drain reaches registration.
	s.beginStopSweep(conv.ID)
	expectGateCancelled(t, s, eng, conv.ID, "cli-q-1", pause, launched)

	// Lifting the interlock re-kicks the drain; the cancelled row must not run.
	s.endStopSweep(conv.ID)
	time.Sleep(150 * time.Millisecond)
	if n := eng.turns.Load(); n != 0 {
		t.Fatalf("cancelled row ran after the sweep ended: %d turn(s)", n)
	}
}

// stalledQueueListStore stalls ListQueuedInputs until its context expires —
// a store that has stopped answering — and records whether the drain's
// admission slot had already been released when the stall began.
type stalledQueueListStore struct {
	chatStore
	released       *atomic.Bool
	releasedFirst  atomic.Bool
	listCalls      atomic.Int32
	ctxHadDeadline atomic.Bool
}

func (w *stalledQueueListStore) ListQueuedInputs(ctx context.Context, _, _ string) ([]store.InputQueueRow, error) {
	w.listCalls.Add(1)
	w.releasedFirst.Store(w.released.Load())
	_, hasDeadline := ctx.Deadline()
	w.ctxHadDeadline.Store(hasDeadline)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return nil, errors.New("queue refresh context was not bounded")
	}
}

// TestQueue_SweptLaunchReleasesSlotBeforeQueueRefresh: when the registration
// gate refuses a claimed row, the drain's per-user admission slot must be
// released before the best-effort queue refresh, and that refresh must run on
// a bounded context. Otherwise a store that has stopped answering pins a slot
// for as long as it stalls, and repeats of the race exhaust the per-user cap
// (429s) while no turn is running.
func TestQueue_SweptLaunchReleasesSlotBeforeQueueRefresh(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.store.EnqueueInput(t.Context(), store.InputQueueRow{
		ID: "q-1", ConversationID: conv.ID, UserEmail: user, ClientInputID: "cli-q-1",
		Message: "follow-up", Attachments: "[]", Mode: store.InputModeQueued,
	}); err != nil {
		t.Fatal(err)
	}
	row, err := s.store.ClaimNextQueuedInput(t.Context(), conv.ID, "claim-placeholder")
	if err != nil || row == nil {
		t.Fatalf("claim: row=%v err=%v", row, err)
	}
	// A direct submission is running (registered during the sweep), so the
	// queue refresh has a live buffer to publish into and reaches the store.
	if _, _, _, ok := s.registerTurn(conv.ID, func() {}); !ok {
		t.Fatal("registerTurn refused on an idle conversation")
	}
	var released atomic.Bool
	stall := &stalledQueueListStore{chatStore: s.store, released: &released}
	s.store = stall
	// A Stop began after the drain decided its row: the gate refuses it.
	gen, _ := s.stopGateForRow(conv.ID, row.AcceptedAt)
	s.beginStopSweep(conv.ID)
	defer s.endStopSweep(conv.ID)

	done := make(chan bool, 1)
	go func() {
		done <- s.startTurn(nil, nil, user, conv, chatRequest{ConversationID: conv.ID, Message: row.Message},
			&queuedLaunch{rowID: row.ID, claimTurnID: row.TurnID, sweepGen: gen}, func() { released.Store(true) })
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("startTurn reported a lost registerTurn race; the gate must handle the row itself")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("startTurn did not return: the swept path's queue refresh is not bounded")
	}
	if stall.listCalls.Load() == 0 {
		t.Fatal("precondition: the swept path never reached the queue refresh")
	}
	if !stall.releasedFirst.Load() {
		t.Fatal("admission slot was still held when the queue refresh stalled")
	}
	if !stall.ctxHadDeadline.Load() {
		t.Fatal("queue refresh ran on an unbounded context")
	}
	if got, err := s.store.LookupInput(t.Context(), conv.ID, "cli-q-1"); err != nil || got == nil || got.State != store.InputStateCancelled {
		t.Fatalf("swept row: %+v err=%v, want cancelled", got, err)
	}
}

// TestQueue_StopSweepGenerationGatesSlowDrain covers the drain that decided
// its row before a Stop and then spent longer preparing its turn than the
// whole sweep took: by the time it registers the interlock is released, so
// only the Stop generation it carries can tell it the row is now pre-Stop.
func TestQueue_StopSweepGenerationGatesSlowDrain(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4)}
	s.agent = eng

	if _, _, err := s.store.EnqueueInput(t.Context(), store.InputQueueRow{
		ID: "q-1", ConversationID: conv.ID, UserEmail: user, ClientInputID: "cli-q-1",
		Message: "follow-up", Attachments: "[]", Mode: store.InputModeQueued,
	}); err != nil {
		t.Fatal(err)
	}
	pause, launched := launchPausedInPrep(t, s, conv.ID)
	// A whole Stop comes and goes while the drain is still preparing.
	s.beginStopSweep(conv.ID)
	s.endStopSweep(conv.ID)
	expectGateCancelled(t, s, eng, conv.ID, "cli-q-1", pause, launched)
}

// flakyTerminalStore fails the first n MarkClaimedInputTerminal writes.
type flakyTerminalStore struct {
	chatStore
	failures atomic.Int32
}

func (w *flakyTerminalStore) MarkClaimedInputTerminal(ctx context.Context, id, claimTurnID, state string) error {
	if w.failures.Add(-1) >= 0 {
		return errors.New("simulated store outage")
	}
	return w.chatStore.MarkClaimedInputTerminal(ctx, id, claimTurnID, state)
}

// TestQueue_TerminalizeQueueRowRetries: a claimed row whose cancel write
// fails must not stay 'running' until restart — the write is retried.
func TestQueue_TerminalizeQueueRowRetries(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.store.EnqueueInput(t.Context(), store.InputQueueRow{
		ID: "q-1", ConversationID: conv.ID, UserEmail: user, ClientInputID: "cli-q-1",
		Message: "follow-up", Attachments: "[]", Mode: store.InputModeQueued,
	}); err != nil {
		t.Fatal(err)
	}
	row, err := s.store.ClaimNextQueuedInput(t.Context(), conv.ID, "claim-placeholder")
	if err != nil || row == nil {
		t.Fatalf("claim: row=%v err=%v", row, err)
	}
	flaky := &flakyTerminalStore{chatStore: s.store}
	flaky.failures.Store(1)
	s.store = flaky

	s.terminalizeQueueRow(conv.ID, row.ID, row.TurnID, store.InputStateCancelled)
	if got, _ := s.store.LookupInput(t.Context(), conv.ID, "cli-q-1"); got == nil || got.State != store.InputStateRunning {
		t.Fatalf("first write was meant to fail; state=%v", got)
	}
	waitForCond(t, queueTerminalizeRetryDelay+3*time.Second, func() bool {
		got, err := s.store.LookupInput(context.Background(), conv.ID, "cli-q-1")
		return err == nil && got != nil && got.State == store.InputStateCancelled
	})
}

// TestQueue_TerminalizeRetryRespectsLaterClaim: a re-queue write that
// committed but reported an error is retried; by then another drain may have
// re-claimed the row. The retry is guarded on the original claim placeholder,
// so it must leave the re-claimed row alone rather than flip a live row back
// to 'queued' and run the input twice.
func TestQueue_TerminalizeRetryRespectsLaterClaim(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.store.EnqueueInput(t.Context(), store.InputQueueRow{
		ID: "q-1", ConversationID: conv.ID, UserEmail: user, ClientInputID: "cli-q-1",
		Message: "follow-up", Attachments: "[]", Mode: store.InputModeQueued,
	}); err != nil {
		t.Fatal(err)
	}
	underlying := s.store
	first, err := underlying.ClaimNextQueuedInput(t.Context(), conv.ID, "claim-A")
	if err != nil || first == nil {
		t.Fatalf("claim A: row=%v err=%v", first, err)
	}
	// The un-claim "fails" from the driver's point of view but its UPDATE
	// committed: emulate by failing the guarded write once while applying it
	// underneath.
	flaky := &flakyTerminalStore{chatStore: underlying}
	flaky.failures.Store(1)
	s.store = flaky
	s.terminalizeQueueRow(conv.ID, first.ID, "claim-A", store.InputStateQueued)
	if err := underlying.MarkClaimedInputTerminal(t.Context(), first.ID, "claim-A", store.InputStateQueued); err != nil {
		t.Fatal(err)
	}
	// Another drain re-claims the row before the retry fires.
	second, err := underlying.ClaimNextQueuedInput(t.Context(), conv.ID, "claim-B")
	if err != nil || second == nil {
		t.Fatalf("claim B: row=%v err=%v", second, err)
	}
	time.Sleep(queueTerminalizeRetryDelay + 500*time.Millisecond)
	got, err := underlying.LookupInput(t.Context(), conv.ID, "cli-q-1")
	if err != nil || got == nil {
		t.Fatalf("lookup: row=%v err=%v", got, err)
	}
	if got.State != store.InputStateRunning || got.TurnID != "claim-B" {
		t.Fatalf("stale retry touched a re-claimed row: state=%q turn=%q, want running/claim-B", got.State, got.TurnID)
	}
}

// TestQueue_StopSparesRowAcceptedAfterStop (#1477): a row accepted after a
// Stop began — in the same wall-clock second, moments later — was never in
// that Stop's swept set and must run when the drain claims it, whether the
// Stop has finished or its sweep is still in flight.
func TestQueue_StopSparesRowAcceptedAfterStop(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endSweep bool
	}{
		{"after the sweep ended", true},
		{"while the sweep is in flight", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := serverFixture(t)
			const user = "alice@x.com"
			conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
			if err != nil {
				t.Fatal(err)
			}
			eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4)}
			s.agent = eng

			// Stay clear of a second boundary so the Stop and the enqueue
			// below land in the same wall-clock second — the case that
			// used to be ambiguous.
			if now := time.Now(); now.Nanosecond() > int(800*time.Millisecond) {
				time.Sleep(time.Until(now.Truncate(time.Second).Add(time.Second)))
			}
			epoch, _, _ := s.beginStopSweep(conv.ID)
			if tc.endSweep {
				s.endStopSweep(conv.ID)
			} else {
				defer s.endStopSweep(conv.ID)
			}
			row0, _, err := s.store.EnqueueInput(t.Context(), store.InputQueueRow{
				ID: "q-1", ConversationID: conv.ID, UserEmail: user, ClientInputID: "cli-q-1",
				Message: "post-stop follow-up", Attachments: "[]", Mode: store.InputModeQueued,
			})
			if err != nil {
				t.Fatal(err)
			}
			if row0.AcceptedAt < epoch {
				t.Fatalf("row accepted at %d is not after the Stop at %d", row0.AcceptedAt, epoch)
			}
			if row0.CreatedAt != epoch/int64(time.Second) {
				t.Fatalf("Stop (%d) and enqueue (%d) fell in different seconds; the same-second case is what this test pins", epoch, row0.AcceptedAt)
			}
			row, err := s.store.ClaimNextQueuedInput(t.Context(), conv.ID, "claim-placeholder")
			if err != nil || row == nil {
				t.Fatalf("claim: row=%v err=%v", row, err)
			}
			if !s.launchQueuedTurn(conv.ID, row) {
				t.Fatal("launchQueuedTurn reported a lost registerTurn race")
			}
			<-eng.started
			eng.release <- struct{}{}
			waitFor(t, "post-Stop row ran", func() bool { return eng.turns.Load() == 1 })
		})
	}
}

// postStopEnqueueStore is the #1477 scenario at the handler: while the Stop's
// sweep is held (as a slow store would hold it), a follow-up is accepted —
// the cancelled turn still looks running, so it becomes a queue row, and its
// insert commits before the sweep's statement runs. The sweep must cancel
// only the row that existed when Stop began.
type postStopEnqueueStore struct {
	chatStore
	t          *testing.T
	user, conv string
	epoch      int64 // the Stop instant the handler passed to the sweep
	postRow    store.InputQueueRow
}

func (w *postStopEnqueueStore) CancelQueuedInputs(ctx context.Context, userEmail, convID string, before int64) (int, error) {
	w.epoch = before
	row, created, err := w.EnqueueInput(ctx, store.InputQueueRow{
		ID: "q-post", ConversationID: w.conv, UserEmail: w.user, ClientInputID: "cli-post",
		Message: "submitted after Stop", Attachments: "[]", Mode: store.InputModeQueued,
	})
	if err != nil || !created {
		w.t.Errorf("post-Stop enqueue: created=%v err=%v", created, err)
	}
	w.postRow = row
	return w.chatStore.CancelQueuedInputs(ctx, userEmail, convID, before)
}

func TestQueue_StopSparesFollowUpSubmittedAfterStop(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4)}
	s.agent = eng
	wrap := &postStopEnqueueStore{chatStore: s.store, t: t, user: user, conv: conv.ID}
	s.store = wrap

	go postChatJSON(t, s, user, map[string]any{"message": "long task", "conversation_id": conv.ID})
	<-eng.started
	if w := postChatJSON(t, s, user, map[string]any{"message": "pre-stop follow-up", "conversation_id": conv.ID, "input_id": "cli-pre"}); w.Code != http.StatusAccepted {
		t.Fatalf("queue submit: %d", w.Code)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/conversations/"+conv.ID+"/cancel", strings.NewReader(`{}`))
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", user)
	wc := httptest.NewRecorder()
	s.Routes().ServeHTTP(wc, req)
	if wc.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", wc.Code)
	}
	if wrap.postRow.AcceptedAt < wrap.epoch {
		t.Fatalf("post-Stop row accepted at %d, before the Stop at %d", wrap.postRow.AcceptedAt, wrap.epoch)
	}
	pre, err := s.store.LookupInput(t.Context(), conv.ID, "cli-pre")
	if err != nil || pre == nil || pre.State != store.InputStateCancelled {
		t.Fatalf("pre-Stop row: %+v err=%v, want cancelled", pre, err)
	}
	post, err := s.store.LookupInput(t.Context(), conv.ID, "cli-post")
	if err != nil || post == nil || post.State == store.InputStateCancelled {
		t.Fatalf("post-Stop row: %+v err=%v, must not be swept", post, err)
	}

	waitFor(t, "active turn cancelled", func() bool { return eng.cancelled.Load() == 1 })
	// The cancelled turn's completion tail-call (or the sweep's re-kick)
	// drains the post-Stop row as the next turn.
	select {
	case <-eng.started:
	case <-time.After(5 * time.Second):
		t.Fatal("post-Stop follow-up never drained after the Stop")
	}
	eng.release <- struct{}{}
	waitFor(t, "post-Stop follow-up ran", func() bool {
		got, err := s.store.LookupInput(context.Background(), conv.ID, "cli-post")
		return err == nil && got != nil && got.State == store.InputStateCompleted
	})
	if eng.turns.Load() != 2 {
		t.Fatalf("turns = %d, want 2 (the stopped turn and the post-Stop follow-up)", eng.turns.Load())
	}
}

// TestQueue_TerminalizeRetryRequeueRekicks: when the first attempt to return a
// claimed row to 'queued' fails and the retry succeeds, the retry must re-kick
// the drain — the caller's own kick has long since fired against a row that
// was still 'running', and no turn remains to kick on completion.
func TestQueue_TerminalizeRetryRequeueRekicks(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4)}
	s.agent = eng
	if _, _, err := s.store.EnqueueInput(t.Context(), store.InputQueueRow{
		ID: "q-1", ConversationID: conv.ID, UserEmail: user, ClientInputID: "cli-q-1",
		Message: "follow-up", Attachments: "[]", Mode: store.InputModeQueued,
	}); err != nil {
		t.Fatal(err)
	}
	row, err := s.store.ClaimNextQueuedInput(t.Context(), conv.ID, "claim-placeholder")
	if err != nil || row == nil {
		t.Fatalf("claim: row=%v err=%v", row, err)
	}
	flaky := &flakyTerminalStore{chatStore: s.store}
	flaky.failures.Store(1)
	s.store = flaky

	s.terminalizeQueueRow(conv.ID, row.ID, row.TurnID, store.InputStateQueued)
	// The caller's kick fires now, against a row still 'running': nothing to claim.
	s.maybeDrainQueue(conv.ID)
	select {
	case <-eng.started:
		eng.release <- struct{}{}
	case <-time.After(queueTerminalizeRetryDelay + 5*time.Second):
		t.Fatal("re-queued row never drained: the successful retry did not re-kick the drain")
	}
	waitFor(t, "re-queued row ran", func() bool { return eng.turns.Load() == 1 })
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

// bufferEvents snapshots a live turn buffer's event names + payloads. In-package
// so the test can read the replay a reattaching client would receive without
// standing up an SSE socket.
func bufferEvents(b *turnBuffer) []bufferedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]bufferedEvent, len(b.events))
	copy(out, b.events)
	return out
}

// A drained turn's buffer must describe the queue it came out of. The previous
// turn's buffer is already sealed when the drain is kicked, so its settle-time
// queue.updated reaches nobody; without a snapshot here, a client attaching to
// the drained turn shows a chip strip that still advertises send-now/remove
// for the row this very turn is running.
func TestQueue_DrainedTurnStreamCarriesQueueSnapshot(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4)}
	s.agent = eng

	go postChatJSON(t, s, user, map[string]any{"message": "first question", "conversation_id": conv.ID})
	<-eng.started

	w := postChatJSON(t, s, user, map[string]any{
		"message": "queued follow-up", "conversation_id": conv.ID, "input_id": "cli-2",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("busy submit: status=%d body=%s", w.Code, w.Body.String())
	}
	var ack struct {
		Input struct{ ID string } `json:"input"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil || ack.Input.ID == "" {
		t.Fatalf("queue ack: %v body=%s", err, w.Body.String())
	}

	// Let turn 1 finish; its tail call drains the queued row as turn 2.
	eng.release <- struct{}{}
	waitFor(t, "queued turn to launch", func() bool { return eng.turns.Load() == 2 })

	entry, ok := s.getInflight(conv.ID)
	if !ok || entry.buf == nil {
		t.Fatal("drained turn has no live buffer")
	}
	// The engine emits a turn.started of its own; the server's is the one
	// carrying the turn id, and it comes first.
	events := bufferEvents(entry.buf)
	var started, snapshot *bufferedEvent
	for i, ev := range events {
		if ev.Name == "turn.started" && started == nil && strings.Contains(string(ev.Data), `"turn_id"`) {
			started = &events[i]
		}
		if ev.Name == "queue.updated" && snapshot == nil {
			snapshot = &events[i]
		}
	}
	if started == nil {
		t.Fatal("drained turn buffer has no turn.started")
	}
	if !strings.Contains(string(started.Data), ack.Input.ID) {
		t.Errorf("turn.started should correlate its input id: %s", started.Data)
	}
	if snapshot == nil {
		t.Fatal("drained turn buffer carries no queue.updated snapshot")
	}
	// The row is CLAIMED (running), not queued: an attaching client must not
	// render remove/send-now affordances for work already in flight.
	if !strings.Contains(string(snapshot.Data), ack.Input.ID) ||
		!strings.Contains(string(snapshot.Data), `"state":"running"`) {
		t.Errorf("queue.updated should show the drained row as running: %s", snapshot.Data)
	}

	eng.release <- struct{}{}
	waitFor(t, "queue row completed", func() bool {
		items, _ := s.store.ListQueuedInputs(context.Background(), user, conv.ID)
		return len(items) == 0
	})
}
