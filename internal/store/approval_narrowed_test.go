package store

import (
	"context"
	"testing"
	"time"
)

// A narrowed approval is claimed and recorded in one write, on a pending,
// unexpired row the user owns only; the record comes back on the reads the
// approval path and the reload use, and the staged arguments are unchanged.
func TestClaimApprovalNarrowed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	const staged = `{"deal_ids":["a","b","c"]}`
	const record = `{"arg":"deal_ids","staged":["a","b","c"],"approved":["a","c"]}`
	a, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_ix_execute_plan", "call_1", staged, 0, ApprovalSeat{Server: "ix"})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}

	if _, err := s.ClaimApprovalNarrowed(ctx, "alice@example.com", a.ID, ApprovalExecutingSentinel, ""); err == nil {
		t.Fatal("an empty record must be refused")
	}
	if ok, err := s.ClaimApprovalNarrowed(ctx, "bob@example.com", a.ID, ApprovalExecutingSentinel, record); err != nil || ok {
		t.Fatalf("another user's claim: ok=%v err=%v, want refused", ok, err)
	}
	if ok, err := s.ClaimApprovalNarrowed(ctx, "alice@example.com", a.ID, ApprovalExecutingSentinel, record); err != nil || !ok {
		t.Fatalf("ClaimApprovalNarrowed: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ClaimApprovalNarrowed(ctx, "alice@example.com", a.ID, ApprovalExecutingSentinel, record); err != nil || ok {
		t.Fatalf("a second claim: ok=%v err=%v, want lost", ok, err)
	}
	got, err := s.GetApproval(ctx, "alice@example.com", a.ID)
	if err != nil || got == nil {
		t.Fatalf("GetApproval: %+v %v", got, err)
	}
	if got.Status != "approved" || got.ResultText != ApprovalExecutingSentinel || got.NarrowedJSON != record || got.ArgsJSON != staged {
		t.Fatalf("row = status %q result %q narrowed %q args %q", got.Status, got.ResultText, got.NarrowedJSON, got.ArgsJSON)
	}
	if err := s.SetApprovalResult(ctx, "alice@example.com", a.ID, "done", false); err != nil {
		t.Fatalf("SetApprovalResult: %v", err)
	}
	resolved, err := s.ListResolvedApprovals(ctx, "alice@example.com", conv.ID)
	if err != nil || len(resolved) != 1 || resolved[0].NarrowedJSON != record {
		t.Fatalf("ListResolvedApprovals = %+v err=%v", resolved, err)
	}

	// An expired card cannot be claimed narrowed either: default-deny stands.
	expired, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_ix_execute_plan", "call_2", staged, time.Now().Unix()-1, ApprovalSeat{Server: "ix"})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if ok, err := s.ClaimApprovalNarrowed(ctx, "alice@example.com", expired.ID, ApprovalExecutingSentinel, record); err != nil || ok {
		t.Fatalf("expired claim: ok=%v err=%v, want refused", ok, err)
	}
	// A card approved whole carries no record.
	whole, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_ix_execute_plan", "call_3", staged, 0, ApprovalSeat{Server: "ix"})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if ok, err := s.ClaimApproval(ctx, "alice@example.com", whole.ID, "approved", ApprovalExecutingSentinel); err != nil || !ok {
		t.Fatalf("ClaimApproval: %v %v", ok, err)
	}
	if got, err := s.GetApproval(ctx, "alice@example.com", whole.ID); err != nil || got.NarrowedJSON != "" {
		t.Fatalf("whole approval narrowed = %q err=%v", got.NarrowedJSON, err)
	}
}
