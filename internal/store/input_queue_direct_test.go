package store

// Direct-turn idempotency claims (migrations 064 and 065): a submission that started a
// turn directly records its input_id in the queue's key space, so a resend of
// the same key is recognised instead of run twice.

import (
	"context"
	"fmt"
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
	if _, err := s.BindInputTurn(ctx, bound.ID, "t1"); err != nil {
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
	if _, err := s.BindInputTurn(ctx, ran.ID, "t1"); err != nil {
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
	if _, err := s.BindInputTurn(ctx, failed.ID, "t2"); err != nil {
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
	if _, err := s.BindInputTurn(ctx, ran.ID, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "do it")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTurn(ctx, "t2", convID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	died, _ := claimDirect(t, s, convID, "died")
	if _, err := s.BindInputTurn(ctx, died.ID, "t2"); err != nil {
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

// ReleaseDirectInput on a claim already bound to its (aborted) turn cannot
// drop it as unbound; it settles it cancelled, since nothing ran.
func TestReleaseDirectInput_SettlesABoundClaim(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t-rel")
	row, _ := claimDirect(t, s, convID, "bound-1")
	if _, err := s.BindInputTurn(ctx, row.ID, "aborted-turn"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseDirectInput(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.LookupInput(ctx, convID, "bound-1")
	if err != nil || got == nil || got.State != InputStateCancelled {
		t.Fatalf("row = %+v, %v: want the bound claim settled cancelled", got, err)
	}
	unbound, _ := claimDirect(t, s, convID, "unbound-1")
	if err := s.ReleaseDirectInput(ctx, unbound.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LookupInput(ctx, convID, "unbound-1"); err != nil || got != nil {
		t.Fatalf("row = %+v, %v: an unbound claim is dropped", got, err)
	}
}

// CancelUnlaunchedInput cancels only a claimed row not yet bound to a turn —
// a direct claim with no turn id, or a drained row holding its placeholder —
// and the bind that follows then finds it no longer running.
func TestCancelUnlaunchedInput_OnlyBeforeTheBind(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t-cub")

	unbound, _ := claimDirect(t, s, convID, "cub-1")
	if ok, err := s.CancelUnlaunchedInput(ctx, unbound.ID); err != nil || !ok {
		t.Fatalf("cancel unbound = %v, %v; want cancelled", ok, err)
	}
	if bound, err := s.BindInputTurn(ctx, unbound.ID, "t-cub"); err != nil || bound {
		t.Fatalf("bind after cancel = %v, %v; want refused", bound, err)
	}

	bound, _ := claimDirect(t, s, convID, "cub-2")
	if _, err := s.BindInputTurn(ctx, bound.ID, "t-cub"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CancelUnlaunchedInput(ctx, bound.ID); err != nil || ok {
		t.Fatalf("cancel bound = %v, %v; want left alone", ok, err)
	}
	if got, _ := s.LookupInput(ctx, convID, "cub-2"); got == nil || got.State != InputStateRunning {
		t.Fatalf("bound claim = %+v, want still running", got)
	}

	drain := func(key string) *InputQueueRow {
		t.Helper()
		if _, _, err := s.EnqueueInput(ctx, InputQueueRow{
			ID: "q-" + key, ConversationID: convID, UserEmail: "u@example.com",
			ClientInputID: key, Message: "later", Attachments: "[]", Mode: InputModeQueued,
		}); err != nil {
			t.Fatal(err)
		}
		row, err := s.ClaimNextQueuedInput(ctx, convID, ClaimTurnPrefix+key)
		if err != nil || row == nil {
			t.Fatalf("drain claim = %+v, %v", row, err)
		}
		return row
	}
	placeholder := drain("cub-3")
	if ok, err := s.CancelUnlaunchedInput(ctx, placeholder.ID); err != nil || !ok {
		t.Fatalf("cancel drained placeholder = %v, %v; want cancelled", ok, err)
	}
	launched := drain("cub-4")
	if _, err := s.BindInputTurn(ctx, launched.ID, "t-cub"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CancelUnlaunchedInput(ctx, launched.ID); err != nil || ok {
		t.Fatalf("cancel drained bound = %v, %v; want left alone", ok, err)
	}
}

// CancelInputKey takes a free key with a cancelled row that a later claim or
// enqueue of the key finds instead of running, and leaves a held key alone.
func TestCancelInputKey_TakesOnlyAFreeKey(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t-cik")

	tomb, created, err := s.CancelInputKey(ctx, InputQueueRow{ID: "tomb-1", ConversationID: convID, UserEmail: "u@example.com", ClientInputID: "cik-1"})
	if err != nil || !created || tomb.State != InputStateCancelled || tomb.Mode != InputModeDirect {
		t.Fatalf("tombstone = %+v created=%v err=%v", tomb, created, err)
	}
	if row, created := claimDirect(t, s, convID, "cik-1"); created || row.ID != "tomb-1" {
		t.Fatalf("claim after the Stop = %+v created=%v, want the cancelled row", row, created)
	}
	if row, created, err := s.EnqueueInput(ctx, InputQueueRow{ID: "q-cik-1", ConversationID: convID, UserEmail: "u@example.com", ClientInputID: "cik-1", Message: "later", Attachments: "[]", Mode: InputModeQueued}); err != nil || created || row.ID != "tomb-1" {
		t.Fatalf("enqueue after the Stop = %+v created=%v err=%v, want the cancelled row", row, created, err)
	}
	if got, _ := s.LookupInputForUser(ctx, "u@example.com", "cik-1"); got == nil || got.ID != "tomb-1" {
		t.Fatalf("per-user lookup = %+v, want the cancelled row", got)
	}
	if items, _ := s.ListQueuedInputs(ctx, "u@example.com", convID); len(items) != 0 {
		t.Fatalf("a Stop's row shows in the queue: %+v", items)
	}

	held, _ := claimDirect(t, s, convID, "cik-2")
	row, created, err := s.CancelInputKey(ctx, InputQueueRow{ID: "tomb-2", ConversationID: convID, UserEmail: "u@example.com", ClientInputID: "cik-2"})
	if err != nil || created || row.ID != held.ID || row.State != InputStateRunning {
		t.Fatalf("held key = %+v created=%v err=%v, want the claim returned unchanged", row, created, err)
	}
}

// CancelStoppedSteer cancels an injected or re-queued steer and never one
// its turn's settlement recorded completed.
func TestCancelStoppedSteer_NeverOverwritesACompletedSteer(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t-css")
	for i, state := range []string{InputStateInjected, InputStateQueued, InputStateCompleted} {
		key := fmt.Sprintf("css-%d", i)
		row, _, err := s.EnqueueInput(ctx, InputQueueRow{ID: "q-" + key, ConversationID: convID, UserEmail: "u@example.com", ClientInputID: key, Message: "steer", Attachments: "[]", Mode: InputModeSteer})
		if err != nil {
			t.Fatal(err)
		}
		if state != InputStateQueued {
			if err := s.MarkInputTerminal(ctx, row.ID, state); err != nil {
				t.Fatal(err)
			}
		}
		ok, err := s.CancelStoppedSteer(ctx, row.ID)
		got, _ := s.LookupInput(ctx, convID, key)
		wantOK := state != InputStateCompleted
		if err != nil || ok != wantOK || (wantOK && got.State != InputStateCancelled) || (!wantOK && got.State != InputStateCompleted) {
			t.Fatalf("%s: cancelled=%v err=%v state=%s", state, ok, err, got.State)
		}
	}
}
