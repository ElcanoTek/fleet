package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// newTestScheduledAgent builds a scheduled Agent over a mock model with no MCP
// servers and no captain's log (so Execute touches no network / git).
func newTestScheduledAgent(t *testing.T, model fantasy.LanguageModel) *Agent {
	t.Helper()
	t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
	return NewAgent(Options{
		Config:        &config.Config{MaxIterations: 50, LLMMaxTokens: 4096, MCPServers: map[string]config.MCPServerConfig{}},
		Model:         model,
		SystemPrompt:  "you are a scheduled agent",
		MaxIterations: 50,
	})
}

// TestExecute_NilModelReturnsError pins the no-model guard.
func TestExecute_NilModelReturnsError(t *testing.T) {
	a := newTestScheduledAgent(t, nil)
	a.model = nil
	if err := a.Execute(context.Background(), "do the thing"); err == nil {
		t.Fatal("expected error with no model configured")
	}
}

// TestExecute_ScheduledDoesNotCollapseToOneRound verifies the scheduled driver
// engages the FULL enforcement loop (Mode=Scheduled) rather than the interactive
// 1-round collapse: a model that just stops without ever calling confirm_audit
// never satisfies finish enforcement, so the loop keeps injecting nudges and
// streams more than once before bounding out at the max-rounds cap. This is the
// observable difference from the interactive InteractivePolicy (which finishes
// at round 0). The terminal error is expected — the point is that the scheduled
// Policy.CanFinish blocked finishing.
func TestExecute_ScheduledDoesNotCollapseToOneRound(t *testing.T) {
	streams := int32(0)
	model := &itMockModel{
		streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
			atomic.AddInt32(&streams, 1)
			return func(yield func(fantasy.StreamPart) bool) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}, nil
		},
	}
	a := newTestScheduledAgent(t, model)

	err := a.Execute(context.Background(), "complete the task")
	// The audit never clears, so the loop exhausts the round cap and errors.
	if err == nil {
		t.Fatal("expected max-rounds error when audit never clears")
	}
	if got := atomic.LoadInt32(&streams); got < 2 {
		t.Errorf("scheduled run must NOT collapse to 1 round; streamed %d times", got)
	}
}

func TestExecute_UsesInjectedMCPBrokerAndCatalog(t *testing.T) {
	broker := &interactiveRecordingBroker{}
	calls := int32(0)
	loaderAdvertised := false
	model := &itMockModel{
		streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
			for _, tool := range call.Tools {
				if tool.GetName() == "mcp_load_servers" {
					loaderAdvertised = true
				}
			}
			round := atomic.AddInt32(&calls, 1)
			return func(yield func(fantasy.StreamPart) bool) {
				if round == 1 {
					yield(fantasy.StreamPart{
						Type:          fantasy.StreamPartTypeToolCall,
						ID:            "mcp-1",
						ToolCallName:  "mcp_bundle_lookup",
						ToolCallInput: `{}`,
					})
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
					return
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}, nil
		},
	}
	a := NewAgent(Options{
		Config:        &config.Config{MaxIterations: 2, LLMMaxTokens: 4096, MCPServers: map[string]config.MCPServerConfig{}},
		Model:         model,
		SystemPrompt:  "scheduled broker test",
		MaxIterations: 2,
		MCPBroker:     broker,
		MCPCatalog: []mcp.ServerTool{{
			ServerName: "bundle",
			Tool:       mcp.Tool{Name: "lookup", Description: "lookup"},
		}},
	})
	err := a.Execute(context.Background(), "look it up")
	if err == nil {
		t.Fatal("expected scheduled audit enforcement to remain unfinished")
	}
	if broker.calls != 1 || broker.server != "bundle" || broker.tool != "lookup" {
		t.Fatalf("broker calls = %d (%q.%q), want one bundle.lookup", broker.calls, broker.server, broker.tool)
	}
	if loaderAdvertised {
		t.Fatal("broker mode advertised the in-process MCP loader")
	}
}

func TestNewAgent_PreservesExplicitEmptyBrokerCatalog(t *testing.T) {
	a := NewAgent(Options{MCPBroker: &interactiveRecordingBroker{}, MCPCatalog: []mcp.ServerTool{}})
	if a.mcpCatalog == nil {
		t.Fatal("explicit empty broker catalog became nil and could fall back to local discovery")
	}
}

func TestScheduledObserverPersistsToolCallAndErrorResult(t *testing.T) {
	session := NewLogSession()
	observer := &scheduledObserver{session: session}

	observer.Observe("tool.call", map[string]any{
		"id": "call-1", "name": "tool_call", "input": `{"name":"mcp_fast_io_download","arguments":{}}`,
	})
	observer.Observe("tool.result", map[string]any{
		"id": "call-1", "name": "tool_call", "text": "invalid arguments", "is_err": true,
	})

	msgs := session.SnapshotMessages()
	if len(msgs) != 2 {
		t.Fatalf("persisted messages = %d, want call + result", len(msgs))
	}
	if len(msgs[0].ToolCalls) != 1 || msgs[0].ToolCalls[0].Name != "tool_call" {
		t.Fatalf("tool call not preserved: %+v", msgs[0])
	}
	if msgs[1].Role != roleTool || msgs[1].ToolCallID == nil || *msgs[1].ToolCallID != "call-1" ||
		msgs[1].ToolName != "tool_call" || !msgs[1].IsError {
		t.Fatalf("tool error result not preserved: %+v", msgs[1])
	}
}
