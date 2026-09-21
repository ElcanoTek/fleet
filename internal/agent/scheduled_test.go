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

func TestScheduledObserverDoesNotPersistToolPreviews(t *testing.T) {
	session := NewLogSession()
	observer := &scheduledObserver{session: session}

	observer.Observe("tool.call", map[string]any{
		"id": "call-1", "name": "tool_call", "input": `{"name":"mcp_fast_io_download","arguments":{}}`,
	})
	observer.Observe("tool.result", map[string]any{
		"id": "call-1", "name": "tool_call", "text": "invalid arguments", "is_err": true,
	})

	msgs := session.SnapshotMessages()
	if len(msgs) != 0 {
		t.Fatalf("UI previews duplicated the core transcript: %+v", msgs)
	}
}

// A scheduled run has no user reading the transcript, so an agent that ended
// its own run with confirm_audit(success=false) must surface as an error the
// runner can classify — otherwise the nil agentcore returns (the enforcement
// gate deliberately LETS an aborting agent finish) is recorded as task success.
// That is exactly what happened to task 3d767956 (#1151).
func TestScheduledTerminalErrorSurfacesAuditAbort(t *testing.T) {
	const summary = "page unchanged; live version remains 259"
	err := scheduledTerminalError(context.Background(), agentcore.Result{
		AuditAborted: true,
		AuditSummary: summary,
	})
	if !errors.Is(err, agentcore.ErrAuditAborted) {
		t.Fatalf("aborted run returned %v, want an error wrapping ErrAuditAborted", err)
	}
	if !strings.Contains(err.Error(), summary) {
		t.Errorf("error %q should carry the agent's own summary", err)
	}

	// No summary is still an abort — the verdict outranks its wording.
	if err := scheduledTerminalError(context.Background(), agentcore.Result{AuditAborted: true}); !errors.Is(err, agentcore.ErrAuditAborted) {
		t.Errorf("summary-less abort returned %v, want ErrAuditAborted", err)
	}

	// A clean run is still clean.
	if err := scheduledTerminalError(context.Background(), agentcore.Result{FinalText: "done"}); err != nil {
		t.Errorf("clean run returned %v, want nil", err)
	}
}

// A run cancelled mid-flight never reached its own audit, so attributing the
// interruption is more truthful than attributing an abort it never made.
func TestCancelOutranksAuditAbort(t *testing.T) {
	err := scheduledTerminalError(context.Background(), agentcore.Result{Cancelled: true, AuditAborted: true})
	if !errors.Is(err, agentcore.ErrRunCancelled) {
		t.Errorf("cancelled run returned %v, want ErrRunCancelled", err)
	}
	if errors.Is(err, agentcore.ErrAuditAborted) {
		t.Error("a cancelled run must not be reported as a deliberate abort")
	}
}

// TestExecute_RoundCapPersistsPartialTranscript closes #1271: a scheduled run
// that exhausts the enforcement round cap is still a hard failure, but the
// rounds it burned were paid for — so the partial work agentcore carries back
// on the Result (#1125) must land in the persisted session log, marked as
// round-cap-truncated, instead of being discarded by the driver's
// `if err != nil { return err }`.
//
// The fixture is the never-clearing audit of
// TestExecute_ScheduledDoesNotCollapseToOneRound plus streamed text and usage:
// the model produces real assistant text and spend every round and never calls
// confirm_audit, so CanFinish blocks until the cap.
func TestExecute_RoundCapPersistsPartialTranscript(t *testing.T) {
	model := &itMockModel{
		streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
			return func(yield func(fantasy.StreamPart) bool) {
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t", Delta: "half-finished analysis"}) {
					return
				}
				yield(fantasy.StreamPart{
					Type:         fantasy.StreamPartTypeFinish,
					FinishReason: fantasy.FinishReasonStop,
					Usage:        fantasy.Usage{InputTokens: 50, OutputTokens: 10},
				})
			}, nil
		},
	}
	a := newTestScheduledAgent(t, model)

	err := a.Execute(context.Background(), "a task the agent never finishes")
	// Failure classification is unchanged: the run still FAILS, and it fails
	// with the same message it always did (the sentinel only makes that failure
	// recognizable without string-matching). internal/runner's
	// TestClassifyFailure pins the other half — this error still maps to
	// FailureTerminal, so retries and notifications behave exactly as before.
	if !errors.Is(err, agentcore.ErrMaxEnforcementRounds) {
		t.Fatalf("round-capped run returned %v, want an error wrapping ErrMaxEnforcementRounds", err)
	}
	if !strings.Contains(err.Error(), "max enforcement rounds") {
		t.Errorf("error %q must keep the operator-facing round-cap wording", err)
	}

	var sawNotice, sawPartial bool
	for _, m := range a.logSession.SnapshotMessages() {
		if m.MessageType == nil || *m.MessageType != messageTypeRoundCapTruncated {
			continue
		}
		switch m.Role {
		case roleUser:
			if strings.Contains(m.Content, "[truncated]") && strings.Contains(m.Content, "enforcement round cap") {
				sawNotice = true
			}
		case roleAssistant:
			if strings.Contains(m.Content, "half-finished analysis") {
				sawPartial = true
			}
		}
	}
	if !sawNotice {
		t.Error("the persisted log carries no round-cap truncation notice")
	}
	if !sawPartial {
		t.Error("round-capped run lost the partial assistant transcript it paid for")
	}
	// The run's spend is in the persisted log too (agentcore's orchestration
	// accounting writes it into this same LogSession as each round streams), so
	// the truncated transcript is not free-floating text with no cost attached.
	if a.logSession.PromptTokens <= 0 || a.logSession.CompletionTokens <= 0 {
		t.Errorf("round-capped run persisted no usage: prompt=%d completion=%d",
			a.logSession.PromptTokens, a.logSession.CompletionTokens)
	}
	// The failure itself is still recorded as a failure, unchanged.
	var sawFatal bool
	for _, m := range a.logSession.SnapshotMessages() {
		if strings.HasPrefix(m.Content, "[fatal]") && strings.Contains(m.Content, "max enforcement rounds") {
			sawFatal = true
		}
	}
	if !sawFatal {
		t.Error("the round-cap failure must still write its [fatal] transcript line")
	}
}

// A cost-ceiling stop used to be reported as "run stopped … without finishing
// the task" whether the run had produced nothing or had executed every declared
// critical action and written its summary (Reklaim health scan 6bd0c212: SES
// accepted the email, artifact published, summary written, then the ceiling —
// and the DLQ said the task never finished). The reason now carries the facts
// an operator needs to tell those two runs apart (#1532).
func TestScheduledTerminalErrorBudgetStopCarriesFacts(t *testing.T) {
	landed := scheduledTerminalError(context.Background(), agentcore.Result{
		StoppedByBudget:         true,
		Cancelled:               true,
		FinalText:               "Report emailed; 142 campaigns summarised.",
		CriticalActionsExecuted: 1,
		Usage:                   agentcore.RunUsage{CostUSD: 8.2862},
	})
	if !errors.Is(landed, agentcore.ErrCostCeilingExceeded) {
		t.Fatalf("budget stop returned %v, want ErrCostCeilingExceeded", landed)
	}
	msg := landed.Error()
	for _, want := range []string{"$8.2862", "1 critical action(s) completed", "no declared critical action is outstanding", "deliverable most likely landed", "had written its final summary", "end-of-run checks did not run", "not been rolled back"} {
		if !strings.Contains(msg, want) {
			t.Errorf("reason %q should contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "without finishing the task") {
		t.Errorf("reason %q must not claim the task never finished when every declared action executed", msg)
	}

	owed := scheduledTerminalError(context.Background(), agentcore.Result{
		StoppedByBudget:            true,
		Cancelled:                  true,
		CriticalActionsExecuted:    1,
		CriticalActionsOutstanding: []string{"mcp_ses_outbound_send_email (record n/a)"},
		Usage:                      agentcore.RunUsage{CostUSD: 3},
	})
	if msg := owed.Error(); !strings.Contains(msg, "still outstanding: mcp_ses_outbound_send_email (record n/a)") || strings.Contains(msg, "most likely landed") {
		t.Errorf("reason %q should name the outstanding commitment and not claim the deliverable landed", msg)
	}

	nothing := scheduledTerminalError(context.Background(), agentcore.Result{StoppedByBudget: true, Cancelled: true})
	if msg := nothing.Error(); !strings.Contains(msg, "0 critical action(s) completed") || strings.Contains(msg, "most likely landed") || strings.Contains(msg, "final summary") {
		t.Errorf("reason %q for a run that produced nothing must say so plainly", msg)
	}
}

// The terminal report must name WHICH ceiling fired. A token-ceiling stop used
// to be reconstructed as a cost-only sentence ("run stopped after $X spent"),
// so an operator saw $0.03 against a $50 deployment ceiling with no token
// numbers and no way to tell why the run stopped. The guard's own sentence is
// preferred now; the audit facts still append.
func TestScheduledTerminalErrorBudgetStopNamesTheCeiling(t *testing.T) {
	token := scheduledTerminalError(context.Background(), agentcore.Result{
		StoppedByBudget:  true,
		Cancelled:        true,
		BudgetStopReason: "TOKEN_CEILING_REACHED: this turn has processed 2000 uncached tokens which meets or exceeds the configured ceiling of 2000. Stop calling tools and end the turn with what you have.",
		Usage:            agentcore.RunUsage{PromptTokens: 1800, CompletionTokens: 200},
	})
	if !errors.Is(token, agentcore.ErrCostCeilingExceeded) {
		t.Fatalf("token stop returned %v, want ErrCostCeilingExceeded", token)
	}
	msg := token.Error()
	for _, want := range []string{"TOKEN_CEILING_REACHED", "2000 uncached tokens", "ceiling of 2000", "0 critical action(s) completed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("token-stop message %q should contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "$") {
		t.Errorf("token-stop message must not fabricate a cost claim: %q", msg)
	}

	cost := scheduledTerminalError(context.Background(), agentcore.Result{
		StoppedByBudget:  true,
		Cancelled:        true,
		BudgetStopReason: "COST_CEILING_REACHED: this turn has accumulated $0.0260 which meets or exceeds the configured ceiling of $0.03. Stop calling tools and end the turn with what you have.",
		Usage:            agentcore.RunUsage{CostUSD: 0.026},
	})
	cmsg := cost.Error()
	for _, want := range []string{"COST_CEILING_REACHED", "$0.0260", "$0.03", "0 critical action(s) completed"} {
		if !strings.Contains(cmsg, want) {
			t.Errorf("cost-stop message %q should contain %q", cmsg, want)
		}
	}
	if strings.Count(cmsg, "cost/token ceiling exceeded") != 1 {
		t.Errorf("sentinel text must appear exactly once (the failure class), got: %q", cmsg)
	}
}
