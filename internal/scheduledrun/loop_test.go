package scheduledrun

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// ── pure exit-condition helpers ──

func TestEvalRegexExit(t *testing.T) {
	if ok, res := evalRegexExit("DONE", "all DONE here"); !ok || res != "regex:matched" {
		t.Errorf("match: ok=%v res=%q", ok, res)
	}
	if ok, res := evalRegexExit("DONE", "still working"); ok || res != "regex:no_match" {
		t.Errorf("no-match: ok=%v res=%q", ok, res)
	}
	if ok, res := evalRegexExit("(", "anything"); ok || res != "regex:invalid" {
		t.Errorf("invalid pattern should not match: ok=%v res=%q", ok, res)
	}
}

func TestFirstWordIsYes(t *testing.T) {
	for _, s := range []string{"YES", "yes", "  Yes, it does", "**YES**", "`yes`", "> YES\nbecause"} {
		if !firstWordIsYes(s) {
			t.Errorf("firstWordIsYes(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"NO", "no", "", "maybe", "not yes"} {
		if firstWordIsYes(s) {
			t.Errorf("firstWordIsYes(%q) = true, want false", s)
		}
	}
}

func TestLastAssistantMessage(t *testing.T) {
	if got := lastAssistantMessage(nil); got != "" {
		t.Errorf("nil session = %q, want empty", got)
	}
	s := &models.LogSession{Messages: []models.LogMessage{
		{Role: "user", Content: "do it"},
		{Role: "assistant", Content: "first"},
		{Role: "user", Content: "again"},
		{Role: "assistant", Content: "final answer"},
	}}
	if got := lastAssistantMessage(s); got != "final answer" {
		t.Errorf("last assistant = %q, want %q", got, "final answer")
	}
	noAsst := &models.LogSession{Messages: []models.LogMessage{{Role: "user", Content: "x"}}}
	if got := lastAssistantMessage(noAsst); got != "" {
		t.Errorf("no assistant = %q, want empty", got)
	}
}

// ── runLoop control ──

type workerOutcome struct {
	session *models.LogSession
	passed  bool
	result  string
	err     error
}

// fakeWorker scripts per-call outcomes (clamping to the last for a "never
// passes" run) and records the extraPrompt fed to each call. Its clock advances
// only when the test's worker closure advances it.
type fakeWorker struct {
	outcomes []workerOutcome
	calls    int
	gotExtra []string
	onCall   func() // optional side effect (e.g. advance a clock)
}

func (f *fakeWorker) run(_ context.Context, extra string) (*models.LogSession, bool, string, error) {
	f.gotExtra = append(f.gotExtra, extra)
	idx := f.calls
	if idx >= len(f.outcomes) {
		idx = len(f.outcomes) - 1
	}
	f.calls++
	if f.onCall != nil {
		f.onCall()
	}
	o := f.outcomes[idx]
	return o.session, o.passed, o.result, o.err
}

// capturing recorder copies each iteration (the loop mutates the pointer across
// running→final, so a snapshot per record() is required).
func capturingRecorder() (func(*models.TaskIteration), *[]models.TaskIteration) {
	var got []models.TaskIteration
	return func(it *models.TaskIteration) { got = append(got, *it) }, &got
}

func sessionWith(text string, cost float64) *models.LogSession {
	return &models.LogSession{
		Cost:     cost,
		Messages: []models.LogMessage{{Role: "assistant", Content: text}},
	}
}

func fixedClock() func() time.Time {
	t := time.Unix(1_700_000_000, 0).UTC()
	return func() time.Time { return t }
}

func countStatus(its []models.TaskIteration, status string) int {
	n := 0
	for _, it := range its {
		if it.Status == status {
			n++
		}
	}
	return n
}

func TestRunLoop_PassesOnSecondIteration(t *testing.T) {
	w := &fakeWorker{outcomes: []workerOutcome{
		{session: sessionWith("attempt one", 0.1), passed: false, result: "regex:no_match"},
		{session: sessionWith("attempt two", 0.1), passed: true, result: "regex:matched"},
	}}
	rec, got := capturingRecorder()
	lc := &models.LoopConfig{MaxIterations: 5, ExitCondition: "regex:matched"}

	session, err := runLoop(context.Background(), lc, uuid.New(), fixedClock(), w.run, rec)
	if err != nil {
		t.Fatalf("expected pass, got err: %v", err)
	}
	if session == nil || lastAssistantMessage(session) != "attempt two" {
		t.Errorf("expected the passing session returned, got %v", session)
	}
	if w.calls != 2 {
		t.Fatalf("worker calls = %d, want 2", w.calls)
	}
	// The first iteration's last assistant message is fed forward to the second.
	if w.gotExtra[0] != "" {
		t.Errorf("first iteration extraPrompt = %q, want empty", w.gotExtra[0])
	}
	if w.gotExtra[1] != "attempt one" {
		t.Errorf("second iteration extraPrompt = %q, want %q (prior output fed forward)", w.gotExtra[1], "attempt one")
	}
	if countStatus(*got, models.IterationStatusPassed) != 1 {
		t.Errorf("want exactly one passed iteration, got records %+v", *got)
	}
}

func TestRunLoop_ExhaustsWithoutPassing(t *testing.T) {
	w := &fakeWorker{outcomes: []workerOutcome{{session: sessionWith("nope", 0), passed: false, result: "regex:no_match"}}}
	rec, _ := capturingRecorder()
	lc := &models.LoopConfig{MaxIterations: 3, ExitCondition: "regex:DONE"}

	_, err := runLoop(context.Background(), lc, uuid.New(), fixedClock(), w.run, rec)
	if err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("expected exhaustion error, got %v", err)
	}
	if w.calls != 3 {
		t.Errorf("worker calls = %d, want 3", w.calls)
	}
}

func TestRunLoop_DefaultMaxIterations(t *testing.T) {
	w := &fakeWorker{outcomes: []workerOutcome{{session: sessionWith("x", 0), passed: false, result: "no"}}}
	rec, _ := capturingRecorder()
	lc := &models.LoopConfig{MaxIterations: 0, ExitCondition: "regex:DONE"} // 0 → default

	_, _ = runLoop(context.Background(), lc, uuid.New(), fixedClock(), w.run, rec)
	if w.calls != models.DefaultLoopMaxIterations {
		t.Errorf("worker calls = %d, want default %d", w.calls, models.DefaultLoopMaxIterations)
	}
}

func TestRunLoop_CostCeiling(t *testing.T) {
	w := &fakeWorker{outcomes: []workerOutcome{{session: sessionWith("x", 0.6), passed: false, result: "no"}}}
	rec, got := capturingRecorder()
	lc := &models.LoopConfig{MaxIterations: 10, ExitCondition: "regex:DONE", MaxCostUSD: 1.0}

	_, err := runLoop(context.Background(), lc, uuid.New(), fixedClock(), w.run, rec)
	if err == nil || !strings.Contains(err.Error(), "cost ceiling") {
		t.Fatalf("expected cost-ceiling error, got %v", err)
	}
	// iter1 (accum 0 → 0.6), iter2 (0.6 → 1.2), then iter3 aborts (1.2 ≥ 1.0).
	if w.calls != 2 {
		t.Errorf("worker calls = %d, want 2 before the ceiling aborts", w.calls)
	}
	if countStatus(*got, models.IterationStatusStopped) != 1 {
		t.Errorf("want one stopped record, got %+v", *got)
	}
}

func TestRunLoop_TimeBudget(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	now := func() time.Time { return clk }
	w := &fakeWorker{outcomes: []workerOutcome{{session: sessionWith("x", 0), passed: false, result: "no"}}}
	// Each worker call burns 6s; with a 10s budget the loop runs twice, then the
	// pre-iteration deadline check aborts the third.
	w.onCall = func() { clk = clk.Add(6 * time.Second) }
	rec, got := capturingRecorder()
	lc := &models.LoopConfig{MaxIterations: 10, ExitCondition: "regex:DONE", TimeBudgetSeconds: 10}

	_, err := runLoop(context.Background(), lc, uuid.New(), now, w.run, rec)
	if err == nil || !strings.Contains(err.Error(), "time budget") {
		t.Fatalf("expected time-budget error, got %v", err)
	}
	if w.calls != 2 {
		t.Errorf("worker calls = %d, want 2 before the deadline aborts", w.calls)
	}
	if countStatus(*got, models.IterationStatusStopped) != 1 {
		t.Errorf("want one stopped record, got %+v", *got)
	}
}

func TestRunLoop_WorkerErrorPropagates(t *testing.T) {
	boom := errors.New("model exploded")
	w := &fakeWorker{outcomes: []workerOutcome{{session: sessionWith("partial", 0), err: boom}}}
	rec, got := capturingRecorder()
	lc := &models.LoopConfig{MaxIterations: 5, ExitCondition: "regex:DONE"}

	_, err := runLoop(context.Background(), lc, uuid.New(), fixedClock(), w.run, rec)
	if !errors.Is(err, boom) {
		t.Fatalf("worker error should propagate, got %v", err)
	}
	if w.calls != 1 {
		t.Errorf("worker calls = %d, want 1 (error stops the loop)", w.calls)
	}
	if countStatus(*got, models.IterationStatusFailed) < 1 {
		t.Errorf("the errored iteration should be recorded failed, got %+v", *got)
	}
}

func TestRunLoop_CancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &fakeWorker{outcomes: []workerOutcome{{session: sessionWith("x", 0), passed: true, result: "ok"}}}
	rec, _ := capturingRecorder()
	lc := &models.LoopConfig{MaxIterations: 5, ExitCondition: "regex:DONE"}

	_, err := runLoop(ctx, lc, uuid.New(), fixedClock(), w.run, rec)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if w.calls != 0 {
		t.Errorf("worker should not run on a cancelled context, got %d calls", w.calls)
	}
}
