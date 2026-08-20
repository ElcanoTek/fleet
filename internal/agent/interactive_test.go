package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// itMockModel is a configurable fantasy.LanguageModel for the interactive
// driver wiring tests (finalize hook + compaction summarizer + 1-round
// collapse). Stream/Generate are pluggable; defaults finish with text.
type itMockModel struct {
	mu            sync.Mutex
	streamCount   int
	generateCount int
	generateText  string
	streamFunc    func(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error)
}

func (m *itMockModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	m.mu.Lock()
	m.generateCount++
	m.mu.Unlock()
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

func TestCompactionSummarizerRefusesOversizedInputBeforeProvider(t *testing.T) {
	model := &itMockModel{generateText: "must not run"}
	droppable := []fantasy.Message{fantasy.NewUserMessage(strings.Repeat("ordinary historical prose ", 50_000))}
	summary := summarizeDroppedMiddle(context.Background(), TurnConfig{Model: model},
		agentcore.CompactionSummarizeInput{Droppable: droppable})
	if !strings.Contains(summary, "messages compacted") {
		t.Fatalf("oversized summarizer did not use deterministic placeholder: %q", summary)
	}
	model.mu.Lock()
	calls := model.generateCount
	model.mu.Unlock()
	if calls != 0 {
		t.Fatalf("oversized summarizer reached provider %d times", calls)
	}
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

// promptHasUserText reports whether any user-role message in msgs carries want
// inside a text part.
func promptHasUserText(msgs []fantasy.Message, want string) bool {
	for _, m := range msgs {
		if m.Role != fantasy.MessageRoleUser {
			continue
		}
		for _, part := range m.Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && strings.Contains(tp.Text, want) {
				return true
			}
		}
	}
	return false
}

// TestInteractiveFinalize_ForceSummarySeesTurnWork is the #1117 regression
// guard for the forced-final-summary recovery. It drives a full turn with the
// TurnConfig manager.RunTurn actually builds (Messages only — there is no
// separate turn-history field left to hand-wire, which is how the old test
// exercised wiring production never had), makes the round execute a tool and
// end with no prose (the exact forced-summary trigger), and asserts on what
// the recovery call RECEIVES: the current user question and the round's tool
// results, with no tool roster attached. The old PriorHistory/TurnHistory
// replay handed the recovery call prior turns only (TurnHistory was never set
// in production), so the "recovered" answer was fabricated from stale context.
func TestInteractiveFinalize_ForceSummarySeesTurnWork(t *testing.T) {
	broker := &interactiveRecordingBroker{}
	var (
		mu            sync.Mutex
		calls         int
		summaryPrompt []fantasy.Message
		summarySeen   bool
	)
	model := &itMockModel{
		streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
			mu.Lock()
			calls++
			n := calls
			if len(call.Tools) == 0 {
				// The forced-summary follow-up is the turn's only tool-less
				// call; capture exactly what it was given to replay.
				summaryPrompt = append([]fantasy.Message(nil), call.Prompt...)
				summarySeen = true
			}
			mu.Unlock()
			return func(yield func(fantasy.StreamPart) bool) {
				switch n {
				case 1:
					// Round step 1: call the governed MCP tool.
					if !yield(fantasy.StreamPart{
						Type: fantasy.StreamPartTypeToolCall, ID: "mcp-1",
						ToolCallName: "mcp_bundle_lookup", ToolCallInput: `{}`,
					}) {
						return
					}
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				case 2:
					// Round step 2: stop with NO prose — the forced-summary trigger.
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
				default:
					// The forced-summary follow-up writes the answer.
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "Spend was 123."}) {
						return
					}
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
				}
			}, nil
		},
	}
	tc := TurnConfig{
		SystemPrompt: "sys",
		Messages:     []fantasy.Message{fantasy.NewUserMessage("look it up")},
		Model:        model,
		MaxTokens:    1024,
		MCPBroker:    broker,
		MCPCatalog: []mcp.ServerTool{{
			ServerName: "bundle",
			Tool:       mcp.Tool{Name: "lookup", Description: "lookup"},
		}},
	}
	res, err := RunInteractiveTurn(context.Background(), tc, &captureObs{})
	if err != nil {
		t.Fatalf("RunInteractiveTurn: %v", err)
	}
	if res.FinalText != "Spend was 123." {
		t.Fatalf("FinalText = %q, want the forced summary text", res.FinalText)
	}
	mu.Lock()
	defer mu.Unlock()
	if !summarySeen {
		t.Fatal("forced-summary follow-up (the tool-less call) never fired")
	}
	// The recovery call must see the CURRENT turn, not just prior history:
	// the user's question and the tool work it is being asked to summarize.
	if !promptHasUserText(summaryPrompt, "look it up") {
		t.Errorf("forced-summary prompt lacks the current user question; got %d messages", len(summaryPrompt))
	}
	assertWellFormedToolPairs(t, summaryPrompt)
	if got, _, ok := toolResultTextFor(t, summaryPrompt, "mcp-1"); !ok || !contains([]byte(got), "broker-result") {
		t.Errorf("forced-summary prompt lacks the round's tool result: got=%q ok=%v", got, ok)
	}
}

// TestInteractiveFinalize_LeakedCallRetrySuppressedAfterToolExecution mirrors
// ADR-0035's TestStreamRoundSuppressesRecoveryAfterToolExecution at the
// finalize seam: a round that EXECUTED a tool (side effect committed) and then
// narrated a leaked `call:...{...}` must not be blindly re-driven with the
// governed roster — the model could re-issue the executed call and repeat a
// real MCP write. The hook must degrade to the tool-less forced summary, so
// the broker sees exactly one call.
func TestInteractiveFinalize_LeakedCallRetrySuppressedAfterToolExecution(t *testing.T) {
	broker := &interactiveRecordingBroker{}
	var (
		mu       sync.Mutex
		calls    int
		reIssued bool
	)
	model := &itMockModel{
		streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
			mu.Lock()
			calls++
			n := calls
			withTools := len(call.Tools) > 0
			reIssue := false
			if n > 2 && withTools && !reIssued {
				// A blind with-tools re-drive: the model rationally re-issues
				// the call whose result it never narrated (fresh arguments, so
				// the byte-identical repeat guard cannot mask the regression).
				reIssued = true
				reIssue = true
			}
			mu.Unlock()
			return func(yield func(fantasy.StreamPart) bool) {
				switch {
				case n == 1:
					// Round step 1: execute the governed MCP tool for real.
					if !yield(fantasy.StreamPart{
						Type: fantasy.StreamPartTypeToolCall, ID: "mcp-1",
						ToolCallName: "mcp_bundle_lookup", ToolCallInput: `{}`,
					}) {
						return
					}
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				case n == 2:
					// Round step 2: narrate a leaked call and stop — strips to
					// empty, so the finalize hook sees the leaked-call trigger.
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "call:default_api:download_url{url:https://x/y}"}) {
						return
					}
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
				case reIssue:
					if !yield(fantasy.StreamPart{
						Type: fantasy.StreamPartTypeToolCall, ID: "mcp-2",
						ToolCallName: "mcp_bundle_lookup", ToolCallInput: `{"q":"again"}`,
					}) {
						return
					}
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				default:
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "summary after side effects"}) {
						return
					}
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
				}
			}, nil
		},
	}
	tc := TurnConfig{
		SystemPrompt: "sys",
		Messages:     []fantasy.Message{fantasy.NewUserMessage("download it")},
		Model:        model,
		MaxTokens:    1024,
		// Belt and braces: cap the steps so a regression re-driving with tools
		// cannot loop the mock forever.
		MaxIterations: 6,
		MCPBroker:     broker,
		MCPCatalog: []mcp.ServerTool{{
			ServerName: "bundle",
			Tool:       mcp.Tool{Name: "lookup", Description: "lookup"},
		}},
	}
	res, err := RunInteractiveTurn(context.Background(), tc, &captureObs{})
	if err != nil {
		t.Fatalf("RunInteractiveTurn: %v", err)
	}
	if broker.calls != 1 {
		t.Fatalf("broker executed %d calls, want 1 — the finalize retry re-drove a round that had already committed a side effect (ADR-0035)", broker.calls)
	}
	if res.FinalText != "summary after side effects" {
		t.Fatalf("FinalText = %q, want the degraded tool-less summary", res.FinalText)
	}
}

// TestInteractiveFinalize_LeakedCallRetryRunsWithoutSideEffects pins the other
// half of the gate: a round with NO committed tool event that narrated only a
// leaked call is still re-driven WITH tools, so the intended action actually
// executes (the pre-existing recovery, which the ADR-0035 gate must not kill).
func TestInteractiveFinalize_LeakedCallRetryRunsWithoutSideEffects(t *testing.T) {
	broker := &interactiveRecordingBroker{}
	var (
		mu    sync.Mutex
		calls int
	)
	model := &itMockModel{
		streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			return func(yield func(fantasy.StreamPart) bool) {
				switch n {
				case 1:
					// The whole round is one leaked narration — zero tool events.
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "call:default_api:lookup{q:x}"}) {
						return
					}
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
				case 2:
					// The leaked-call retry makes the intended call for real.
					if !yield(fantasy.StreamPart{
						Type: fantasy.StreamPartTypeToolCall, ID: "mcp-1",
						ToolCallName: "mcp_bundle_lookup", ToolCallInput: `{}`,
					}) {
						return
					}
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				default:
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "did the real call"}) {
						return
					}
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
				}
			}, nil
		},
	}
	tc := TurnConfig{
		SystemPrompt:  "sys",
		Messages:      []fantasy.Message{fantasy.NewUserMessage("look it up")},
		Model:         model,
		MaxTokens:     1024,
		MaxIterations: 6,
		MCPBroker:     broker,
		MCPCatalog: []mcp.ServerTool{{
			ServerName: "bundle",
			Tool:       mcp.Tool{Name: "lookup", Description: "lookup"},
		}},
	}
	res, err := RunInteractiveTurn(context.Background(), tc, &captureObs{})
	if err != nil {
		t.Fatalf("RunInteractiveTurn: %v", err)
	}
	if broker.calls != 1 {
		t.Fatalf("broker executed %d calls, want 1 — the retry must still run when the round committed nothing", broker.calls)
	}
	if res.FinalText != "did the real call" {
		t.Fatalf("FinalText = %q, want the retry's recovered text", res.FinalText)
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
	msg := summarizer(context.Background(), agentcore.CompactionSummarizeInput{Droppable: droppable})
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

type interactiveRecordingBroker struct {
	server string
	tool   string
	calls  int
}

func (b *interactiveRecordingBroker) CallMCP(_ context.Context, server, tool string, _ map[string]any) (string, bool, error) {
	b.server = server
	b.tool = tool
	b.calls++
	return "broker-result", false, nil
}

func TestRunInteractiveTurn_UsesInjectedMCPBrokerAndCatalog(t *testing.T) {
	broker := &interactiveRecordingBroker{}
	calls := 0
	model := &itMockModel{
		streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
			calls++
			round := calls
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
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "done"})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}, nil
		},
	}
	tc := TurnConfig{
		SystemPrompt: "sys",
		Messages:     []fantasy.Message{fantasy.NewUserMessage("look it up")},
		Model:        model,
		MaxTokens:    1024,
		MCPBroker:    broker,
		MCPCatalog: []mcp.ServerTool{{
			ServerName: "bundle",
			Tool:       mcp.Tool{Name: "lookup", Description: "lookup"},
		}},
	}
	res, err := RunInteractiveTurn(context.Background(), tc, &captureObs{})
	if err != nil {
		t.Fatalf("RunInteractiveTurn: %v", err)
	}
	if res.FinalText != "done" {
		t.Fatalf("FinalText = %q, want done", res.FinalText)
	}
	if calls != 2 {
		t.Fatalf("model calls = %d, want tool round plus final round", calls)
	}
	if broker.calls != 1 || broker.server != "bundle" || broker.tool != "lookup" {
		t.Fatalf("broker calls = %d (%q.%q), want one bundle.lookup", broker.calls, broker.server, broker.tool)
	}
}

// recordingNoteProposer records the slug of the propose_note call routed to it,
// so a test can prove the tool was both registered and wired to the proposer.
type recordingNoteProposer struct{ slug string }

func (n *recordingNoteProposer) Propose(slug, _, _, _ string) (string, error) {
	n.slug = slug
	return "note-1", nil
}

// TestRunInteractiveTurn_ProposeNoteWiredWhenProposerSet proves the single
// agentcore-boundary guarantee for propose_note (issue #40): when a NoteProposer
// is set on the TurnConfig, RunInteractiveTurn both REGISTERS the propose_note
// tool (so the model's call is accepted, not rejected as unknown) and WIRES the
// proposer (so the call routes to it) — advertised, registered, and wired in
// lockstep.
func TestRunInteractiveTurn_ProposeNoteWiredWhenProposerSet(t *testing.T) {
	np := &recordingNoteProposer{}
	calls := 0
	model := &itMockModel{
		streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
			calls++
			round := calls
			return func(yield func(fantasy.StreamPart) bool) {
				if round == 1 {
					// Round 0: emit a propose_note tool call (intercepted by the gate).
					yield(fantasy.StreamPart{
						Type: fantasy.StreamPartTypeToolCall, ID: "pn-1",
						ToolCallName: "propose_note", ToolCallInput: `{"slug":"s","title":"t","body":"b","reason":"because"}`,
					})
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
					return
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "done"})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}, nil
		},
	}
	tc := TurnConfig{
		SystemPrompt: "sys",
		Messages:     []fantasy.Message{fantasy.NewUserMessage("save a note")},
		Model:        model,
		MaxTokens:    1024,
		NoteProposer: np,
	}
	if _, err := RunInteractiveTurn(context.Background(), tc, &captureObs{}); err != nil {
		t.Fatalf("RunInteractiveTurn: %v", err)
	}
	if np.slug != "s" {
		t.Fatalf("propose_note did not route to the wired proposer (slug=%q); tool must be registered + wired", np.slug)
	}
}
