package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// TestClaimApproval_OnlyOneWinner is the regression test for the
// double-send TOCTOU: the HTTP handler used to check Status != "pending"
// in memory, run the staged tool, and only then flip the row — so two
// concurrent approve requests both fired the email. The claim must be
// the atomic gate, and exactly one caller may win it.
func TestClaimApproval_OnlyOneWinner(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	a, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_sendgrid_send_email", "call_1", `{}`, 0, ApprovalSeat{})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}

	const racers = 8
	var wg sync.WaitGroup
	wins := make(chan bool, racers)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := s.ClaimApproval(ctx, "alice@example.com", a.ID, "approved", "executing")
			if err != nil {
				t.Errorf("ClaimApproval: %v", err)
				return
			}
			wins <- claimed
		}()
	}
	wg.Wait()
	close(wins)

	won := 0
	for c := range wins {
		if c {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("expected exactly 1 claim winner, got %d", won)
	}

	got, err := s.GetApproval(ctx, "alice@example.com", a.ID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if got.Status != "approved" {
		t.Fatalf("status = %q, want approved", got.Status)
	}
}

func TestClaimApproval_UserScopedAndValidated(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	a, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_sendgrid_send_email", "call_1", `{}`, 0, ApprovalSeat{})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}

	if _, err := s.ClaimApproval(ctx, "alice@example.com", a.ID, "maybe", "x"); err == nil {
		t.Fatal("invalid status should be rejected")
	}
	if claimed, err := s.ClaimApproval(ctx, "mallory@example.com", a.ID, "approved", "x"); err != nil || claimed {
		t.Fatalf("cross-user claim must not succeed (claimed=%v err=%v)", claimed, err)
	}
}

func TestSetApprovalResult_UpdatesClaimedRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	a, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_sendgrid_send_email", "call_1", `{}`, 0, ApprovalSeat{})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}

	// Pending rows must not be touched — result text belongs to a claim.
	if err := s.SetApprovalResult(ctx, "alice@example.com", a.ID, "too early"); err != nil {
		t.Fatalf("SetApprovalResult: %v", err)
	}
	got, _ := s.GetApproval(ctx, "alice@example.com", a.ID)
	if got.ResultText == "too early" {
		t.Fatal("SetApprovalResult must not write to a pending approval")
	}

	if claimed, err := s.ClaimApproval(ctx, "alice@example.com", a.ID, "approved", "executing"); err != nil || !claimed {
		t.Fatalf("claim failed (claimed=%v err=%v)", claimed, err)
	}
	if err := s.SetApprovalResult(ctx, "alice@example.com", a.ID, "sent ok"); err != nil {
		t.Fatalf("SetApprovalResult: %v", err)
	}
	got, _ = s.GetApproval(ctx, "alice@example.com", a.ID)
	if got.ResultText != "sent ok" {
		t.Fatalf("result_text = %q, want %q", got.ResultText, "sent ok")
	}
}

// TestClaimApprovalAndSetModel_AtomicAndRetryable pins the suggest_advanced
// resolution's contract: the approved claim and the model pin land together
// or not at all. A pin that cannot land (here: a conversation id that is not
// the user's) rolls the claim back — the approval is still pending and the
// model untouched — so the same call can be retried and then wins.
func TestClaimApprovalAndSetModel_AtomicAndRetryable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const user = "alice@example.com"

	conv, err := s.CreateConversation(ctx, user, "t", "victoria", "cheap/model", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	a, err := s.CreateApproval(ctx, conv.ID, user, "suggest_advanced_model", "call_1", `{}`, 0, ApprovalSeat{})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}

	// The pin fails: wrong conversation. Nothing may have changed.
	claimed, err := s.ClaimApprovalAndSetModel(ctx, user, a.ID, "pinned", "not-a-conversation", "advanced/model")
	if claimed || !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("bad conversation: claimed=%v err=%v, want false/ErrConversationNotFound", claimed, err)
	}
	got, err := s.GetApproval(ctx, user, a.ID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if got.Status != "pending" {
		t.Fatalf("after a failed pin the approval is %q; the claim must have rolled back", got.Status)
	}
	c, err := s.Get(ctx, user, conv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.Model != "cheap/model" {
		t.Fatalf("model = %q after a failed claim; want unchanged", c.Model)
	}

	// The retry wins and both rows change.
	claimed, err = s.ClaimApprovalAndSetModel(ctx, user, a.ID, "pinned", conv.ID, "advanced/model")
	if err != nil || !claimed {
		t.Fatalf("retry: claimed=%v err=%v, want true/nil", claimed, err)
	}
	got, _ = s.GetApproval(ctx, user, a.ID)
	if got.Status != "approved" || got.ResultText != "pinned" {
		t.Fatalf("approval = %q/%q, want approved/pinned", got.Status, got.ResultText)
	}
	c, _ = s.Get(ctx, user, conv.ID)
	if c.Model != "advanced/model" {
		t.Fatalf("model = %q, want advanced/model", c.Model)
	}

	// A second resolution loses the claim without touching anything.
	claimed, err = s.ClaimApprovalAndSetModel(ctx, user, a.ID, "again", conv.ID, "other/model")
	if claimed || err != nil {
		t.Fatalf("already resolved: claimed=%v err=%v, want false/nil", claimed, err)
	}
	c, _ = s.Get(ctx, user, conv.ID)
	if c.Model != "advanced/model" {
		t.Fatalf("a lost claim changed the model to %q", c.Model)
	}
}
