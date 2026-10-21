package store

import (
	"context"
	"strings"
	"testing"
)

// A stop is recorded once, only on the owner's row while its call is
// executing, and read back by the reads the running card and the reload use.
func TestMarkApprovalStopRequested(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	a, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_ix_execute_plan", "call_1", `{}`, 0, ApprovalSeat{Server: "ix"})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if ok, err := s.MarkApprovalStopRequested(ctx, "alice@example.com", a.ID); err != nil || ok {
		t.Fatalf("a pending card: ok=%v err=%v, want refused", ok, err)
	}
	if ok, err := s.ClaimApproval(ctx, "alice@example.com", a.ID, "approved", ApprovalExecutingSentinel); err != nil || !ok {
		t.Fatalf("ClaimApproval: %v %v", ok, err)
	}
	if ok, err := s.MarkApprovalStopRequested(ctx, "bob@example.com", a.ID); err != nil || ok {
		t.Fatalf("another user's stop: ok=%v err=%v, want refused", ok, err)
	}
	if ok, err := s.MarkApprovalStopRequested(ctx, "alice@example.com", a.ID); err != nil || !ok {
		t.Fatalf("MarkApprovalStopRequested: ok=%v err=%v", ok, err)
	}
	if ok, err := s.MarkApprovalStopRequested(ctx, "alice@example.com", a.ID); err != nil || ok {
		t.Fatalf("a second stop: ok=%v err=%v, want nothing new", ok, err)
	}
	got, err := s.GetApproval(ctx, "alice@example.com", a.ID)
	if err != nil || got.StopRequestedAt == 0 {
		t.Fatalf("GetApproval stop = %d err=%v", got.StopRequestedAt, err)
	}
	executing, err := s.ListExecutingApprovals(ctx, "alice@example.com", conv.ID, ApprovalExecutingSentinel)
	if err != nil || len(executing) != 1 || executing[0].StopRequestedAt == 0 {
		t.Fatalf("ListExecutingApprovals = %+v err=%v", executing, err)
	}
	if err := s.SetApprovalResult(ctx, "alice@example.com", a.ID, "stopped after 1", false); err != nil {
		t.Fatalf("SetApprovalResult: %v", err)
	}
	resolved, err := s.ListResolvedApprovals(ctx, "alice@example.com", conv.ID)
	if err != nil || len(resolved) != 1 || resolved[0].StopRequestedAt == 0 {
		t.Fatalf("ListResolvedApprovals = %+v err=%v", resolved, err)
	}

	// A settled card takes no stop.
	b, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_ix_execute_plan", "call_2", `{}`, 0, ApprovalSeat{Server: "ix"})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if ok, err := s.ClaimApproval(ctx, "alice@example.com", b.ID, "approved", ApprovalExecutingSentinel); err != nil || !ok {
		t.Fatalf("ClaimApproval: %v %v", ok, err)
	}
	if err := s.SetApprovalResult(ctx, "alice@example.com", b.ID, "done", false); err != nil {
		t.Fatalf("SetApprovalResult: %v", err)
	}
	if ok, err := s.MarkApprovalStopRequested(ctx, "alice@example.com", b.ID); err != nil || ok {
		t.Fatalf("a settled card: ok=%v err=%v, want refused", ok, err)
	}
}

// A stopped call cut off by a restart says in its recovery breadcrumb that
// the person stopped it.
func TestRecoverStrandedApprovals_KeepsTheStop(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	a, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_ix_execute_plan", "call_1", `{}`, 0, ApprovalSeat{Server: "ix"})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if ok, err := s.ClaimApproval(ctx, "alice@example.com", a.ID, "approved", ApprovalExecutingSentinel); err != nil || !ok {
		t.Fatalf("ClaimApproval: %v %v", ok, err)
	}
	if ok, err := s.MarkApprovalStopRequested(ctx, "alice@example.com", a.ID); err != nil || !ok {
		t.Fatalf("MarkApprovalStopRequested: %v %v", ok, err)
	}
	if n, err := s.RecoverStrandedApprovals(ctx); err != nil || n != 1 {
		t.Fatalf("RecoverStrandedApprovals = %d, %v", n, err)
	}
	history, err := s.LoadHistory(ctx, conv.ID)
	if err != nil || len(history) == 0 {
		t.Fatalf("LoadHistory = %d, %v", len(history), err)
	}
	last := string(history[len(history)-1].Content)
	if !strings.Contains(last, "pressed Stop") || !strings.Contains(last, "cannot tell whether the call reached its server") {
		t.Fatalf("recovery breadcrumb = %s, want the neutral stop line", last)
	}
	// Never the running-call line: a stop accepted before the transport was
	// reached sent no request and no cancellation.
	if strings.Contains(last, "Its server was asked to stop") {
		t.Fatalf("recovery breadcrumb = %s claims a cancellation was sent", last)
	}
}
