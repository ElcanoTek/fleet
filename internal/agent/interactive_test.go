package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// itMockModel is a configurable fantasy.LanguageModel for the interactive
// driver wiring tests (finalize hook + compaction summarizer + 1-round
// collapse). Stream/Generate are pluggable; defaults finish with text.
type itMockModel struct {
	mu           sync.Mutex
	streamCount  int
	generateText string
	streamFunc   func(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error)
}

func (m *itMockModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	text := m.generateText
	if text == "" {
		text = "summary text"
	}
	return &fantasy.Response{
		Content:      []fantasy.Content{fantasy.TextContent{Text: text}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

func (m *itMockModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.streamCount++
	fn := m.streamFunc
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, call)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 10, OutputTokens: 5},
		})
	}, nil
}

func (m *itMockModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *itMockModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *itMockModel) Provider() string { return "mock" }
func (m *itMockModel) Model() string    { return "mock-model" }

type captureObs struct{ events []string }

func (o *captureObs) Observe(eventType string, _ map[string]any) {
	o.events = append(o.events, eventType)
}

// TestInteractiveFinalize_ForcesSummaryOnEmptyText verifies the finalize hook's
// forced-final-summary path: when the turn produced no user-visible text, the
// hook makes a tool-less follow-up call and returns the recovered answer.
func TestInteractiveFinalize_ForcesSummaryOnEmptyText(t *testing.T) {
	model := &itMockModel{
		// The force-summary follow-up streams a written answer.
		streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
			return func(yield func(fantasy.StreamPart) bool) {
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "Here is the answer."}) {
					return
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}, nil
		},
	}
	tc := TurnConfig{
		SystemPrompt: "sys",
		Model:        model,
		MaxTokens:    1024,
		TurnHistory: []HistoryEntry{
			mustEntry("user", "text", TextContent{Text: "pull the report"}),
			mustEntry("assistant", entryTypeToolCall, ToolCallContent{ID: "c1", Name: "run_python", Input: "{}"}),
			mustEntry("tool", "tool_result", ToolResultContent{ID: "c1", Name: "run_python", Text: `{"output":"spend=123"}`}),
		},
	}
	hook := buildInteractiveFinalize(tc)
	obs := &captureObs{}
	recovered, err := hook(context.Background(), agentcore.FinalizeInput{
		Mode:         agentcore.ModeInteractive,
		FinalText:    "", // turn ended with no text
		Observer:     obs,
		SystemPrompt: "sys",
	})
	if err != nil {
		t.Fatalf("finalize hook error: %v", err)
	}
	if recovered != "Here is the answer." {
		t.Errorf("recovered = %q, want forced summary text", recovered)
	}
}

// TestInteractiveFinalize_StripsLeakedCall verifies the hook returns the
// stripped text when the reply was real prose with a stray leaked call inline
// (no follow-up model call needed).
func TestInteractiveFinalize_StripsLeakedCall(t *testing.T) {
	model := &itMockModel{}
	tc := TurnConfig{SystemPrompt: "sys", Model: model, MaxTokens: 1024}
	hook := buildInteractiveFinalize(tc)
	recovered, err := hook(context.Background(), agentcore.FinalizeInput{
		FinalText:    "Done — see the table.\ncall:default_api:download_url{url:https://x/y}\nMore below.",
		SystemPrompt: "sys",
	})
	if err != nil {
		t.Fatalf("finalize hook error: %v", err)
	}
	if strings.Contains(recovered, "call:default_api") {
		t.Errorf("leaked call not stripped: %q", recovered)
	}
	if !strings.Contains(recovered, "Done — see the table.") || !strings.Contains(recovered, "More below.") {
		t.Errorf("real prose lost: %q", recovered)
	}
	// No follow-up stream should have fired (real text survived stripping).
	if model.streamCount != 0 {
		t.Errorf("unexpected follow-up stream calls: %d", model.streamCount)
	}
}

// TestInteractiveCompactionSummarizer_TagsSummary verifies the compaction
// summarizer produces a message tagged with the compaction prefix (so the cache
// layer treats it as a stable boundary) carrying the model's summary text.
func TestInteractiveCompactionSummarizer_TagsSummary(t *testing.T) {
	model := &itMockModel{generateText: "condensed brief"}
	tc := TurnConfig{Model: model, MaxTokens: 4096}
	summarizer := buildInteractiveCompactionSummarizer(tc)
	droppable := []fantasy.Message{
		fantasy.NewUserMessage("old turn 1"),
		fantasy.NewUserMessage("old turn 2"),
	}
	msg := summarizer(context.Background(), droppable)
	text := ""
	for _, part := range msg.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			text += tp.Text
		}
	}
	if !strings.HasPrefix(text, compactionSummaryPrefix) {
		t.Errorf("summary not tagged with compaction prefix: %q", text)
	}
	if !strings.Contains(text, "condensed brief") {
		t.Errorf("summary text missing: %q", text)
	}
}

// TestRunInteractiveTurn_OneRoundCollapse verifies the interactive driver
// collapses the shared loop to a single pass (InteractivePolicy CanFinish true
// at round 0) and returns the streamed text.
func TestRunInteractiveTurn_OneRoundCollapse(t *testing.T) {
	model := &itMockModel{
		streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
			return func(yield func(fantasy.StreamPart) bool) {
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "hello"}) {
					return
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}, nil
		},
	}
	tc := TurnConfig{
		SystemPrompt: "sys",
		Messages:     []fantasy.Message{fantasy.NewUserMessage("hi")},
		Label:        "turn-1",
		Model:        model,
		MaxTokens:    1024,
	}
	obs := &captureObs{}
	res, err := RunInteractiveTurn(context.Background(), tc, obs)
	if err != nil {
		t.Fatalf("RunInteractiveTurn: %v", err)
	}
	if res.Rounds != 1 {
		t.Errorf("interactive turn should collapse to 1 round, got %d", res.Rounds)
	}
	if res.FinalText != "hello" {
		t.Errorf("FinalText = %q, want hello", res.FinalText)
	}
	if res.Label != "turn-1" {
		t.Errorf("Label = %q, want turn-1", res.Label)
	}
}

// TestACPInteractiveFallback asserts the governance-honesty gate is now EMPTY:
// P-ACP-2b closed every reason the P-ACP-1 gate carried (MCP, approval staging,
// memory/note staging, lockdown) by extending the host-side delegation, so
// native-acp runs FULLY GOVERNED in every case it previously fell back. The gate
// stays as the single auditable hook to re-introduce a fallback if a new governed
// surface ever lands before its delegation seam.
func TestACPInteractiveFallback(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*TurnConfig)
	}{
		{"clean", func(*TurnConfig) {}},
		{"lockdown", func(tc *TurnConfig) { tc.Lockdown = true }},
		{"mcp", func(tc *TurnConfig) { tc.Selection = agentcore.MCPSelection{{Server: "x"}} }},
		{"approvals", func(tc *TurnConfig) { tc.ApprovalStager = stubApprovalStager{} }},
		{"memory", func(tc *TurnConfig) { tc.MemoryProposer = stubMemoryProposer{} }},
		{"note", func(tc *TurnConfig) { tc.NoteProposer = stubNoteProposer{} }},
		{"all", func(tc *TurnConfig) {
			tc.Lockdown = true
			tc.Selection = agentcore.MCPSelection{{Server: "x"}}
			tc.ApprovalStager = stubApprovalStager{}
			tc.MemoryProposer = stubMemoryProposer{}
			tc.NoteProposer = stubNoteProposer{}
		}},
	}
	for _, tcase := range cases {
		t.Run(tcase.name, func(t *testing.T) {
			tc := TurnConfig{Runtime: "native-acp", NativeAgentImage: "img"}
			tcase.mutate(&tc)
			if reason := acpInteractiveFallback(tc); reason != "" {
				t.Fatalf("acpInteractiveFallback(%s) = %q, want empty (native-acp fully governs this case in P-ACP-2b)", tcase.name, reason)
			}
		})
	}
}

type stubApprovalStager struct{}

func (stubApprovalStager) Stage(string, string, string) (string, error)   { return "", nil }
func (stubApprovalStager) StageSuggestion(string) (string, string, error) { return "", "", nil }

type stubMemoryProposer struct{}

func (stubMemoryProposer) Propose(string) (string, error) { return "", nil }

type stubNoteProposer struct{}

func (stubNoteProposer) Propose(_, _, _, _ string) (string, error) { return "", nil }
