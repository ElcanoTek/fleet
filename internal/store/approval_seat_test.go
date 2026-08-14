package store

import (
	"context"
	"testing"
)

// The staged credential seat must survive the card's whole lifetime: execution
// happens on a later HTTP request, long after the turn scope closed, and it
// reopens the seat from this row (#167 residual 2).
func TestCreateApproval_PersistsAndReadsBackTheStagedSeat(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	named, err := s.CreateApproval(ctx, conv.ID, "alice@example.com",
		"mcp_sendgrid_client_a_send_email", "call_1", `{}`, 0,
		ApprovalSeat{Server: "sendgrid", Account: "client_a"})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	got, err := s.GetApproval(ctx, "alice@example.com", named.ID)
	if err != nil || got == nil {
		t.Fatalf("GetApproval: %v (row=%v)", err, got)
	}
	if got.MCPServer != "sendgrid" || got.MCPAccount != "client_a" {
		t.Fatalf("seat = {%q, %q}, want {sendgrid, client_a}", got.MCPServer, got.MCPAccount)
	}

	// A recorded server with no account is the DEFAULT seat, which is a real
	// answer and must round-trip as an empty account rather than as "no seat".
	def, err := s.CreateApproval(ctx, conv.ID, "alice@example.com",
		"mcp_sendgrid_send_email", "call_2", `{}`, 0, ApprovalSeat{Server: "sendgrid"})
	if err != nil {
		t.Fatalf("CreateApproval (default seat): %v", err)
	}
	got, err = s.GetApproval(ctx, "alice@example.com", def.ID)
	if err != nil || got == nil {
		t.Fatalf("GetApproval (default seat): %v (row=%v)", err, got)
	}
	if got.MCPServer != "sendgrid" || got.MCPAccount != "" {
		t.Fatalf("default seat = {%q, %q}, want {sendgrid, \"\"}", got.MCPServer, got.MCPAccount)
	}

	// A native tool records nothing; the columns stay NULL and read back empty,
	// which execution treats exactly as a pre-#167 legacy row.
	native, err := s.CreateApproval(ctx, conv.ID, "alice@example.com", "bash", "call_3", `{}`, 0, ApprovalSeat{})
	if err != nil {
		t.Fatalf("CreateApproval (native): %v", err)
	}
	got, err = s.GetApproval(ctx, "alice@example.com", native.ID)
	if err != nil || got == nil {
		t.Fatalf("GetApproval (native): %v (row=%v)", err, got)
	}
	if got.MCPServer != "" || got.MCPAccount != "" {
		t.Fatalf("native seat = {%q, %q}, want empty", got.MCPServer, got.MCPAccount)
	}

	pending, err := s.ListPendingApprovals(ctx, "alice@example.com", conv.ID)
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	for _, a := range pending {
		if a.ID == named.ID && (a.MCPServer != "sendgrid" || a.MCPAccount != "client_a") {
			t.Fatalf("list dropped the seat: {%q, %q}", a.MCPServer, a.MCPAccount)
		}
	}
}
