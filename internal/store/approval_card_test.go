package store

import (
	"context"
	"testing"
)

// A describer's card is stored on a pending row only, comes back on every
// approval read, and is absent (empty) for a row that never got one.
func TestSetApprovalCard(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	a, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_deals_update_deal", "call_1", `{"deal_id":"d1"}`, 0, ApprovalSeat{Server: "deals"})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	plain, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_pages_deploy_page", "call_2", `{}`, 0, ApprovalSeat{Server: "pages"})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	const card = `{"title":"Update 1 deal","items":[{"label":"Q4 Video"}]}`

	if ok, err := s.SetApprovalCard(ctx, "bob@example.com", a.ID, card); err != nil || ok {
		t.Fatalf("another user's write: ok=%v err=%v, want refused", ok, err)
	}
	if ok, err := s.SetApprovalCard(ctx, "alice@example.com", a.ID, card); err != nil || !ok {
		t.Fatalf("SetApprovalCard: ok=%v err=%v", ok, err)
	}
	got, err := s.GetApproval(ctx, "alice@example.com", a.ID)
	if err != nil || got == nil || got.CardJSON != card {
		t.Fatalf("GetApproval card = %+v err=%v, want the stored card", got, err)
	}
	pending, err := s.ListPendingApprovals(ctx, "alice@example.com", conv.ID)
	if err != nil || len(pending) != 2 {
		t.Fatalf("ListPendingApprovals = %d rows, err=%v", len(pending), err)
	}
	for _, p := range pending {
		want := ""
		if p.ID == a.ID {
			want = card
		}
		if p.CardJSON != want {
			t.Errorf("pending %s card = %q, want %q", p.ToolName, p.CardJSON, want)
		}
	}

	// Once resolved, the card stays readable (the resolved card keeps its
	// title) and can no longer be written.
	if claimed, err := s.ClaimApproval(ctx, "alice@example.com", a.ID, "rejected", "User declined this action."); err != nil || !claimed {
		t.Fatalf("ClaimApproval: claimed=%v err=%v", claimed, err)
	}
	if ok, err := s.SetApprovalCard(ctx, "alice@example.com", a.ID, `{"title":"late"}`); err != nil || ok {
		t.Fatalf("write on a resolved row: ok=%v err=%v, want refused", ok, err)
	}
	resolved, err := s.ListResolvedApprovals(ctx, "alice@example.com", conv.ID)
	if err != nil || len(resolved) != 1 || resolved[0].CardJSON != card {
		t.Fatalf("ListResolvedApprovals = %+v err=%v, want the original card", resolved, err)
	}
	if got, _ := s.GetApproval(ctx, "alice@example.com", plain.ID); got.CardJSON != "" {
		t.Fatalf("a row without a card reads %q", got.CardJSON)
	}
}
