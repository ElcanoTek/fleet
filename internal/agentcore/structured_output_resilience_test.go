package agentcore

// Review pins for the terminal-phase resilience fixes: in-phase transient
// retry, transient-vs-format classification, native strict downgrade, and the
// terminal prompt budget guard.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
)

// flakyTerminalModel fails Generate with the scripted errors first, then
// serves the scripted responses.
type flakyTerminalModel struct {
	mockModel
	provider string
	slug     string

	mu        sync.Mutex
	errs      []error
	responses []*fantasy.Response
	calls     []fantasy.Call
}

func (m *flakyTerminalModel) Provider() string { return m.provider }
func (m *flakyTerminalModel) Model() string    { return m.slug }

func (m *flakyTerminalModel) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, call)
	if len(m.errs) > 0 {
		err := m.errs[0]
		m.errs = m.errs[1:]
		return nil, err
	}
	if len(m.responses) == 0 {
		return nil, errors.New("unexpected terminal generation")
	}
	resp := m.responses[0]
	m.responses = m.responses[1:]
	return resp, nil
}

func (m *flakyTerminalModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func transient502() error {
	return &fantasy.ProviderError{Message: "bad gateway", StatusCode: 502}
}

func fatal400() error {
	return &fantasy.ProviderError{Message: "invalid response_format schema", StatusCode: 400}
}

func TestTerminalGenerate_TransientBlipRetriedInPhase(t *testing.T) {
	model := &flakyTerminalModel{
		provider:  "anthropic",
		slug:      "claude-test",
		errs:      []error{transient502(), transient502()},
		responses: []*fantasy.Response{terminalToolResponse(`{"answer":42}`)},
	}
	eng, orch := testTerminalEngine(model)
	out, err := eng.generateTerminalStructuredOutput(
		context.Background(), model, "system", []fantasy.Message{fantasy.NewUserMessage("work")},
		json.RawMessage(terminalTestSchema), 1000, orch,
	)
	if err != nil {
		t.Fatalf("transient blips must be absorbed in-phase: %v", err)
	}
	if string(out) != `{"answer":42}` {
		t.Fatalf("output = %s", out)
	}
	if model.callCount() != 3 {
		t.Fatalf("calls = %d, want 3 (2 blips + success)", model.callCount())
	}
}

func TestTerminalGenerate_TransientExhaustionIsTransientClassNotFormat(t *testing.T) {
	model := &flakyTerminalModel{
		provider: "anthropic",
		slug:     "claude-test",
		errs:     []error{transient502(), transient502(), transient502(), transient502(), transient502()},
	}
	eng, orch := testTerminalEngine(model)
	_, err := eng.generateTerminalStructuredOutput(
		context.Background(), model, "system", []fantasy.Message{fantasy.NewUserMessage("work")},
		json.RawMessage(terminalTestSchema), 1000, orch,
	)
	if err == nil {
		t.Fatal("want error after exhausted transient retries")
	}
	// The runner maps ErrRetryBudgetExhausted to the TRANSIENT re-queue class.
	// Mapping this to the format class would DLQ a run whose committed tool
	// work was fine and whose only fault was provider weather.
	if !errors.Is(err, ErrRetryBudgetExhausted) {
		t.Fatalf("want ErrRetryBudgetExhausted, got: %v", err)
	}
	if errors.Is(err, ErrStructuredOutputFormat) {
		t.Fatalf("transient exhaustion misclassified as format failure: %v", err)
	}
}

func TestTerminalGenerate_FatalErrorFailsWithoutRetry(t *testing.T) {
	model := &flakyTerminalModel{
		provider: "anthropic", // non-OpenRouter: tool path, no native downgrade
		slug:     "claude-test",
		errs:     []error{fatal400()},
	}
	eng, orch := testTerminalEngine(model)
	_, err := eng.generateTerminalStructuredOutput(
		context.Background(), model, "system", []fantasy.Message{fantasy.NewUserMessage("work")},
		json.RawMessage(terminalTestSchema), 1000, orch,
	)
	if !errors.Is(err, ErrStructuredOutputGeneration) {
		t.Fatalf("want ErrStructuredOutputGeneration, got: %v", err)
	}
	if model.callCount() != 1 {
		t.Fatalf("fatal errors must not retry: %d calls", model.callCount())
	}
}

func TestTerminalGenerate_NativeStrictRejectionDowngradesToToolPath(t *testing.T) {
	model := &flakyTerminalModel{
		provider:  "openrouter", // native strict path first
		slug:      "openai/gpt-test",
		errs:      []error{fatal400()}, // upstream rejects the strict subset
		responses: []*fantasy.Response{terminalToolResponse(`{"answer":42}`)},
	}
	eng, orch := testTerminalEngine(model)
	out, err := eng.generateTerminalStructuredOutput(
		context.Background(), model, "system", []fantasy.Message{fantasy.NewUserMessage("work")},
		json.RawMessage(terminalTestSchema), 1000, orch,
	)
	if err != nil {
		t.Fatalf("strict rejection must downgrade to the forced-tool path: %v", err)
	}
	if string(out) != `{"answer":42}` {
		t.Fatalf("output = %s", out)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (native rejected + tool-path success)", len(model.calls))
	}
	if len(model.calls[0].Tools) != 0 {
		t.Fatal("first call should be native strict (no tools)")
	}
	if len(model.calls[1].Tools) != 1 || model.calls[1].Tools[0].GetName() != structuredOutputToolName {
		t.Fatalf("second call did not downgrade to the forced tool: %#v", model.calls[1].Tools)
	}
}

func TestBoundTerminalPrompt_ReducesOversizedTranscript(t *testing.T) {
	model := &flakyTerminalModel{provider: "anthropic", slug: "deepseek/context-budget-test"}
	huge := strings.Repeat("x", 3*HardMaxToolOutputBytes)
	messages := []fantasy.Message{
		fantasy.NewUserMessage("start"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "bash", Input: `{"cmd":"big"}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentText{Text: huge}},
		}},
	}
	bounded, err := boundTerminalPrompt(model, "system", messages, 1000)
	if err != nil {
		t.Fatalf("boundTerminalPrompt: %v", err)
	}
	after := estimateModelMessagesTokens(bounded)
	before := estimateModelMessagesTokens(messages)
	if after >= before {
		t.Fatalf("terminal prompt not reduced: before=%d after=%d", before, after)
	}
	// The original slice must not be mutated (the caller may still need it).
	if got := estimateModelMessagesTokens(messages); got != before {
		t.Fatalf("input messages mutated: %d -> %d", before, got)
	}
}
