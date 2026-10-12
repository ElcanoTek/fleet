package store

import (
	"context"
	"testing"
)

// The result card is written once, only onto a row whose outcome it can
// describe: an approved call still executing (just before its outcome) or a
// notify-mode record. A pending, declined or settled row, another user's row,
// or a second write is refused; the reads the web uses return it.
func TestSetApprovalResultCard(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const user, card = "alice@example.com", `{"outcome":"done","title":"Created 1 deal","items":[]}`
	conv, err := s.CreateConversation(ctx, user, "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	a, err := s.CreateApproval(ctx, conv.ID, user, "mcp_x_execute_plan", "call_1", `{}`, 0, ApprovalSeat{Server: "x"})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if ok, err := s.SetApprovalResultCard(ctx, user, a.ID, card); err != nil || ok {
		t.Fatalf("a pending row: ok=%v err=%v, want refused", ok, err)
	}
	if claimed, err := s.ClaimApproval(ctx, user, a.ID, "approved", ApprovalExecutingSentinel); err != nil || !claimed {
		t.Fatalf("ClaimApproval: %v %v", claimed, err)
	}
	if ok, err := s.SetApprovalResultCard(ctx, "bob@example.com", a.ID, card); err != nil || ok {
		t.Fatalf("another user's write: ok=%v err=%v, want refused", ok, err)
	}
	if ok, err := s.SetApprovalResultCard(ctx, user, a.ID, card); err != nil || !ok {
		t.Fatalf("executing row: ok=%v err=%v", ok, err)
	}
	if ok, err := s.SetApprovalResultCard(ctx, user, a.ID, `{"other":1}`); err != nil || ok {
		t.Fatalf("a second write: ok=%v err=%v, want refused", ok, err)
	}
	if err := s.SetApprovalResult(ctx, user, a.ID, "done", false); err != nil {
		t.Fatalf("SetApprovalResult: %v", err)
	}
	if got, _ := s.GetApproval(ctx, user, a.ID); got == nil || got.ResultCardJSON != card {
		t.Fatalf("GetApproval result card = %+v", got)
	}
	if rows, _ := s.ListResolvedApprovals(ctx, user, conv.ID); len(rows) != 1 || rows[0].ResultCardJSON != card {
		t.Fatalf("ListResolvedApprovals = %+v", rows)
	}

	// A settled row (no card written while it ran) takes none afterwards.
	b, _ := s.CreateApproval(ctx, conv.ID, user, "mcp_x_execute_plan", "call_2", `{}`, 0, ApprovalSeat{Server: "x"})
	_, _ = s.ClaimApproval(ctx, user, b.ID, "approved", ApprovalExecutingSentinel)
	_ = s.SetApprovalResult(ctx, user, b.ID, "boom", true)
	if ok, err := s.SetApprovalResultCard(ctx, user, b.ID, card); err != nil || ok {
		t.Fatalf("a settled failed row: ok=%v err=%v, want refused", ok, err)
	}
	// A declined row takes none.
	c, _ := s.CreateApproval(ctx, conv.ID, user, "mcp_x_execute_plan", "call_3", `{}`, 0, ApprovalSeat{Server: "x"})
	_ = s.ResolveApproval(ctx, user, c.ID, "rejected", "Declined by user.")
	if ok, err := s.SetApprovalResultCard(ctx, user, c.ID, card); err != nil || ok {
		t.Fatalf("a declined row: ok=%v err=%v, want refused", ok, err)
	}
	// A notify-mode record (approved, is_err false, the record text) takes it.
	d, _ := s.CreateApproval(ctx, conv.ID, user, "mcp_x_next_campaign_id", "call_4", `{}`, 0, ApprovalSeat{Server: "x"})
	_ = s.ResolveApproval(ctx, user, d.ID, "approved", NotifyRecordResultPrefix+": this tool is declared notify-mode in the client bundle.")
	if ok, err := s.SetApprovalResultCard(ctx, user, d.ID, card); err != nil || !ok {
		t.Fatalf("a notify record: ok=%v err=%v", ok, err)
	}
}
