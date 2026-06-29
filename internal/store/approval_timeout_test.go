package store

import (
	"context"
	"testing"
	"time"
)

// TestCreateApproval_PersistsExpiresAt verifies the #225 expires_at column
// round-trips: a positive deadline is stored and surfaced on the returned row
// and on subsequent reads, while a zero deadline persists NULL (→ 0 on read).
func TestCreateApproval_PersistsExpiresAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	deadline := time.Now().Unix() + 300
	a, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "mcp_sendgrid_send_email", "call_1", `{}`, deadline)
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if a.ExpiresAt != deadline {
		t.Errorf("returned ExpiresAt = %d, want %d", a.ExpiresAt, deadline)
	}
	got, err := s.GetApproval(ctx, "alice@example.com", a.ID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if got.ExpiresAt != deadline {
		t.Errorf("GetApproval ExpiresAt = %d, want %d", got.ExpiresAt, deadline)
	}

	// Zero deadline → stored NULL → read back as 0 (no expiry).
	b, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "bash", "call_2", `{}`, 0)
	if err != nil {
		t.Fatalf("CreateApproval (no expiry): %v", err)
	}
	if b.ExpiresAt != 0 {
		t.Errorf("no-expiry ExpiresAt = %d, want 0", b.ExpiresAt)
	}
	gotB, err := s.GetApproval(ctx, "alice@example.com", b.ID)
	if err != nil {
		t.Fatalf("GetApproval b: %v", err)
	}
	if gotB.ExpiresAt != 0 {
		t.Errorf("no-expiry GetApproval ExpiresAt = %d, want 0", gotB.ExpiresAt)
	}
}

// TestListExpiredApprovals_FiltersCorrectly is the read half of the
// default-DENY-on-timeout sweep (#225): only pending approvals with a positive,
// past deadline are returned. Future deadlines, no-deadline rows, and
// already-resolved rows are excluded.
func TestListExpiredApprovals_FiltersCorrectly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	now := time.Now().Unix()

	past, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "bash", "c_past", `{}`, now-10)
	if err != nil {
		t.Fatalf("create past: %v", err)
	}
	if _, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "bash", "c_future", `{}`, now+300); err != nil {
		t.Fatalf("create future: %v", err)
	}
	if _, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "bash", "c_none", `{}`, 0); err != nil {
		t.Fatalf("create no-expiry: %v", err)
	}
	// An expired-but-already-resolved row must be excluded.
	resolved, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "bash", "c_resolved", `{}`, now-20)
	if err != nil {
		t.Fatalf("create resolved: %v", err)
	}
	if ok, err := s.ClaimApproval(ctx, "alice@example.com", resolved.ID, "approved", "done"); err != nil || !ok {
		t.Fatalf("claim resolved: ok=%v err=%v", ok, err)
	}

	expired, err := s.ListExpiredApprovals(ctx, now)
	if err != nil {
		t.Fatalf("ListExpiredApprovals: %v", err)
	}
	if len(expired) != 1 {
		t.Fatalf("expected exactly 1 expired pending approval, got %d: %+v", len(expired), expired)
	}
	if expired[0].ID != past.ID {
		t.Errorf("expired[0].ID = %s, want %s (the past-deadline pending row)", expired[0].ID, past.ID)
	}
	if expired[0].ExpiresAt != now-10 {
		t.Errorf("expired[0].ExpiresAt = %d, want %d", expired[0].ExpiresAt, now-10)
	}
}

// TestSetApprovalTimeout_RoundTrip verifies the per-conversation override
// column (#225) sets, reads, and clears via List/Get.
func TestSetApprovalTimeout_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Default: NULL → nil pointer.
	got, err := s.Get(ctx, "alice@example.com", conv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ApprovalTimeoutSeconds != nil {
		t.Errorf("fresh conversation ApprovalTimeoutSeconds = %v, want nil", *got.ApprovalTimeoutSeconds)
	}

	// Set an override.
	want := 120
	if err := s.SetApprovalTimeout(ctx, "alice@example.com", conv.ID, &want); err != nil {
		t.Fatalf("SetApprovalTimeout: %v", err)
	}
	got, err = s.Get(ctx, "alice@example.com", conv.ID)
	if err != nil {
		t.Fatalf("Get after set: %v", err)
	}
	if got.ApprovalTimeoutSeconds == nil || *got.ApprovalTimeoutSeconds != want {
		t.Errorf("ApprovalTimeoutSeconds = %v, want %d", got.ApprovalTimeoutSeconds, want)
	}
	// List must carry it too.
	list, err := s.List(ctx, "alice@example.com", false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ApprovalTimeoutSeconds == nil || *list[0].ApprovalTimeoutSeconds != want {
		t.Errorf("List did not carry the override: %+v", list)
	}

	// Clear it back to the global default.
	if err := s.SetApprovalTimeout(ctx, "alice@example.com", conv.ID, nil); err != nil {
		t.Fatalf("SetApprovalTimeout(nil): %v", err)
	}
	got, err = s.Get(ctx, "alice@example.com", conv.ID)
	if err != nil {
		t.Fatalf("Get after clear: %v", err)
	}
	if got.ApprovalTimeoutSeconds != nil {
		t.Errorf("after clear ApprovalTimeoutSeconds = %v, want nil", *got.ApprovalTimeoutSeconds)
	}
}
