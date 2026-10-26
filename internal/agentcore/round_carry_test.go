package agentcore

// Enforcement-round transcript carry: when the policy blocks a finish, the
// next round's input must contain the blocked round's assistant/tool work —
// not just the original prompt plus the nudge. Without the carry the model
// starts the task over from scratch on every nudge (observed: a 31-minute
// scheduled run re-downloading and re-computing its whole analysis after the
// audit-confirmation nudge, task 401172db).

import (
	"context"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"

	"github.com/ElcanoTek/fleet/internal/tools"
)

func TestCarryRoundMessages(t *testing.T) {
	res := &fantasy.AgentResult{
		Steps: []fantasy.StepResult{
			{Messages: []fantasy.Message{
				{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
					fantasy.ReasoningPart{Text: "private chain of thought"},
					fantasy.TextPart{Text: "running the analysis"},
					fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "bash", Input: `{"command":"ls"}`},
				}},
				{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
					fantasy.ToolResultPart{ToolCallID: "c1"},
				}},
			}},
			{Messages: []fantasy.Message{
				// Reasoning-only assistant message: dropped entirely.
				{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
					fantasy.ReasoningPart{Text: "more thinking"},
				}},
				{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
					fantasy.TextPart{Text: "final summary"},
				}},
			}},
		},
	}

	out := carryRoundMessages(res)
	if len(out) != 3 {
		t.Fatalf("carried %d messages, want 3 (assistant+tool from step 1, text from step 2): %+v", len(out), out)
	}
	if out[0].Role != fantasy.MessageRoleAssistant || len(out[0].Content) != 2 {
		t.Errorf("first carried message = %+v, want assistant with reasoning stripped (2 parts)", out[0])
	}
	for _, p := range out[0].Content {
		if _, isReasoning := p.(fantasy.ReasoningPart); isReasoning {
			t.Error("reasoning part leaked into carried assistant message")
		}
	}
	if out[1].Role != fantasy.MessageRoleTool {
		t.Errorf("second carried message role = %q, want tool (call/result pairing preserved)", out[1].Role)
	}
	if out[2].Role != fantasy.MessageRoleAssistant {
		t.Errorf("third carried message = %+v, want the step-2 text message", out[2])
	}

	if got := carryRoundMessages(nil); got != nil {
		t.Errorf("nil result should carry nothing, got %+v", got)
	}
}

// textCapturingModel emits a distinct text response per call and records the
// exact message slice each Stream call received.
type textCapturingModel struct {
	mockModel
	slug        string
	replies     []string
	omitTextEnd bool
	secondBlock string

	recMu sync.Mutex
	seen  [][]fantasy.Message
}

func (m *textCapturingModel) Model() string { return m.slug }

func (m *textCapturingModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.recMu.Lock()
	idx := len(m.seen)
	m.seen = append(m.seen, append([]fantasy.Message(nil), call.Prompt...))
	reply := "done"
	if idx < len(m.replies) {
		reply = m.replies[idx]
	}
	m.recMu.Unlock()
	return func(yield func(fantasy.StreamPart) bool) {
		// Full text lifecycle: fantasy commits streamed text into the step's
		// content (and thus StepResult.Messages) only at TextEnd, exactly like
		// a real provider stream.
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: reply}) {
			return
		}
		if !m.omitTextEnd && !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"}) {
			return
		}
		if m.secondBlock != "" {
			for _, part := range []fantasy.StreamPart{
				{Type: fantasy.StreamPartTypeTextStart, ID: "t2"},
				{Type: fantasy.StreamPartTypeTextDelta, ID: "t2", Delta: m.secondBlock},
				{Type: fantasy.StreamPartTypeTextEnd, ID: "t2"},
			} {
				if !yield(part) {
					return
				}
			}
		}
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 10, OutputTokens: 5},
		})
	}, nil
}

func (m *textCapturingModel) call(i int) []fantasy.Message {
	m.recMu.Lock()
	defer m.recMu.Unlock()
	return m.seen[i]
}

func TestRun_EnforcementRoundCarriesTranscript(t *testing.T) {
	session := NewLogSession()
	model := &textCapturingModel{slug: "carry-test-model", replies: []string{"round one analysis", "confirmed"}}
	result, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:      historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("do the task")}, label: "carry"},
		Policy:     newRoundsPolicy(session, 1), // round 0 blocked with a nudge, round 1 finishes
		Executor:   &stubExecutor{},
		Model:      model,
		LogSession: session,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(model.seen) != 2 {
		t.Fatalf("expected 2 rounds, got %d", len(model.seen))
	}
	if result.FinalText != "confirmed" {
		t.Fatalf("final answer includes a superseded pre-audit draft: %q", result.FinalText)
	}

	round2 := model.call(1)
	var sawRoundOneWork, sawNudge bool
	nudgeIdx, workIdx := -1, -1
	for i, m := range round2 {
		text := msgText(m)
		if m.Role == fantasy.MessageRoleAssistant && strings.Contains(text, "round one analysis") {
			sawRoundOneWork = true
			workIdx = i
		}
		if strings.Contains(text, "keep going") {
			sawNudge = true
			nudgeIdx = i
		}
	}
	if !sawRoundOneWork {
		for i, m := range round2 {
			t.Logf("round2[%d] role=%s parts=%d text=%q", i, m.Role, len(m.Content), msgText(m))
		}
		t.Fatalf("round 2 input lacks round 1's assistant work — the model would restart the task from scratch; got %d messages", len(round2))
	}
	if !sawNudge {
		t.Fatal("round 2 input lacks the enforcement nudge")
	}
	if workIdx > nudgeIdx {
		t.Errorf("carried transcript (idx %d) must precede the nudge (idx %d) so the nudge reads as a follow-up", workIdx, nudgeIdx)
	}
}

type streamEventObserver struct {
	mu     sync.Mutex
	events []streamEvent
}

type streamEvent struct {
	typ     string
	payload map[string]any
}

func (o *streamEventObserver) Observe(eventType string, payload map[string]any) {
	copied := map[string]any{}
	for k, v := range payload {
		copied[k] = v
	}
	o.mu.Lock()
	o.events = append(o.events, streamEvent{typ: eventType, payload: copied})
	o.mu.Unlock()
}

func reconstructVisibleText(events []streamEvent) string {
	var b strings.Builder
	for _, e := range events {
		text, _ := e.payload["text"].(string)
		switch e.typ {
		case "text.delta":
			b.WriteString(text)
		case evtTextReplace:
			b.Reset()
			b.WriteString(text)
		}
	}
	return b.String()
}

func TestRun_LiveStreamRetractsPreAuditDraft(t *testing.T) {
	session := NewLogSession()
	model := &textCapturingModel{slug: "stream-replace-test", replies: []string{"round one analysis", "confirmed"}}
	obs := &streamEventObserver{}
	result, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:      historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("do the task")}, label: "stream-replace"},
		Policy:     newRoundsPolicy(session, 1),
		Executor:   &stubExecutor{},
		Model:      model,
		LogSession: session,
		Observer:   obs,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.FinalText != "confirmed" {
		t.Fatalf("FinalText = %q, want confirmed", result.FinalText)
	}
	obs.mu.Lock()
	events := append([]streamEvent(nil), obs.events...)
	obs.mu.Unlock()
	var lastReplace string
	var sawReplace bool
	for _, e := range events {
		if e.typ == evtTextReplace {
			sawReplace = true
			lastReplace, _ = e.payload["text"].(string)
		}
	}
	if !sawReplace {
		t.Fatal("expected text.replace with the authoritative final answer")
	}
	if lastReplace != "confirmed" {
		t.Fatalf("text.replace payload = %q, want confirmed", lastReplace)
	}
	if got := reconstructVisibleText(events); got != "confirmed" {
		t.Fatalf("live stream reconstructed %q, want the final round only", got)
	}
}

func TestRun_FinalRoundOnlyWhenProviderOmitsCompletedText(t *testing.T) {
	session := NewLogSession()
	model := &textCapturingModel{slug: "delta-only", replies: []string{"superseded draft", "final streamed answer"}, omitTextEnd: true}
	observer := &streamEventObserver{}
	result, err := Run(context.Background(), ModeScheduled, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:  historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("finish after audit")}, label: "delta-only"},
		Policy: newRoundsPolicy(session, 1), Executor: &stubExecutor{}, Model: model, Observer: observer, LogSession: session,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.seen) != 2 {
		t.Fatalf("got %d rounds", len(model.seen))
	}
	if result.FinalText != "final streamed answer" {
		t.Fatalf("final text includes earlier round: %q", result.FinalText)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if got := reconstructVisibleText(observer.events); got != result.FinalText {
		t.Fatalf("live result %q differs from stored %q", got, result.FinalText)
	}
}

func TestRunPreservesAllCompletedTextBlocks(t *testing.T) {
	session := NewLogSession()
	observer := &streamEventObserver{}
	result, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:  historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("answer")}, label: "multi-block"},
		Policy: newRoundsPolicy(session, 0), Executor: &stubExecutor{},
		Model:    &textCapturingModel{slug: "multi-block", replies: []string{"first "}, secondBlock: "second"},
		Observer: observer, LogSession: session,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText != "first second" {
		t.Fatalf("completed blocks truncated: %q", result.FinalText)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if got := reconstructVisibleText(observer.events); got != result.FinalText {
		t.Fatalf("live text %q != persisted result %q", got, result.FinalText)
	}
}

func TestRunRetractsFinalizeObserverDraftWhenFinalAnswerIsEmpty(t *testing.T) {
	observer := &streamEventObserver{}
	result, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input: stubInput{system: "s", user: "answer", label: "empty-finalize"}, Policy: passPolicy{},
		Model: &textCapturingModel{slug: "empty-main", replies: []string{""}}, Observer: observer,
		Finalize: func(_ context.Context, in FinalizeInput) (string, error) {
			in.Observer.Observe("text.delta", map[string]any{"text": "discarded finalize draft"})
			return "", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if result.FinalText != "" || reconstructVisibleText(observer.events) != "" {
		t.Fatalf("finalize draft survived: final=%q live=%q", result.FinalText, reconstructVisibleText(observer.events))
	}
}

func TestScheduledResultOmitsPreAuditDraft(t *testing.T) {
	session := NewLogSession()
	model := &textCapturingModel{slug: "scheduled-final-test", replies: []string{"FUTURE_PASS_391", "Audit complete. FUTURE_PASS_391"}}
	result, err := Run(context.Background(), ModeScheduled, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:  historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("compute and audit")}, label: "scheduled-final"},
		Policy: newRoundsPolicy(session, 1), Executor: &stubExecutor{}, Model: model, LogSession: session,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText != "Audit complete. FUTURE_PASS_391" {
		t.Fatalf("scheduled result concatenated superseded drafts: %q", result.FinalText)
	}
}

// The finalize seam rides the same carry (#1117): the hook's recovery calls
// replay FinalizeInput.Messages, so the loop must hand the hook the finishing
// round's input PLUS its completed tool transcript — the round input alone
// lacks the tool calls/results the forced summary is asked to write up. The
// loop also reports how many tool events the round committed, the datum the
// hook's ADR-0035 gate needs to refuse a blind with-tools re-drive.
func TestRunFinalizeInputCarriesRoundTranscriptAndToolEvents(t *testing.T) {
	probe := fantasy.NewAgentTool("probe_tool", "returns a canned result",
		func(context.Context, panicTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("spend=123"), nil
		})
	var got FinalizeInput
	_, err := Run(context.Background(), ModeInteractive, RunConfig{
		EnvPrefix:   CanonicalEnvPrefix,
		NativeTools: []fantasy.AgentTool{probe},
	}, Deps{
		Input:    stubInput{system: "sys", user: "pull the report", label: "finalize-carry"},
		Observer: &captureObserver{},
		Policy:   passPolicy{},
		Model:    scriptedToolModel([]scriptedToolCall{{id: "call-1", name: "probe_tool"}}),
		Finalize: func(_ context.Context, in FinalizeInput) (string, error) {
			got = in
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.RoundToolEvents != 2 {
		t.Errorf("RoundToolEvents = %d, want 2 (the round's committed tool call + result)", got.RoundToolEvents)
	}
	var sawUser, sawCall, sawResult bool
	for _, m := range got.Messages {
		if m.Role == fantasy.MessageRoleUser && strings.Contains(msgText(m), "pull the report") {
			sawUser = true
		}
		for _, part := range m.Content {
			if cp, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok && cp.ToolCallID == "call-1" {
				sawCall = true
			}
			if rp, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok && rp.ToolCallID == "call-1" {
				sawResult = true
			}
		}
	}
	if !sawUser {
		t.Error("FinalizeInput.Messages lacks the round's user message")
	}
	if !sawCall || !sawResult {
		t.Errorf("FinalizeInput.Messages lacks the round's tool transcript (call=%v result=%v) — a recovery call would fabricate from stale context", sawCall, sawResult)
	}
}

func TestCarryRoundMessages_DropsOrphanedToolCalls(t *testing.T) {
	res := &fantasy.AgentResult{
		Steps: []fantasy.StepResult{
			{
				FinishReason: fantasy.FinishReasonToolCalls,
				Messages: []fantasy.Message{
					{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
						fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "bash", Input: `{"command":"ls"}`},
					}},
					{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
						fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentText{Text: "ok"}},
					}},
				},
			},
			{
				// Step cut off by max_output_tokens (FinishReasonLength): provider
				// emitted text + a ToolCallPart, but fantasy did not execute the
				// tool call so no ToolResultPart exists.
				FinishReason: fantasy.FinishReasonLength,
				Messages: []fantasy.Message{
					{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
						fantasy.TextPart{Text: "Starting HTML build"},
						fantasy.ToolCallPart{ToolCallID: "orphan-1", ToolName: "run_python", Input: `{"code":"print('truncated`},
					}},
					{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
						fantasy.ReasoningPart{Text: "thinking"},
						fantasy.ToolCallPart{ToolCallID: "orphan-2", ToolName: "run_python", Input: `{}`},
					}},
				},
			},
		},
	}

	out := carryRoundMessages(res)
	if len(out) != 3 {
		t.Fatalf("carried %d messages, want 3 (step 1 assistant+tool, step 2 text-only assistant): %+v", len(out), out)
	}
	for _, m := range out {
		for _, part := range m.Content {
			if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok && strings.HasPrefix(tc.ToolCallID, "orphan-") {
				t.Fatalf("orphaned ToolCallPart %q leaked into carried round messages", tc.ToolCallID)
			}
		}
	}
	if got := msgText(out[2]); got != "Starting HTML build" {
		t.Fatalf("step 2 assistant text = %q, want %q", got, "Starting HTML build")
	}
}

// TestRun_ScheduledLengthCutoffDoesNotTriggerPrematureSelfAudit reproduces the
// Reklaim Daily Health Scan failure (task f4c58656): when a step in a scheduled
// run hits FinishReasonLength (max_output_tokens) mid-tool-call while
// task_tracker still has open items, Run must NOT flip selfAuditRequested and
// hand the model the self-audit ritual on the next round. Instead it must tell
// the model that its previous response hit finish_reason=length, show the open
// task_tracker items, strip any unexecuted ToolCallPart from the carried
// transcript, and hold the self-audit request until the tasks are marked done.
func TestRun_ScheduledLengthCutoffDoesNotTriggerPrematureSelfAudit(t *testing.T) {
	session := NewLogSession()
	pol := NewScheduledPolicy(session, 100, 0, 0)
	tracker := tools.NewTaskTrackerTool()

	var (
		mu       sync.Mutex
		stepCall int
		prompts  [][]fantasy.Message
	)
	model := &mockModel{
		streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
			mu.Lock()
			idx := stepCall
			stepCall++
			prompts = append(prompts, append([]fantasy.Message(nil), call.Prompt...))
			mu.Unlock()

			return func(yield func(fantasy.StreamPart) bool) {
				switch idx {
				case 0:
					// Round 0, step 0: register a task_tracker plan with open work.
					yield(fantasy.StreamPart{
						Type:          fantasy.StreamPartTypeToolCall,
						ID:            "tc-plan",
						ToolCallName:  toolNameTaskTracker,
						ToolCallInput: `{"command":"plan","task_list":` + pendingTaskListJSON + `}`,
					})
					yield(fantasy.StreamPart{
						Type:         fantasy.StreamPartTypeFinish,
						FinishReason: fantasy.FinishReasonToolCalls,
						Usage:        fantasy.Usage{InputTokens: 50, OutputTokens: 20},
					})
				case 1:
					// Round 0, step 1: model hits max_output_tokens mid-tool-call.
					yield(fantasy.StreamPart{
						Type:          fantasy.StreamPartTypeToolCall,
						ID:            "tc-truncated",
						ToolCallName:  "bash",
						ToolCallInput: `{"command":"echo truncated"}`,
					})
					yield(fantasy.StreamPart{
						Type:         fantasy.StreamPartTypeFinish,
						FinishReason: fantasy.FinishReasonLength,
						Usage:        fantasy.Usage{InputTokens: 60, OutputTokens: 16384},
					})
				case 2:
					// Round 1, step 0: after receiving the length + task_tracker
					// nudges, model marks all tasks done.
					allDone := `[
						{"id":"1","title":"download the report","status":"done"},
						{"id":"2","title":"compute the rollup","status":"done"},
						{"id":"3","title":"email the summary","status":"done"}]`
					yield(fantasy.StreamPart{
						Type:          fantasy.StreamPartTypeToolCall,
						ID:            "tc-done",
						ToolCallName:  toolNameTaskTracker,
						ToolCallInput: `{"command":"plan","task_list":` + allDone + `}`,
					})
					yield(fantasy.StreamPart{
						Type:         fantasy.StreamPartTypeFinish,
						FinishReason: fantasy.FinishReasonToolCalls,
						Usage:        fantasy.Usage{InputTokens: 70, OutputTokens: 20},
					})
				case 3:
					// Round 1, step 1: model finishes the work and stops.
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t1"})
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "Work complete, ready for audit."})
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"})
					yield(fantasy.StreamPart{
						Type:         fantasy.StreamPartTypeFinish,
						FinishReason: fantasy.FinishReasonStop,
						Usage:        fantasy.Usage{InputTokens: 80, OutputTokens: 10},
					})
				case 4:
					// Round 2, step 0: after receiving the self-audit request,
					// model calls confirm_audit(success=true, critical_actions=[]).
					yield(fantasy.StreamPart{
						Type:          fantasy.StreamPartTypeToolCall,
						ID:            "tc-audit",
						ToolCallName:  toolNameConfirmAudit,
						ToolCallInput: `{"success":true,"reasoning":"verified all outputs","artifacts_checked":["report.html"],"workflow_sections_checked":["delivery"],"critical_actions":[],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`,
					})
					yield(fantasy.StreamPart{
						Type:         fantasy.StreamPartTypeFinish,
						FinishReason: fantasy.FinishReasonToolCalls,
						Usage:        fantasy.Usage{InputTokens: 90, OutputTokens: 30},
					})
				default:
					// Round 2, step 1: final summary.
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t2"})
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t2", Delta: "Report delivered cleanly."})
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t2"})
					yield(fantasy.StreamPart{
						Type:         fantasy.StreamPartTypeFinish,
						FinishReason: fantasy.FinishReasonStop,
						Usage:        fantasy.Usage{InputTokens: 100, OutputTokens: 10},
					})
				}
			}, nil
		},
	}

	res, err := Run(context.Background(), ModeScheduled, RunConfig{
		EnvPrefix:           CanonicalEnvPrefix,
		IncludeConfirmAudit: true,
		NativeTools:         []fantasy.AgentTool{tracker},
	}, Deps{
		Input:      historyInput{system: "sys", msgs: []fantasy.Message{fantasy.NewUserMessage("run daily scan")}, label: "reklaim-scan"},
		Policy:     pol,
		Executor:   &stubExecutor{},
		Model:      model,
		LogSession: session,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalText != "Report delivered cleanly." {
		t.Fatalf("FinalText = %q, want %q", res.FinalText, "Report delivered cleanly.")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(prompts) < 5 {
		t.Fatalf("expected at least 5 model calls across 3 rounds, got %d", len(prompts))
	}

	// Inspect the prompt sent at the start of Round 1 (call index 2):
	// - must NOT contain the orphaned tc-truncated tool call
	// - must NOT demand confirm_audit yet
	// - MUST contain both the finish_reason=length nudge and the task_tracker nudge
	round1Prompt := prompts[2]
	var sawLengthNudge, sawTrackerNudge, sawPrematureAudit bool
	for _, m := range round1Prompt {
		for _, part := range m.Content {
			if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok && tc.ToolCallID == "tc-truncated" {
				t.Fatal("Round 1 prompt carried orphaned ToolCallPart tc-truncated from FinishReasonLength step")
			}
		}
		text := msgText(m)
		if strings.Contains(text, "finish_reason=length") {
			sawLengthNudge = true
		}
		if strings.Contains(text, "Task tracker: 1 todo, 1 in progress.") && strings.Contains(text, "Task List:") {
			sawTrackerNudge = true
		}
		if strings.Contains(text, "Before finishing:") {
			sawPrematureAudit = true
		}
	}
	if !sawLengthNudge {
		t.Fatal("Round 1 prompt missing finish_reason=length truncation nudge")
	}
	if !sawTrackerNudge {
		t.Fatal("Round 1 prompt missing task_tracker pending-work nudge with rendered plan")
	}
	if sawPrematureAudit {
		t.Fatal("Round 1 prompt prematurely requested self-audit while work remained")
	}
}

// A Gemini thought signature rides on a reasoning part, and Gemini wants it
// back with its tool call on later requests of the same turn (#1703). Replay
// keeps that signature, with any thought text dropped, and still drops every
// other reasoning part; a message left with nothing but a signature goes too.
func TestReplayKeepsGeminiThoughtSignatures(t *testing.T) {
	sig := &google.ReasoningMetadata{Signature: "sig-1", ToolID: "c1"}
	out := messagesForReplay([]fantasy.Message{
		fantasy.NewUserMessage("task"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ReasoningPart{Text: "thought text", ProviderOptions: fantasy.ProviderOptions{google.Name: sig}},
			fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "bash", Input: `{"command":"ls"}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "c1"}}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ReasoningPart{Text: "anthropic thinking", ProviderOptions: fantasy.ProviderOptions{
				anthropic.Name: &anthropic.ReasoningOptionMetadata{Signature: "a-sig"},
			}},
			fantasy.TextPart{Text: "done"},
		}},
		// A signature whose call was never run: the call is dropped, so is the
		// message.
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ReasoningPart{ProviderOptions: fantasy.ProviderOptions{google.Name: &google.ReasoningMetadata{Signature: "sig-2"}}},
			fantasy.ToolCallPart{ToolCallID: "c2", ToolName: "bash", Input: `{}`},
		}},
	})
	if len(out) != 4 {
		t.Fatalf("replayed %d messages, want 4 (user, signed call, result, text): %+v", len(out), out)
	}
	first, ok := out[1].Content[0].(fantasy.ReasoningPart)
	if !ok || len(out[1].Content) != 2 {
		t.Fatalf("signed call message = %+v, want the signature then the call", out[1].Content)
	}
	if md := google.GetReasoningMetadata(first.Options()); first.Text != "" || md == nil || md.Signature != "sig-1" || md.ToolID != "c1" {
		t.Errorf("kept reasoning part = %+v, want signature sig-1 for c1 and no text", first)
	}
	if len(out[3].Content) != 1 {
		t.Errorf("anthropic reasoning survived replay: %+v", out[3].Content)
	}
}
