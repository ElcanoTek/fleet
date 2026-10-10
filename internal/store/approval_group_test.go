package store

import (
	"context"
	"testing"
)

// The approval group is stored on a pending row the user owns only, comes
// back on every approval read the web and the group endpoint use, and is
// empty for a row that never joined one.
func TestSetApprovalGroup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	a, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_pubmatic_execute_plan", "call_1", `{}`, 0, ApprovalSeat{Server: "pubmatic"})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	plain, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_pages_deploy_page", "call_2", `{}`, 0, ApprovalSeat{Server: "pages"})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	const group = "turn-1"

	if ok, err := s.SetApprovalGroup(ctx, "bob@example.com", a.ID, group); err != nil || ok {
		t.Fatalf("another user's write: ok=%v err=%v, want refused", ok, err)
	}
	if ok, err := s.SetApprovalGroup(ctx, "alice@example.com", a.ID, group); err != nil || !ok {
		t.Fatalf("SetApprovalGroup: ok=%v err=%v", ok, err)
	}
	if got, err := s.GetApproval(ctx, "alice@example.com", a.ID); err != nil || got == nil || got.GroupID != group {
		t.Fatalf("GetApproval group = %+v err=%v, want %q", got, err, group)
	}
	pending, err := s.ListPendingApprovals(ctx, "alice@example.com", conv.ID)
	if err != nil || len(pending) != 2 {
		t.Fatalf("ListPendingApprovals = %d rows, err=%v", len(pending), err)
	}
	for _, p := range pending {
		want := ""
		if p.ID == a.ID {
			want = group
		}
		if p.GroupID != want {
			t.Errorf("pending %s group = %q, want %q", p.ToolName, p.GroupID, want)
		}
	}

	// Once resolved the group stays readable, and a resolved row can never be
	// pulled into a group afterwards.
	if claimed, err := s.ClaimApproval(ctx, "alice@example.com", a.ID, "rejected", "User declined this action."); err != nil || !claimed {
		t.Fatalf("ClaimApproval: claimed=%v err=%v", claimed, err)
	}
	if claimed, err := s.ClaimApproval(ctx, "alice@example.com", plain.ID, "rejected", "User declined this action."); err != nil || !claimed {
		t.Fatalf("ClaimApproval: claimed=%v err=%v", claimed, err)
	}
	if ok, err := s.SetApprovalGroup(ctx, "alice@example.com", plain.ID, group); err != nil || ok {
		t.Fatalf("write on a resolved row: ok=%v err=%v, want refused", ok, err)
	}
	resolved, err := s.ListResolvedApprovals(ctx, "alice@example.com", conv.ID)
	if err != nil || len(resolved) != 2 {
		t.Fatalf("ListResolvedApprovals = %+v err=%v", resolved, err)
	}
	for _, r := range resolved {
		want := ""
		if r.ID == a.ID {
			want = group
		}
		if r.GroupID != want {
			t.Errorf("resolved %s group = %q, want %q", r.ToolName, r.GroupID, want)
		}
	}
}
