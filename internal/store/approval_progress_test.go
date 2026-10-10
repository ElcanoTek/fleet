package store

import (
	"context"
	"testing"
)

// Progress is written only onto the owner's executing row (approved, the
// executing sentinel, no outcome yet), read back by the approval reads the
// web uses, and refused once the outcome is recorded.
func TestSetApprovalProgress(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	a, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_x_execute_plan", "call_1", `{}`, 0, ApprovalSeat{Server: "x"})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	const p1 = `{"progress":1,"total":24,"updated_at":1}`
	if ok, err := s.SetApprovalProgress(ctx, "alice@example.com", a.ID, p1); err != nil || ok {
		t.Fatalf("a pending row: ok=%v err=%v, want refused", ok, err)
	}
	if claimed, err := s.ClaimApproval(ctx, "alice@example.com", a.ID, "approved", ApprovalExecutingSentinel); err != nil || !claimed {
		t.Fatalf("ClaimApproval: %v %v", claimed, err)
	}
	if ok, err := s.SetApprovalProgress(ctx, "bob@example.com", a.ID, p1); err != nil || ok {
		t.Fatalf("another user's write: ok=%v err=%v, want refused", ok, err)
	}
	if ok, err := s.SetApprovalProgress(ctx, "alice@example.com", a.ID, p1); err != nil || !ok {
		t.Fatalf("executing row: ok=%v err=%v", ok, err)
	}
	if got, _ := s.GetApproval(ctx, "alice@example.com", a.ID); got == nil || got.ProgressJSON != p1 {
		t.Fatalf("GetApproval progress = %+v", got)
	}
	if rows, _ := s.ListResolvedApprovals(ctx, "alice@example.com", conv.ID); len(rows) != 1 || rows[0].ProgressJSON != p1 {
		t.Fatalf("ListResolvedApprovals = %+v", rows)
	}
	if rows, _ := s.ListExecutingApprovals(ctx, "alice@example.com", conv.ID, ApprovalExecutingSentinel); len(rows) != 1 || rows[0].ProgressJSON != p1 {
		t.Fatalf("ListExecutingApprovals = %+v", rows)
	}
	if err := s.SetApprovalResult(ctx, "alice@example.com", a.ID, "done", false); err != nil {
		t.Fatalf("SetApprovalResult: %v", err)
	}
	if ok, err := s.SetApprovalProgress(ctx, "alice@example.com", a.ID, `{"progress":2,"updated_at":2}`); err != nil || ok {
		t.Fatalf("a settled row: ok=%v err=%v, want refused", ok, err)
	}
}
