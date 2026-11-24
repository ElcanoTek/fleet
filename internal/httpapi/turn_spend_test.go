package httpapi

// Turn spend reaches turn_metrics on every outcome. These drive the real agent
// loop (agent.Manager against the fake LLM, internal/fakellm) through POST
// /chat with the in-memory store, and read back the RecordTurn rows — the rows
// per-principal budgets, the admin dashboard and usage analytics aggregate.
// The fake LLM reports 10 prompt / 5 completion tokens per completion; a price
// override gives each completion a cost.

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/fakellm"
	"github.com/ElcanoTek/fleet/internal/store"
)

const (
	spendTestModel = "anthropic/claude-opus-4.8"
	// One fake completion: 10 prompt tokens at $1000/M + 5 completion tokens
	// at $2000/M.
	spendPerCompletion = 0.02
)

// newSpendHarness wires a real Manager on the fake LLM (scripted by sc) to a
// server over the in-memory store, with a price for the test model.
func newSpendHarness(t *testing.T, sc fakellm.Scenario) (*Server, *fakeChatStore) {
	t.Helper()
	agentcore.ConfigurePricing(agentcore.PricingConfig{Overrides: []agentcore.PricingOverride{
		{Model: spendTestModel, InputCostPerMillionTokens: 1000, OutputCostPerMillionTokens: 2000},
		{Model: "openrouter/" + spendTestModel, InputCostPerMillionTokens: 1000, OutputCostPerMillionTokens: 2000},
	}})
	t.Cleanup(func() { agentcore.ConfigurePricing(agentcore.PricingConfig{}) })
	// The scripted bash steps write the bash audit log; keep it out of the
	// source tree.
	t.Setenv("FLEET_AUDIT_DIR", t.TempDir())
	fake := fakellm.New()
	fake.SetDefault(sc)
	mgr := newResumeManager(t, fake.Handler())
	st := newFakeChatStore()
	return newDefaultChatServer(t, mgr, st), st
}

func (s *fakeChatStore) recordedTurnMetrics() []store.TurnMetric {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.TurnMetric(nil), s.turnMetrics...)
}

func assertTurnMetric(t *testing.T, m store.TurnMetric, completions int, cancelled bool) {
	t.Helper()
	if m.PromptTokens != 10*completions || m.CompletionTokens != 5*completions {
		t.Errorf("turn_metrics tokens = %d/%d, want %d/%d (%d billed completion(s))",
			m.PromptTokens, m.CompletionTokens, 10*completions, 5*completions, completions)
	}
	if want := spendPerCompletion * float64(completions); math.Abs(m.CostUSD-want) > 1e-9 {
		t.Errorf("turn_metrics cost = %v, want %v", m.CostUSD, want)
	}
	if m.Cancelled != cancelled {
		t.Errorf("turn_metrics cancelled = %v, want %v", m.Cancelled, cancelled)
	}
	if m.ConversationID == "" || m.UserEmail != "u@x.com" {
		t.Errorf("turn_metrics attribution = %q/%q", m.ConversationID, m.UserEmail)
	}
}

func postSpendTurn(t *testing.T, srv *Server, body map[string]any) string {
	t.Helper()
	w := postChatRequest(t, srv, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// TestFailedTurnRecordsItsSpend: a provider error after a billed tool step
// ends the turn with turn.model_required, and the billed step is recorded in
// turn_metrics — the same usage the persisted turn_summary shows.
func TestFailedTurnRecordsItsSpend(t *testing.T) {
	srv, st := newSpendHarness(t, fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.BashStep("call-1", "echo hi"),
		{Kind: fakellm.StepStatus, Status: http.StatusBadRequest},
	}})
	body := postSpendTurn(t, srv, map[string]any{"message": "run it", "model": spendTestModel})
	if !strings.Contains(body, "event: turn.model_required") {
		t.Fatalf("stream missing turn.model_required:\n%s", body)
	}
	rows := st.recordedTurnMetrics()
	if len(rows) != 1 {
		t.Fatalf("RecordTurn rows = %d, want 1 for the failed turn", len(rows))
	}
	assertTurnMetric(t, rows[0], 1, false)

	// The persisted partial turn_summary carries the same spend.
	st.mu.Lock()
	hist := append([]agent.HistoryEntry(nil), st.history[rows[0].ConversationID]...)
	st.mu.Unlock()
	if len(hist) == 0 || hist[len(hist)-1].Type != "turn_summary" {
		t.Fatalf("persisted history does not end in a turn_summary: %+v", hist)
	}
	if !strings.Contains(string(hist[len(hist)-1].Content), `"prompt_tokens":10`) {
		t.Errorf("turn_summary = %s, want the billed step's usage", hist[len(hist)-1].Content)
	}
}

// TestTurnWhoseHistorySaveFailedRecordsItsSpend: the run completed, then its
// terminal history commit failed; the turn errors, and its billed completion
// is still recorded once.
func TestTurnWhoseHistorySaveFailedRecordsItsSpend(t *testing.T) {
	srv, st := newSpendHarness(t, fakellm.Scenario{Steps: []fakellm.Step{fakellm.TextStep("The answer.")}})
	st.commitTurnErr = errors.New("database unavailable")
	body := postSpendTurn(t, srv, map[string]any{"message": "a question", "model": spendTestModel})
	if !strings.Contains(body, "event: turn.error") {
		t.Fatalf("stream missing turn.error:\n%s", body)
	}
	rows := st.recordedTurnMetrics()
	if len(rows) != 1 {
		t.Fatalf("RecordTurn rows = %d, want 1 for the turn whose save failed", len(rows))
	}
	assertTurnMetric(t, rows[0], 1, false)
}

// TestCompletedTurnRecordsSpendOnce: a completed two-step turn writes one row
// carrying both completions — not one per completion, and not twice.
func TestCompletedTurnRecordsSpendOnce(t *testing.T) {
	srv, st := newSpendHarness(t, fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.BashStep("call-1", "echo hi"),
		fakellm.TextStep("Done."),
	}})
	body := postSpendTurn(t, srv, map[string]any{"message": "run it", "model": spendTestModel})
	if !strings.Contains(body, "event: turn.completed") {
		t.Fatalf("stream missing turn.completed:\n%s", body)
	}
	rows := st.recordedTurnMetrics()
	if len(rows) != 1 {
		t.Fatalf("RecordTurn rows = %d, want 1", len(rows))
	}
	assertTurnMetric(t, rows[0], 2, false)
}

// TestRetriedTurnRecordsOnlyItsOwnSpend: a failed turn and the turn that
// retries it each record their own billed completions, once — the retry does
// not carry the failed turn's spend.
func TestRetriedTurnRecordsOnlyItsOwnSpend(t *testing.T) {
	srv, st := newSpendHarness(t, fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.BashStep("call-1", "echo hi"),
		{Kind: fakellm.StepStatus, Status: http.StatusBadRequest},
	}})
	postSpendTurn(t, srv, map[string]any{"message": "run it", "model": spendTestModel})
	first := st.recordedTurnMetrics()
	if len(first) != 1 {
		t.Fatalf("RecordTurn rows after the failed turn = %d, want 1", len(first))
	}
	// The retry answers in one completion ([[echo:…]] replies at once).
	body := postSpendTurn(t, srv, map[string]any{
		"conversation_id": first[0].ConversationID,
		"message":         "try again [[echo:Recovered.]]",
		"model":           spendTestModel,
	})
	if !strings.Contains(body, "event: turn.completed") {
		t.Fatalf("retry stream missing turn.completed:\n%s", body)
	}
	rows := st.recordedTurnMetrics()
	if len(rows) != 2 {
		t.Fatalf("RecordTurn rows = %d, want 2 (one per turn)", len(rows))
	}
	assertTurnMetric(t, rows[0], 1, false)
	assertTurnMetric(t, rows[1], 1, false)
}

// bufferHasEvent reports whether a turn's buffer has emitted an event named
// name.
func bufferHasEvent(b *turnBuffer, name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ev := range b.events {
		if ev.Name == name {
			return true
		}
	}
	return false
}

// TestStoppedTurnRecordsTheRunningStepsSpend: a Stop while a step's tool runs
// ends the turn cancelled, and its turn_metrics row includes that step's
// completion, which the provider billed before the tool started.
func TestStoppedTurnRecordsTheRunningStepsSpend(t *testing.T) {
	srv, st := newSpendHarness(t, fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.BashStep("call-1", "sleep 30"),
		fakellm.TextStep("Not reached."),
	}})
	postDone := make(chan string, 1)
	go func() {
		postDone <- postChatRequest(t, srv, map[string]any{"message": "wait a while", "model": spendTestModel}).Body.String()
	}()
	var convID string
	waitForCond(t, 10*time.Second, func() bool {
		srv.inflightMu.Lock()
		defer srv.inflightMu.Unlock()
		for id, e := range srv.inflight {
			if e.buf != nil && bufferHasEvent(e.buf, "tool.call") {
				convID = id
				return true
			}
		}
		return false
	})
	if convID == "" {
		t.Fatal("the turn's tool never started")
	}
	if rr := do(t, srv.Routes(), http.MethodPost, "/conversations/"+convID+"/cancel", nil, "u@x.com"); rr.Code != http.StatusNoContent {
		t.Fatalf("cancel: status %d, want 204", rr.Code)
	}
	var body string
	select {
	case body = <-postDone:
	case <-time.After(15 * time.Second):
		t.Fatal("chat POST did not return after the Stop")
	}
	if !strings.Contains(body, "event: turn.cancelled") {
		t.Fatalf("stream missing turn.cancelled:\n%s", body)
	}
	rows := st.recordedTurnMetrics()
	if len(rows) != 1 {
		t.Fatalf("RecordTurn rows = %d, want 1", len(rows))
	}
	assertTurnMetric(t, rows[0], 1, true)
}

// slowTitleEngine is fakeEngine with an auto-title call that blocks until the
// test releases it.
type slowTitleEngine struct {
	fakeEngine
	entered chan struct{}
	release chan struct{}
}

func (e *slowTitleEngine) SuggestTitle(ctx context.Context, _, _ string) string {
	close(e.entered)
	select {
	case <-e.release:
	case <-ctx.Done():
	}
	return ""
}

// TestCompletedTurnRecordsSpendBeforePostTurnWork: a finished turn's row is
// written as soon as the turn finishes, not after the post-turn work (here a
// slow auto-title) — a crash in that window would otherwise lose it, and a
// budget check would see it late.
func TestCompletedTurnRecordsSpendBeforePostTurnWork(t *testing.T) {
	t.Parallel()
	engine := &slowTitleEngine{entered: make(chan struct{}), release: make(chan struct{})}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, engine, st)
	srv.cfg.AutoTitle = true
	postDone := make(chan struct{})
	go func() {
		defer close(postDone)
		postChatRequest(t, srv, map[string]any{"message": "hello there", "model": spendTestModel})
	}()
	select {
	case <-engine.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never reached auto-title")
	}
	rows := st.recordedTurnMetrics()
	close(engine.release)
	<-postDone
	if len(rows) != 1 {
		t.Fatalf("RecordTurn rows while auto-title ran = %d, want 1", len(rows))
	}
	if after := st.recordedTurnMetrics(); len(after) != 1 {
		t.Fatalf("RecordTurn rows after the turn = %d, want 1 (exactly once)", len(after))
	}
}
