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
	slug    string
	replies []string

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
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"}) {
			return
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
	_, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
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
