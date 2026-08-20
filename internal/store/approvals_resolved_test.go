package store

import (
	"context"
	"testing"
)

// Resolved cards must survive a reload: the conversation GET re-hydrates them
// from this listing so the transcript keeps the shape it had live — including
// a notify-mode record whose undo hint has no other durable delivery (#1153).
func TestListResolvedApprovals_ReturnsResolvedRowsOldestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const user = "alice@example.com"

	conv, err := s.CreateConversation(ctx, user, "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	mk := func(tool, callID string) *Approval {
		t.Helper()
		a, err := s.CreateApproval(ctx, conv.ID, user, tool, callID, `{}`, 0, ApprovalSeat{})
		if err != nil {
			t.Fatalf("CreateApproval(%s): %v", tool, err)
		}
		return a
	}

	pending := mk("bash", "call_p")
	sent := mk("mcp_sendgrid_send_email", "call_s")
	denied := mk("mcp_pages_deploy_page", "call_d")
	if err := s.ResolveApproval(ctx, user, sent.ID, "approved", `{"status_code":202}`); err != nil {
		t.Fatalf("ResolveApproval(sent): %v", err)
	}
	if err := s.ResolveApproval(ctx, user, denied.ID, "rejected", "User declined this action."); err != nil {
		t.Fatalf("ResolveApproval(denied): %v", err)
	}

	got, err := s.ListResolvedApprovals(ctx, user, conv.ID)
	if err != nil {
		t.Fatalf("ListResolvedApprovals: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("resolved rows = %d, want 2 (the pending row must be excluded)", len(got))
	}
	for _, a := range got {
		if a.ID == pending.ID {
			t.Fatal("pending approval leaked into the resolved listing")
		}
	}
	// Oldest first across seconds (created_at is unix-seconds, so same-second
	// rows have no defined tiebreak — display anchors by tool_call_id anyway).
	if got[0].CreatedAt > got[1].CreatedAt {
		t.Errorf("order not oldest-first: created_at %d then %d", got[0].CreatedAt, got[1].CreatedAt)
	}
	byID := map[string]Approval{got[0].ID: got[0], got[1].ID: got[1]}
	s1, ok := byID[sent.ID]
	if !ok {
		t.Fatal("resolved listing missing the approved send")
	}
	if s1.ResultText != `{"status_code":202}` || s1.Status != "approved" {
		t.Errorf("resolved row lost its outcome: status=%q result=%q", s1.Status, s1.ResultText)
	}
	d1, ok := byID[denied.ID]
	if !ok {
		t.Fatal("resolved listing missing the rejected deploy")
	}
	if d1.ToolCallID != "call_d" {
		t.Errorf("tool_call_id = %q, want call_d (reload placement anchor)", d1.ToolCallID)
	}
	if d1.Status != "rejected" {
		t.Errorf("deploy status = %q, want rejected", d1.Status)
	}

	// Scoping: another user's listing must not see these rows.
	other, err := s.ListResolvedApprovals(ctx, "mallory@example.com", conv.ID)
	if err != nil {
		t.Fatalf("ListResolvedApprovals(other user): %v", err)
	}
	if len(other) != 0 {
		t.Errorf("cross-user listing returned %d rows, want 0", len(other))
	}
}
