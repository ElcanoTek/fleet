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

	requeued, completed, err := s.RecoverInputQueue(ctx)
	if err != nil {
		t.Fatalf("RecoverInputQueue: %v", err)
	}
	if requeued != 1 || completed != 1 {
		t.Fatalf("requeued=%d completed=%d, want 1/1", requeued, completed)
	}
	items, _ := s.ListQueuedInputs(ctx, "u@example.com", convID)
	if len(items) != 1 || items[0].ID != b.ID || items[0].State != InputStateQueued {
		t.Fatalf("recovery state wrong: %+v", items)
	}
	_ = a
}
