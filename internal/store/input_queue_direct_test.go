package store

// Direct-turn idempotency claims (migration 063): a submission that started a
// turn directly records its input_id in the queue's key space, so a resend of
// the same key is recognised instead of run twice.

import (
	"context"
	"testing"
	"time"
)

func claimDirect(t *testing.T, s *Store, convID, clientID string) (InputQueueRow, bool) {
	t.Helper()
	row, created, err := s.ClaimDirectInput(context.Background(), InputQueueRow{
		ID: "d-" + clientID + "-" + time.Now().Format("150405.000000000"), ConversationID: convID,
		UserEmail: "u@example.com", ClientInputID: clientID, Message: "do it", Attachments: "[]",
	})
	if err != nil {
		t.Fatalf("ClaimDirectInput: %v", err)
	}
	return row, created
}

func TestClaimDirectInput_SharesTheQueueKeySpace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")

	first, created := claimDirect(t, s, convID, "k1")
	if !created || first.Mode != InputModeDirect || first.State != InputStateRunning {
		t.Fatalf("first claim = %+v created=%v", first, created)
	}
	// The same key again, by either path, finds the existing claim.
	again, created := claimDirect(t, s, convID, "k1")
	if created || again.ID != first.ID {
		t.Fatalf("second direct claim created=%v id=%s, want the first row", created, again.ID)
	}
	q, created, err := s.EnqueueInput(ctx, InputQueueRow{
		ID: "q-k1", ConversationID: convID, UserEmail: "u@example.com",
		ClientInputID: "k1", Message: "do it", Attachments: "[]", Mode: InputModeQueued,
	})
	if err != nil || created || q.ID != first.ID {
		t.Fatalf("queue insert of a claimed key: created=%v id=%s err=%v, want the direct row", created, q.ID, err)
	}
	if got, _ := s.LookupInput(ctx, convID, "k1"); got == nil || got.ID != first.ID {
		t.Fatalf("LookupInput = %+v", got)
	}
	// A direct claim is not a queue item.
	items, err := s.ListQueuedInputs(ctx, "u@example.com", convID)
	if err != nil || len(items) != 0 {
		t.Fatalf("queue listing shows %d rows (err=%v), want none", len(items), err)
	}
	if n, _ := s.CountPendingInputs(ctx, convID); n != 0 {
		t.Fatalf("pending = %d", n)
	}
	if row, err := s.ClaimNextQueuedInput(ctx, convID, "t9"); err != nil || row != nil {
		t.Fatalf("drain claimed a direct row: %+v err=%v", row, err)
	}
}

func TestReleaseDirectInput_OnlyAnUnlaunchedClaim(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")

	unbound, _ := claimDirect(t, s, convID, "free")
	if err := s.ReleaseDirectInput(ctx, unbound.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LookupInput(ctx, convID, "free"); got != nil {
		t.Fatal("a released claim must free its key")
	}

	bound, _ := claimDirect(t, s, convID, "bound")
	if err := s.BindInputTurn(ctx, bound.ID, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseDirectInput(ctx, bound.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LookupInput(ctx, convID, "bound"); got == nil {
		t.Fatal("a claim whose turn launched must never be released")
	}
}

func TestSettleDirectInput_CompletedOnlyWhenTheInputRan(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")

	ran, _ := claimDirect(t, s, convID, "ran")
	if err := s.BindInputTurn(ctx, ran.ID, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "do it")); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleDirectInput(ctx, ran.ID, "t1"); err != nil {
		t.Fatal(err)
	}

	if err := s.CreateTurn(ctx, "t2", convID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	failed, _ := claimDirect(t, s, convID, "failed")
	if err := s.BindInputTurn(ctx, failed.ID, "t2"); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleDirectInput(ctx, failed.ID, "t2"); err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]string{"ran": InputStateCompleted, "failed": InputStateCancelled} {
		got, _ := s.LookupInput(ctx, convID, key)
		if got == nil || got.State != want {
			t.Errorf("%s: %+v, want state %s (never re-queued)", key, got, want)
		}
	}
}

// Boot recovery never re-queues a direct input: one whose turn committed its
// user entry completes, and the rest cancel — nothing runs later unattended.
func TestRecoverInputQueue_SettlesDirectClaims(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")

	ran, _ := claimDirect(t, s, convID, "ran")
	if err := s.BindInputTurn(ctx, ran.ID, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "do it")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTurn(ctx, "t2", convID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	died, _ := claimDirect(t, s, convID, "died")
	if err := s.BindInputTurn(ctx, died.ID, "t2"); err != nil {
		t.Fatal(err)
	}

	requeued, completed, cancelled, err := s.RecoverInputQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 0 || completed != 1 || cancelled != 1 {
		t.Fatalf("requeued=%d completed=%d cancelled=%d, want 0/1/1", requeued, completed, cancelled)
	}
	if got, _ := s.LookupInput(ctx, convID, "died"); got == nil || got.State != InputStateCancelled {
		t.Fatalf("uncommitted direct claim = %+v, want cancelled", got)
	}
}

func TestLookupInputForUser_FindsTheKeyAcrossConversations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	claimed, _ := claimDirect(t, s, convID, "first-key")

	got, err := s.LookupInputForUser(ctx, "u@example.com", "first-key")
	if err != nil || got == nil || got.ID != claimed.ID || got.ConversationID != convID {
		t.Fatalf("got %+v err=%v, want the claim in %s", got, err, convID)
	}
	if other, _ := s.LookupInputForUser(ctx, "someone@else.com", "first-key"); other != nil {
		t.Fatal("another user's key must not be found")
	}
}
