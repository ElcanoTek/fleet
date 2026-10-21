package agent

import (
	"context"
	"slices"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
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

// A sub-agent spawned from a chat turn inherits the turn's interactive-hidden
// tools (agent_policy.interactive_hidden_tools), at every depth, so
// delegating never reaches a tool chat could not. A scheduled task's agent
// carries none.
func TestInteractiveHidden_SpawnHostAndChildrenInheritTheList(t *testing.T) {
	tc := TurnConfig{
		SystemPrompt:       "sys",
		Config:             &config.Config{MaxIterations: 10},
		Subagent:           SubagentOptions{Enabled: true, MaxDepth: 2, MaxChildren: 5},
		HiddenToolSuffixes: []string{"ox_create_deal"},
	}
	host := newInteractiveSpawnHost(tc, agentcore.NewInteractivePolicy(1.0, 0, nil, nil), nil)
	child := host.buildChild(SubagentRoleWorker, nil, nil, nil, 0, 0, 0)
	grandchild := child.buildChild(SubagentRoleExplore, nil, nil, nil, 0, 0, 0)
	for name, a := range map[string]*Agent{"host": host, "child": child, "grandchild": grandchild} {
		if !slices.Equal(a.hiddenToolSuffixes, []string{"ox_create_deal"}) {
			t.Fatalf("%s: hidden = %v", name, a.hiddenToolSuffixes)
		}
	}
	scheduled := NewAgent(Options{Config: &config.Config{}, SystemPrompt: "task"})
	if c := scheduled.buildChild(SubagentRoleWorker, nil, nil, nil, 0, 0, 0); scheduled.hiddenToolSuffixes != nil || c.hiddenToolSuffixes != nil {
		t.Fatalf("a scheduled task hides %v (child %v)", scheduled.hiddenToolSuffixes, c.hiddenToolSuffixes)
	}
}

// End to end through Execute (the loop a sub-agent runs): the hidden tool is
// not in the tool list the model is sent; its sibling is.
func TestInteractiveHidden_ChildRunIsNotOfferedTheTool(t *testing.T) {
	var sent []string
	model := &itMockModel{streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		if sent == nil {
			for _, tool := range call.Tools {
				sent = append(sent, tool.GetName())
			}
		}
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}}
	a := newTestScheduledAgent(t, model)
	a.mcpCatalog = []mcp.ServerTool{
		{ServerName: "ox", Tool: mcp.Tool{Name: "ox_create_deal", InputSchema: map[string]any{"type": "object"}}},
		{ServerName: "ox", Tool: mcp.Tool{Name: "ox_get_deal", InputSchema: map[string]any{"type": "object"}}},
	}
	a.hiddenToolSuffixes = []string{"ox_create_deal"}
	_ = a.Execute(context.Background(), "Look at the deal.")
	if slices.Contains(sent, "mcp_ox_ox_create_deal") || !slices.Contains(sent, "mcp_ox_ox_get_deal") {
		t.Fatalf("tools sent to the model = %v", sent)
	}
}
