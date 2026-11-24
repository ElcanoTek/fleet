package store

import (
	"context"
	"testing"
)

// An email card's Private choice is claimed and recorded in one write; it
// comes back on the reads the approval path and the reload use, and a card
// approved without one keeps none.
func TestClaimApprovalPrivate(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	seedAccount(t, s, "alice@example.com")
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	const staged = `{"to_email":"a@client.example","subject":"s","content":"c"}`
	for i, private := range []bool{true, false} {
		a, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_ses_outbound_send_email", "call_"+string(rune('a'+i)), staged, 0, ApprovalSeat{Server: "ses_outbound"})
		if err != nil {
			t.Fatalf("CreateApproval: %v", err)
		}
		if ok, err := s.ClaimApprovalPrivate(ctx, "bob@example.com", a.ID, ApprovalExecutingSentinel, private); err != nil || ok {
			t.Fatalf("another user's claim: ok=%v err=%v, want refused", ok, err)
		}
		if ok, err := s.ClaimApprovalPrivate(ctx, "alice@example.com", a.ID, ApprovalExecutingSentinel, private); err != nil || !ok {
			t.Fatalf("ClaimApprovalPrivate(%v): ok=%v err=%v", private, ok, err)
		}
		if ok, err := s.ClaimApprovalPrivate(ctx, "alice@example.com", a.ID, ApprovalExecutingSentinel, !private); err != nil || ok {
			t.Fatalf("a second claim: ok=%v err=%v, want lost", ok, err)
		}
		got, err := s.GetApproval(ctx, "alice@example.com", a.ID)
		if err != nil || got == nil || got.PrivateChoice == nil || *got.PrivateChoice != private || got.Status != "approved" || got.ArgsJSON != staged {
			t.Fatalf("GetApproval = %+v err=%v, want approved with private_choice %v", got, err, private)
		}
	}
	executing, err := s.ListExecutingApprovals(ctx, "alice@example.com", conv.ID, ApprovalExecutingSentinel)
	if err != nil || len(executing) != 2 || executing[0].PrivateChoice == nil || executing[1].PrivateChoice == nil {
		t.Fatalf("ListExecutingApprovals = %+v err=%v", executing, err)
	}

	// A card approved whole records no choice.
	plain, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_ses_outbound_send_email", "call_z", staged, 0, ApprovalSeat{Server: "ses_outbound"})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if ok, err := s.ClaimApproval(ctx, "alice@example.com", plain.ID, "approved", ApprovalExecutingSentinel); err != nil || !ok {
		t.Fatalf("ClaimApproval: ok=%v err=%v", ok, err)
	}
	if got, err := s.GetApproval(ctx, "alice@example.com", plain.ID); err != nil || got.PrivateChoice != nil {
		t.Fatalf("whole claim private_choice = %v err=%v, want none", got.PrivateChoice, err)
	}
}
