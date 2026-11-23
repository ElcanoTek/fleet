package store

import (
	"context"
	"testing"
)

// Cards the caller names in keepIDs stay pending; every other pending card for
// the tool is superseded (a batch one turn staged is not a retry).
func TestSupersedePendingApprovals_KeepsNamedCards(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	conv := resumeConv(t, s)
	const tool = "mcp_mg_update_rtb_rule"
	mk := func(call string) *Approval {
		a, err := s.CreateApproval(ctx, conv.ID, resumeUser, tool, call, `{}`, 0, ApprovalSeat{})
		if err != nil {
			t.Fatalf("CreateApproval: %v", err)
		}
		return a
	}
	older, kept1, kept2 := mk("call-0"), mk("call-1"), mk("call-2")

	n, err := s.SupersedePendingApprovals(ctx, resumeUser, conv.ID, tool, kept1.ID, kept2.ID)
	if err != nil || n != 1 {
		t.Fatalf("SupersedePendingApprovals = %d, %v; want exactly the unnamed card", n, err)
	}
	for _, c := range []struct {
		id, want string
	}{{older.ID, "rejected"}, {kept1.ID, "pending"}, {kept2.ID, "pending"}} {
		got, err := s.GetApproval(ctx, resumeUser, c.id)
		if err != nil || got == nil || got.Status != c.want {
			t.Fatalf("approval %s status = %+v, %v; want %s", c.id, got, err, c.want)
		}
	}

	// No keep list: every pending card for the tool goes (the old behavior).
	if n, err := s.SupersedePendingApprovals(ctx, resumeUser, conv.ID, tool); err != nil || n != 2 {
		t.Fatalf("SupersedePendingApprovals without keep = %d, %v; want 2", n, err)
	}
}
