package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/acpruntime"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// TestACPVerifyBroker_RunsHostVerifier proves the host side of the native-acp
// verifier seam (#35): the broker maps the agent-shipped tool-exec summary onto
// the agent package's records and runs the SAME runEndOfRunVerifier the
// in-process path runs, on the HOST fallback model — so the verifier's model call
// and creds never enter the agent container. The records cross the seam; the
// missing-actions verdict comes back.
func TestACPVerifyBroker_RunsHostVerifier(t *testing.T) {
	t.Run("missing actions surface", func(t *testing.T) {
		model := &itMockModel{generateText: `{"missing_actions":["send the report"],"reasoning":"no send_email call observed"}`}
		broker := &acpVerifyBroker{agent: &Agent{fallbackModel: model}, task: "email the team the weekly report"}
		missing, err := broker.Verify(context.Background(), 1, []acpruntime.ToolExecRecord{
			{Name: "bash", Succeeded: true},
		})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if len(missing) != 1 || missing[0] != "send the report" {
			t.Fatalf("missing = %v, want [send the report]", missing)
		}
	})

	t.Run("clean verdict returns empty", func(t *testing.T) {
		model := &itMockModel{generateText: `{"missing_actions":[],"reasoning":"all required actions attempted"}`}
		broker := &acpVerifyBroker{agent: &Agent{fallbackModel: model}, task: "look up a contact"}
		missing, err := broker.Verify(context.Background(), 1, []acpruntime.ToolExecRecord{{Name: "mcp_acme_lookup", Succeeded: true}})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if len(missing) != 0 {
			t.Fatalf("missing = %v, want empty on a clean verdict", missing)
		}
	})
}

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

// TestExecute_NativeACPWithoutImageFallsBackInProcess proves the honesty gate: a
// scheduled task selecting native-acp with NO agent image configured falls back to
// the fully-governed in-process loop rather than crashing or silently
// under-governing. The fallback runs the SAME scheduled enforcement loop (so a
// model that never confirms the audit still errors out after multiple rounds —
// proving it took the in-process path, not a broken ACP spawn).
func TestExecute_NativeACPWithoutImageFallsBackInProcess(t *testing.T) {
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
	a.runtime = clientconfig.RuntimeNativeACP
	a.nativeAgentImage = "" // no image → fall back to in-process

	err := a.Execute(context.Background(), "complete the task")
	if err == nil {
		t.Fatal("expected the in-process enforcement loop to error when audit never clears (proves fallback ran)")
	}
	if got := atomic.LoadInt32(&streams); got < 2 {
		t.Errorf("fallback must run the in-process scheduled loop (>1 round); streamed %d times", got)
	}
}

// TestACPScheduledFallback_NoImage pins acpScheduledFallback: native-acp falls
// back (non-empty reason) when no image OR no host sandbox is configured, and
// clears (empty) only when both are present.
func TestACPScheduledFallback_NoImage(t *testing.T) {
	sb := &sandbox.Sandbox{} // non-nil; the fallback only nil-checks it
	if reason := acpScheduledFallback(&Agent{nativeAgentImage: "", sb: sb}); reason == "" {
		t.Fatal("native-acp with no image must report a fallback reason")
	}
	if reason := acpScheduledFallback(&Agent{nativeAgentImage: "localhost/fleet-native-agent:latest", sb: nil}); reason == "" {
		t.Fatal("native-acp with no host sandbox must report a fallback reason")
	}
	if reason := acpScheduledFallback(&Agent{nativeAgentImage: "localhost/fleet-native-agent:latest", sb: sb}); reason != "" {
		t.Fatalf("native-acp with an image and a sandbox must NOT fall back, got reason %q", reason)
	}
}

// TestACPScheduledFallback_LoadOnDemandFallsBack pins issue #41: a scheduled
// native-acp task that declares NO mcp_selection but runs on a box with loadable
// (enabled) MCP servers falls back to the in-process loop (which ships the
// mcp_load_servers loader) rather than running silently tool-less. A declared
// selection, a disabled-only catalog, or a nil config must NOT trigger this
// fallback.
func TestACPScheduledFallback_LoadOnDemandFallsBack(t *testing.T) {
	sb := &sandbox.Sandbox{}
	const img = "localhost/fleet-native-agent:latest"
	withEnabled := &config.Config{MCPServers: map[string]config.MCPServerConfig{"acme": {Enabled: true}}}
	withDisabled := &config.Config{MCPServers: map[string]config.MCPServerConfig{"acme": {Enabled: false}}}

	// (a) nil selection + a loadable server → fall back (would be tool-less otherwise).
	if reason := acpScheduledFallback(&Agent{nativeAgentImage: img, sb: sb, config: withEnabled, mcpSelection: nil}); reason == "" {
		t.Fatal("no-selection task with loadable servers must fall back to in-process")
	}
	// (b) declared selection → native-acp advertises it up-front, no loader needed.
	if reason := acpScheduledFallback(&Agent{nativeAgentImage: img, sb: sb, config: withEnabled, mcpSelection: agentcore.MCPSelection{{Server: "acme"}}}); reason != "" {
		t.Fatalf("a declared selection must NOT fall back, got %q", reason)
	}
	// (c) nil selection + only disabled servers → nothing loadable, native-acp is fine.
	if reason := acpScheduledFallback(&Agent{nativeAgentImage: img, sb: sb, config: withDisabled, mcpSelection: nil}); reason != "" {
		t.Fatalf("a disabled-only catalog has nothing to load; must NOT fall back, got %q", reason)
	}
	// (d) nil config (regression guard for TestACPScheduledFallback_NoImage's shape).
	if reason := acpScheduledFallback(&Agent{nativeAgentImage: img, sb: sb, config: nil, mcpSelection: nil}); reason != "" {
		t.Fatalf("nil config must be safe (nothing loadable); must NOT fall back, got %q", reason)
	}
}

// TestACPMCPClient_NoSelectionAdvertisesNoSurface proves the cross-task
// scope-creep guard: a scheduled task with NO declared mcp_selection (which reuses
// the SHARED process-wide client) advertises NO MCP surface — acpMCPClient returns
// nil so buildACPHostGovernance produces no descriptors and no broker. A task that
// declared a selection (bound onto a dedicated per-task client) advertises that
// client's servers.
func TestACPMCPClient_NoSelectionAdvertisesNoSurface(t *testing.T) {
	shared := mcp.NewClient()
	t.Cleanup(func() { _ = shared.Close() })

	// No declared selection → nil client → no advertised MCP surface, even though a
	// shared client is present (it may hold other tasks' servers).
	a := &Agent{mcpClient: shared, mcpSelection: nil}
	if got := a.acpMCPClient(); got != nil {
		t.Fatalf("no-selection task must advertise no MCP surface (nil client), got %v", got)
	}
	gov := buildACPHostGovernance(a.acpMCPClient(), nil, nil, a.mcpSelection, acpStagers{})
	if gov.MCPDescriptors != nil || gov.MCPBroker != nil {
		t.Fatalf("no-selection task must yield no descriptors/broker, got descs=%v broker=%v",
			gov.MCPDescriptors, gov.MCPBroker)
	}

	// Declared selection → the (dedicated) per-task client is advertised.
	a2 := &Agent{mcpClient: shared, mcpSelection: agentcore.MCPSelection{{Server: "acme"}}}
	if got := a2.acpMCPClient(); got != shared {
		t.Fatalf("declared-selection task must advertise its bound client, got %v", got)
	}
}

// TestRecordACPUsage_FoldsReportedUsage proves the ACP scheduled path folds the
// agent's reported cumulative run usage into the captain's-log session counters.
// The gross counters (prompt/completion/cached/cacheCreation/cost) match the
// in-process accounting exactly; LastStepPromptTokens records the last step's
// INPUT only (the per-step cache-read split is not recoverable from the cumulative
// RunUsage — a documented benign approximation, see recordACPUsage).
func TestRecordACPUsage_FoldsReportedUsage(t *testing.T) {
	a := &Agent{logSession: NewLogSession()}
	a.recordACPUsage(agentcore.RunUsage{
		PromptTokens:        300,
		LastStepInputTokens: 100,
		CompletionTokens:    60,
		CachedTokens:        30,
		CacheCreationTokens: 5,
		CostUSD:             0.012,
	})
	ls := a.logSession
	if ls.PromptTokens != 300 || ls.CompletionTokens != 60 || ls.CachedTokens != 30 ||
		ls.CacheCreationTokens != 5 || ls.LastStepPromptTokens != 100 || ls.Cost != 0.012 {
		t.Fatalf("recordACPUsage mismatch: prompt=%d completion=%d cached=%d cacheCreate=%d lastStep=%d cost=%v",
			ls.PromptTokens, ls.CompletionTokens, ls.CachedTokens, ls.CacheCreationTokens, ls.LastStepPromptTokens, ls.Cost)
	}
}
