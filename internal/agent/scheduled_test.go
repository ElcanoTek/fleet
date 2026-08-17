package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
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

// TestExecute_BrokerModeGatesToolsByInjectedAllowlist pins Gate-2 for broker
// mode: production scrubs config.MCPServers after broker boot, so the run's
// per-server tool allowlist must come from the driver-injected
// Options.MCPToolAllowlist — an excluded catalog tool is never advertised to
// the model.
func TestExecute_BrokerModeGatesToolsByInjectedAllowlist(t *testing.T) {
	t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
	advertised := map[string]bool{}
	model := &itMockModel{
		streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
			for _, tool := range call.Tools {
				advertised[tool.GetName()] = true
			}
			return func(yield func(fantasy.StreamPart) bool) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}, nil
		},
	}
	a := NewAgent(Options{
		Config:        &config.Config{MaxIterations: 2, LLMMaxTokens: 4096},
		Model:         model,
		SystemPrompt:  "scheduled broker gate-2 test",
		MaxIterations: 2,
		MCPBroker:     &interactiveRecordingBroker{},
		MCPCatalog: []mcp.ServerTool{
			{ServerName: "bundle", Tool: mcp.Tool{Name: "lookup", Description: "lookup"}},
			{ServerName: "bundle", Tool: mcp.Tool{Name: "purge", Description: "purge"}},
		},
		MCPToolAllowlist: agentcore.MCPAllowlist{"bundle": {"lookup"}},
	})
	if err := a.Execute(context.Background(), "look it up"); err == nil {
		t.Fatal("expected scheduled audit enforcement to remain unfinished")
	}
	if !advertised["mcp_bundle_lookup"] {
		t.Error("allowlisted tool mcp_bundle_lookup was not advertised")
	}
	if advertised["mcp_bundle_purge"] {
		t.Error("tool mcp_bundle_purge escaped the injected Gate-2 allowlist")
	}
}

func TestNewAgent_PreservesExplicitEmptyBrokerCatalog(t *testing.T) {
	a := NewAgent(Options{MCPBroker: &interactiveRecordingBroker{}, MCPCatalog: []mcp.ServerTool{}})
	if a.mcpCatalog == nil {
		t.Fatal("explicit empty broker catalog became nil and could fall back to local discovery")
	}
}

// TestExecute_CostCeilingStopIsAnError is the driver-level regression guard for
// #1105: a free-form scheduled run that trips its cost/token ceiling mid-work
// must return an error wrapping agentcore.ErrCostCeilingExceeded (the sentinel
// the runner's classifyFailure maps to the cost_ceiling class), NOT the nil
// that recorded the task as SUCCESS — success notification, email reply-back,
// no finish gates. The partial transcript must still persist to the session log
// exactly as before.
func TestExecute_CostCeilingStopIsAnError(t *testing.T) {
	model := &itMockModel{
		streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
			return func(yield func(fantasy.StreamPart) bool) {
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t", Delta: "partial work"}) {
					return
				}
				yield(fantasy.StreamPart{
					Type:         fantasy.StreamPartTypeFinish,
					FinishReason: fantasy.FinishReasonStop,
					// One step's usage blows straight through the 10-token
					// ceiling below, so the budget-guarded PrepareStep aborts
					// the run before the next paid completion.
					Usage: fantasy.Usage{InputTokens: 50, OutputTokens: 10},
				})
			}, nil
		},
	}
	t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
	a := NewAgent(Options{
		Config:        &config.Config{MaxIterations: 50, LLMMaxTokens: 4096, MaxTotalTokens: 10, MCPServers: map[string]config.MCPServerConfig{}},
		Model:         model,
		SystemPrompt:  "you are a scheduled agent",
		MaxIterations: 50,
	})

	err := a.Execute(context.Background(), "burn the budget")
	if !errors.Is(err, agentcore.ErrCostCeilingExceeded) {
		t.Fatalf("budget-stopped run returned %v, want an error wrapping ErrCostCeilingExceeded", err)
	}
	// The partial transcript survives the reclassification: the assistant text
	// streamed before the ceiling fired is in the session log.
	var sawPartial bool
	for _, m := range a.logSession.SnapshotMessages() {
		if m.Role == roleAssistant && strings.Contains(m.Content, "partial work") {
			sawPartial = true
		}
	}
	if !sawPartial {
		t.Fatal("budget-stopped run lost its partial transcript")
	}
}

// TestExecute_CancelledRunIsAnError pins the cancel half of #1105: a run whose
// agentcore Result comes back Cancelled (nil error) must surface as an
// ErrRunCancelled-wrapped error carrying the ctx cause — the runner's
// stop/pause markers attribute it; with no marker it now classifies as a
// failure instead of falling through to success. The cancel is attribution, not
// a fault, so no "[fatal]" line is written to the session log.
func TestExecute_CancelledRunIsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := &itMockModel{
		streamFunc: func(c context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
			// Cancel mid-run, the way a stop/pause handler does, then fail the
			// stream with the ctx error so the run classifies it as a cancel.
			cancel()
			return nil, c.Err()
		},
	}
	a := newTestScheduledAgent(t, model)

	err := a.Execute(ctx, "long task")
	if !errors.Is(err, agentcore.ErrRunCancelled) {
		t.Fatalf("cancelled run returned %v, want an error wrapping ErrRunCancelled", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run error %v must carry the ctx cause", err)
	}
	for _, m := range a.logSession.SnapshotMessages() {
		if strings.HasPrefix(m.Content, "[fatal]") {
			t.Fatalf("a surfaced cancel must not write a [fatal] transcript line, got %q", m.Content)
		}
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
