package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/store"
)

// An approval card outlives the turn scope that staged it. These tests cover
// the #167 residual-2 contract: the seat the turn was running on is recorded at
// staging and reopened at execution, and a seat that no longer resolves fails
// the approval instead of quietly sending as the default client.

// scopedApprovalEngine records the selection an approval execution asked for.
type scopedApprovalEngine struct {
	*fakeEngine
	shared    agentcore.MCPBroker
	catalog   []mcp.ServerTool
	scope     *agent.MCPScope
	openErr   error
	requested agentcore.MCPSelection
	closed    bool
}

func (e *scopedApprovalEngine) MCPBroker() agentcore.MCPBroker { return e.shared }
func (e *scopedApprovalEngine) MCPCatalog() []mcp.ServerTool   { return e.catalog }
func (e *scopedApprovalEngine) OpenApprovalRemoteMCPScope(context.Context, string, string, string) (*agent.RemoteMCPOverlay, error) {
	return nil, nil
}

func (e *scopedApprovalEngine) OpenApprovalMCPScope(_ context.Context, selection agentcore.MCPSelection, _ string) (*agent.MCPScope, error) {
	e.requested = selection
	if e.openErr != nil {
		return nil, e.openErr
	}
	if e.scope == nil {
		return nil, nil
	}
	scope := *e.scope
	scope.Close = func(context.Context) error {
		e.closed = true
		return nil
	}
	return &scope, nil
}

func TestRunStagedTool_ReopensTheStagedSeat(t *testing.T) {
	scoped := &fakeMCPBroker{text: "sent from client-a"}
	engine := &scopedApprovalEngine{
		fakeEngine: &fakeEngine{},
		// The shared default-seat broker must NOT be the one that runs the call.
		shared:  &fakeMCPBroker{text: "sent from the default seat"},
		catalog: sendgridApprovalCatalog,
		scope: &agent.MCPScope{
			Broker:  scoped,
			Catalog: []mcp.ServerTool{{ServerName: "send_grid_client_a", Tool: mcp.Tool{Name: "send_email"}}},
		},
	}
	s := &Server{agent: engine}

	text, err := s.runStagedTool(context.Background(), &store.Approval{
		ToolName:   "mcp_send_grid_client_a_send_email",
		ArgsJSON:   `{"to":"test@example.com"}`,
		MCPServer:  "send_grid",
		MCPAccount: "client_a",
	})
	if err != nil {
		t.Fatalf("runStagedTool: %v", err)
	}
	if text != "sent from client-a" {
		t.Fatalf("result = %q, want the scoped broker's result", text)
	}
	if len(engine.requested) != 1 || engine.requested[0].Server != "send_grid" || engine.requested[0].Account != "client_a" {
		t.Fatalf("reopened selection = %+v, want the staged {send_grid, client_a} seat", engine.requested)
	}
	if scoped.server != "send_grid_client_a" || scoped.tool != "send_email" {
		t.Fatalf("routed to %q.%q, want the account-variant registration", scoped.server, scoped.tool)
	}
	if !engine.closed {
		t.Fatal("per-approval scope was not closed after execution")
	}
}

func TestRunStagedTool_FailsClosedWhenTheStagedSeatIsGone(t *testing.T) {
	engine := &scopedApprovalEngine{
		fakeEngine: &fakeEngine{},
		shared:     &fakeMCPBroker{text: "sent from the default seat"},
		catalog:    sendgridApprovalCatalog,
		openErr:    errors.New("no <VAR>_CLIENT_A credentials are set"),
	}
	s := &Server{agent: engine}

	_, err := s.runStagedTool(context.Background(), &store.Approval{
		ToolName:   "mcp_send_grid_client_a_send_email",
		ArgsJSON:   `{}`,
		MCPServer:  "send_grid",
		MCPAccount: "client_a",
	})
	if err == nil {
		t.Fatal("a revoked seat executed instead of failing closed")
	}
	if !strings.Contains(err.Error(), "send_grid (account client_a)") {
		t.Fatalf("error = %v, want the staged seat named", err)
	}
}

// A row staged before the seat columns existed (or by a native tool) has no
// seat to reopen, and must keep working on the shared broker.
func TestRunStagedTool_LegacyRowWithoutASeatUsesTheSharedBroker(t *testing.T) {
	shared := &fakeMCPBroker{text: "sent"}
	engine := &scopedApprovalEngine{
		fakeEngine: &fakeEngine{},
		shared:     shared,
		catalog:    sendgridApprovalCatalog,
	}
	s := &Server{agent: engine}

	text, err := s.runStagedTool(context.Background(), &store.Approval{
		ToolName: "mcp_send_grid_send_email",
		ArgsJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("runStagedTool: %v", err)
	}
	if text != "sent" || shared.server != "send_grid" {
		t.Fatalf("result/route = %q %q, want the shared broker", text, shared.server)
	}
	if engine.requested != nil {
		t.Fatalf("a seatless row opened a scope for %+v", engine.requested)
	}
}

// Staging resolves against the TURN's catalog and records the turn's seat.
// Before this, the stager held the process-wide default-seat catalog, in which
// a named-account tool identity does not appear at all.
func TestApprovalStager_BindTurnMCPScopeRecordsTheTurnSeat(t *testing.T) {
	stager := &approvalStager{
		mcpCatalog: []mcp.ServerTool{{ServerName: "send_grid", Tool: mcp.Tool{Name: "send_email"}}},
	}
	if seat := stager.seatFor("mcp_send_grid_client_a_send_email"); seat.Server != "" {
		t.Fatalf("unbound stager resolved %+v against the default-seat catalog", seat)
	}

	stager.BindTurnMCPScope(agent.TurnMCPScope{
		Broker:    &fakeMCPBroker{},
		Catalog:   []mcp.ServerTool{{ServerName: "send_grid_client_a", Tool: mcp.Tool{Name: "send_email"}}},
		Selection: agentcore.MCPSelection{{Server: "send_grid", Account: "client_a"}},
	})

	seat := stager.seatFor("mcp_send_grid_client_a_send_email")
	if seat.Server != "send_grid" || seat.Account != "client_a" {
		t.Fatalf("seat = %+v, want the turn's {send_grid, client_a} selection", seat)
	}
	// A native tool has no MCP identity and records no seat.
	if seat := stager.seatFor("bash"); seat.Server != "" || seat.Account != "" {
		t.Fatalf("native tool recorded seat %+v, want none", seat)
	}
}

// A server the turn selected on its default seat records the server with an
// empty account, so execution reopens that server rather than falling through
// to the shared client.
func TestApprovalStager_DefaultSeatIsStillRecorded(t *testing.T) {
	stager := &approvalStager{}
	stager.BindTurnMCPScope(agent.TurnMCPScope{
		Catalog:   []mcp.ServerTool{{ServerName: "send_grid", Tool: mcp.Tool{Name: "send_email"}}},
		Selection: agentcore.MCPSelection{{Server: "send_grid"}},
	})
	seat := stager.seatFor("mcp_send_grid_send_email")
	if seat.Server != "send_grid" || seat.Account != "" {
		t.Fatalf("seat = %+v, want the default seat of send_grid", seat)
	}
}
