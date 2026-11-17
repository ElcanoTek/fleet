package agent

import (
	"context"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/fakellm"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// The real chat turn (Manager.RunTurn) applies the installed
// interactive_hidden_tools: the request the model receives does not offer
// the hidden tool, and does offer its sibling.
func TestInteractiveHidden_ManagerRunTurnHidesTheTool(t *testing.T) {
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{InteractiveHiddenTools: []string{"ox_create_deal"}})
	fake := fakellm.New()
	fake.SetDefault(fakellm.Scenario{Steps: []fakellm.Step{fakellm.TextStep("noted")}})
	capture := &systemPromptCapture{next: fake.Handler()}
	catalog := []mcp.ServerTool{
		{ServerName: "ox", Tool: mcp.Tool{Name: "ox_create_deal", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}},
		{ServerName: "ox", Tool: mcp.Tool{Name: "ox_get_deal", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}},
	}
	mgr := newFakeLLMManagerWithHandler(t, capture, func(opts *ManagerOptions) {
		opts.OpenRemoteMCPOverlay = func(context.Context, string, map[string]bool, RemoteMCPSelection) (*RemoteMCPOverlay, error) {
			return &RemoteMCPOverlay{
				Broker:     inertMCPBroker{},
				Servers:    map[string]bool{"ox": true},
				Catalog:    catalog,
				CloseScope: func(context.Context) error { return nil },
			}, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := mgr.RunTurn(ctx, TurnInput{UserMessage: "hi", Model: "anthropic/claude-opus-4.8", UserEmail: "user@example.test"}, &recordingSink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	tools := capture.toolNames()
	if tools["mcp_ox_ox_create_deal"] || !tools["mcp_ox_ox_get_deal"] {
		t.Fatalf("tools offered to the model: %v", tools)
	}
}
