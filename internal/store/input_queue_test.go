package store

// #785 input-queue store tests: idempotent enqueue, atomic FIFO claims,
// lifecycle guards, and boot recovery against the #798 durable record.

import (
	"context"
	"testing"
	"time"
)

func enqueue(t *testing.T, s *Store, convID, clientID, msg, mode string) InputQueueRow {
	t.Helper()
	row, created, err := s.EnqueueInput(context.Background(), InputQueueRow{
		ID: "iq-" + clientID, ConversationID: convID, UserEmail: "u@example.com",
		ClientInputID: clientID, Message: msg, Attachments: "[]", Mode: mode,
	})
	if err != nil {
		t.Fatalf("EnqueueInput: %v", err)
	}
	if !created {
		t.Fatalf("expected a fresh row for %s", clientID)
	}
	return row
}

func TestEnqueueInput_IdempotentOnClientID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")

	first := enqueue(t, s, convID, "cli-1", "hello", InputModeQueued)
	replay, created, err := s.EnqueueInput(ctx, InputQueueRow{
		ID: "iq-other", ConversationID: convID, UserEmail: "u@example.com",
		ClientInputID: "cli-1", Message: "hello", Attachments: "[]", Mode: InputModeQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("replayed client_input_id must not create a second row")
	}
	if replay.ID != first.ID {
		t.Fatalf("replay returned a different row: %s vs %s", replay.ID, first.ID)
	}
	items, err := s.ListQueuedInputs(ctx, "u@example.com", convID)
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %d err=%v, want exactly 1", len(items), err)
	}
}

func TestClaimNextQueuedInput_FIFOAndExactlyOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	enqueue(t, s, convID, "cli-1", "first", InputModeQueued)
	enqueue(t, s, convID, "cli-2", "second", InputModeQueued)

	a, err := s.ClaimNextQueuedInput(ctx, convID, "turn-a")
	if err != nil || a == nil || a.Message != "first" {
		t.Fatalf("claim 1 = %+v err=%v, want the FIFO head", a, err)
	}
	b, err := s.ClaimNextQueuedInput(ctx, convID, "turn-b")
	if err != nil || b == nil || b.Message != "second" {
		t.Fatalf("claim 2 = %+v err=%v", b, err)
	}
	c, err := s.ClaimNextQueuedInput(ctx, convID, "turn-c")
	if err != nil || c != nil {
		t.Fatalf("claim 3 = %+v err=%v, want empty queue", c, err)
	}
}

func TestQueueLifecycleGuards(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	row := enqueue(t, s, convID, "cli-1", "steer me", InputModeSteer)

	// Injected flips only from queued.
	ok, err := s.MarkInputInjected(ctx, row.ID, "t1")
	if err != nil || !ok {
		t.Fatalf("inject: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.MarkInputInjected(ctx, row.ID, "t1"); ok {
		t.Fatal("second inject must lose the guard")
	}
	// Remove only affects queued rows.
	if ok, _ := s.RemoveQueuedInput(ctx, "u@example.com", convID, row.ID); ok {
		t.Fatal("remove of an injected row must refuse")
	}
	// Terminal commit settles injected rows.
	if err := s.CompleteInjectedInputs(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	items, _ := s.ListQueuedInputs(ctx, "u@example.com", convID)
	if len(items) != 0 {
		t.Fatalf("completed rows still listed: %+v", items)
	}
}

func TestCancelQueuedInputs_CoversQueueOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	enqueue(t, s, convID, "cli-1", "a", InputModeQueued)
	enqueue(t, s, convID, "cli-2", "b", InputModeQueued)
	claimed, _ := s.ClaimNextQueuedInput(ctx, convID, "turn-a")

	n, err := s.CancelQueuedInputs(ctx, "u@example.com", convID)
	if err != nil || n != 1 {
		t.Fatalf("cancelled %d err=%v, want 1 (the still-queued row)", n, err)
	}
	items, _ := s.ListQueuedInputs(ctx, "u@example.com", convID)
	if len(items) != 1 || items[0].ID != claimed.ID {
		t.Fatalf("running row must survive Stop: %+v", items)
	}
}

func TestRecoverInputQueue_ResolvesAgainstDurableRecord(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")

	// Row A: claimed by a turn whose user entry committed (#798) — its text
	// is durably in history, so recovery completes it.
	a := enqueue(t, s, convID, "cli-a", "landed", InputModeQueued)
	if _, err := s.ClaimNextQueuedInput(ctx, convID, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "landed")); err != nil {
		t.Fatal(err)
	}

	// Row B: claimed by a turn that died before the user entry committed —
	// recovery re-queues it.
	if err := s.CreateTurn(ctx, "t2", convID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	b := enqueue(t, s, convID, "cli-b", "lost turn", InputModeQueued)
	if _, err := s.ClaimNextQueuedInput(ctx, convID, "t2"); err != nil {
		t.Fatal(err)
	}

	requeued, completed, cancelled, err := s.RecoverInputQueue(ctx)
	if err != nil {
		t.Fatalf("RecoverInputQueue: %v", err)
	}
	if requeued != 1 || completed != 1 || cancelled != 0 {
		t.Fatalf("requeued=%d completed=%d cancelled=%d, want 1/1/0", requeued, completed, cancelled)
	}
	items, _ := s.ListQueuedInputs(ctx, "u@example.com", convID)
	if len(items) != 1 || items[0].ID != b.ID || items[0].State != InputStateQueued {
		t.Fatalf("recovery state wrong: %+v", items)
	}
	_ = a
}

// steerIntent journals one pre-dispatch tool intent on turn t1 (the
// turn_journal_test.go journalIntent helper with the fields the watermark
// tests don't vary).
func steerIntent(t *testing.T, s *Store, seq int64, callID string) {
	t.Helper()
	journalIntent(t, s, "t1", seq, callID, "send_email", "{}")
}

// #823: an injected steer of a failed (history-uncommitted) turn re-queues
// ONLY when nothing dispatched after the injection. Tools that ran BEFORE the
// steer was injected are below the watermark and must not block the requeue —
// the durable next-turn fallback stays intact where it is provably safe.
func TestSettleTurnInputs_SteerRequeuedWhenNoPostInjectionIntent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")

	// Two tools dispatched BEFORE the steer was injected.
	steerIntent(t, s, 1, "call-1")
	steerIntent(t, s, 2, "call-2")
	row := enqueue(t, s, convID, "cli-steer", "change of plans", InputModeSteer)
	if ok, err := s.MarkInputInjected(ctx, row.ID, "t1"); err != nil || !ok {
		t.Fatalf("inject: ok=%v err=%v", ok, err)
	}

	requeued, cancelled, err := s.SettleTurnInputs(ctx, "t1", "")
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 1 || cancelled != 0 {
		t.Fatalf("requeued=%d cancelled=%d, want 1/0", requeued, cancelled)
	}
	items, _ := s.ListQueuedInputs(ctx, "u@example.com", convID)
	if len(items) != 1 || items[0].ID != row.ID || items[0].State != InputStateQueued {
		t.Fatalf("safe steer was not re-queued: %+v", items)
	}
}

// #823: a tool intent journaled AFTER the injection watermark proves the model
// dispatched with the steer in context — its committed side effects survive
// the failed turn (#820), so re-queuing would re-execute the instruction
// (at-least-once "send that email"). The row must cancel instead.
func TestSettleTurnInputs_SteerCancelledAfterPostInjectionIntent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")

	steerIntent(t, s, 1, "call-1")
	row := enqueue(t, s, convID, "cli-steer", "actually, email Bob instead", InputModeSteer)
	if ok, err := s.MarkInputInjected(ctx, row.ID, "t1"); err != nil || !ok {
		t.Fatalf("inject: ok=%v err=%v", ok, err)
	}
	// Dispatched after the model saw the steer; then the turn fails without a
	// history commit (guardrail block, terminal-commit failure, …).
	steerIntent(t, s, 2, "call-2")

	requeued, cancelled, err := s.SettleTurnInputs(ctx, "t1", "")
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 0 || cancelled != 1 {
		t.Fatalf("requeued=%d cancelled=%d, want 0/1", requeued, cancelled)
	}
	got, err := s.LookupInput(ctx, convID, "cli-steer")
	if err != nil || got == nil {
		t.Fatalf("LookupInput: %+v err=%v", got, err)
	}
	if got.State != InputStateCancelled {
		t.Fatalf("steer state = %s, want cancelled (a requeue would double-execute)", got.State)
	}
}

// #823 at boot: RecoverInputQueue applies the same watermark split to rows a
// dead process left injected on turns that never committed history.
func TestRecoverInputQueue_SteerWatermarkSplit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")

	// Unsafe: a tool dispatched after this steer's injection.
	steerIntent(t, s, 1, "call-1")
	unsafe := enqueue(t, s, convID, "cli-unsafe", "send it to Bob", InputModeSteer)
	if ok, err := s.MarkInputInjected(ctx, unsafe.ID, "t1"); err != nil || !ok {
		t.Fatalf("inject unsafe: ok=%v err=%v", ok, err)
	}
	steerIntent(t, s, 2, "call-2")

	// Safe: injected into a turn that never dispatched afterwards.
	if err := s.CreateTurn(ctx, "t2", convID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	safe := enqueue(t, s, convID, "cli-safe", "one more thing", InputModeSteer)
	if ok, err := s.MarkInputInjected(ctx, safe.ID, "t2"); err != nil || !ok {
		t.Fatalf("inject safe: ok=%v err=%v", ok, err)
	}

	requeued, completed, cancelled, err := s.RecoverInputQueue(ctx)
	if err != nil {
		t.Fatalf("RecoverInputQueue: %v", err)
	}
	if requeued != 1 || completed != 0 || cancelled != 1 {
		t.Fatalf("requeued=%d completed=%d cancelled=%d, want 1/0/1", requeued, completed, cancelled)
	}
	items, _ := s.ListQueuedInputs(ctx, "u@example.com", convID)
	if len(items) != 1 || items[0].ID != safe.ID || items[0].State != InputStateQueued {
		t.Fatalf("recovery split wrong: %+v", items)
	}
}
