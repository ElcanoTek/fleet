package store

import (
	"context"
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
	a, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_sendgrid_send_email", "call_1", `{}`, 0)
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
	a, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_sendgrid_send_email", "call_1", `{}`, 0)
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
	a, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_sendgrid_send_email", "call_1", `{}`, 0)
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
