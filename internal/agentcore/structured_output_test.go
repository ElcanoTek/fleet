package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"
)

const terminalTestSchema = `{
  "type":"object",
  "properties":{"answer":{"type":"integer"}},
  "required":["answer"],
  "additionalProperties":false
}`

type terminalTestModel struct {
	mockModel
	provider string
	model    string

	mu        sync.Mutex
	responses []*fantasy.Response
	generate  []fantasy.Call
}

func (m *terminalTestModel) Provider() string { return m.provider }
func (m *terminalTestModel) Model() string    { return m.model }

func (m *terminalTestModel) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generate = append(m.generate, call)
	if len(m.responses) == 0 {
		return nil, errors.New("unexpected terminal generation")
	}
	resp := m.responses[0]
	m.responses = m.responses[1:]
	return resp, nil
}

func (m *terminalTestModel) calls() []fantasy.Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]fantasy.Call(nil), m.generate...)
}

func terminalToolResponse(input string) *fantasy.Response {
	return &fantasy.Response{
		Content: fantasy.ResponseContent{fantasy.ToolCallContent{
			ToolCallID: "terminal-call",
			ToolName:   structuredOutputToolName,
			Input:      input,
		}},
		FinishReason: fantasy.FinishReasonToolCalls,
		Usage:        fantasy.Usage{InputTokens: 20, OutputTokens: 5},
	}
}

func testTerminalEngine(model fantasy.LanguageModel) (*engine, *orchestrationState) {
	session := NewLogSession()
	orch := newOrchestrationState(session, 10)
	return newRunEngine(RunConfig{}, Deps{Model: model}, session), orch
}

func TestTerminalStructuredOutput_InvalidThenCorrected_NoOrdinaryTools(t *testing.T) {
	model := &terminalTestModel{
		provider: "anthropic",
		model:    "claude-test",
		responses: []*fantasy.Response{
			terminalToolResponse(`{"answer":"wrong"}`),
			terminalToolResponse(`{"answer":42}`),
		},
	}
	eng, orch := testTerminalEngine(model)
	out, err := eng.generateTerminalStructuredOutput(
		context.Background(), model, "system", []fantasy.Message{fantasy.NewUserMessage("completed work")},
		json.RawMessage(terminalTestSchema), 1000, orch,
	)
	if err != nil {
		t.Fatalf("generateTerminalStructuredOutput: %v", err)
	}
	if string(out) != `{"answer":42}` {
		t.Fatalf("output = %s", out)
	}
	calls := model.calls()
	if len(calls) != 2 {
		t.Fatalf("terminal calls = %d, want 2", len(calls))
	}
	for i, call := range calls {
		if len(call.Tools) != 1 || call.Tools[0].GetName() != structuredOutputToolName {
			t.Fatalf("call %d tools = %#v; correction exposed something besides the terminal tool", i, call.Tools)
		}
		if call.ToolChoice == nil || *call.ToolChoice != fantasy.SpecificToolChoice(structuredOutputToolName) {
			t.Fatalf("call %d did not force the terminal tool: %v", i, call.ToolChoice)
		}
	}
	if orch.PromptTokens != 40 || orch.CompletionTokens != 10 {
		t.Fatalf("terminal usage not governed: prompt=%d completion=%d", orch.PromptTokens, orch.CompletionTokens)
	}
}

func TestTerminalStructuredOutput_PermanentInvalidAndMissingAreBounded(t *testing.T) {
	t.Run("invalid", func(t *testing.T) {
		model := &terminalTestModel{provider: "anthropic", model: "m"}
		for range StructuredOutputCorrectionAttempts + 1 {
			model.responses = append(model.responses, terminalToolResponse(`{"answer":"wrong"}`))
		}
		eng, orch := testTerminalEngine(model)
		_, err := eng.generateTerminalStructuredOutput(context.Background(), model, "", nil, json.RawMessage(terminalTestSchema), 1000, orch)
		if !errors.Is(err, ErrStructuredOutputInvalid) || !errors.Is(err, ErrStructuredOutputFormat) {
			t.Fatalf("error = %v, want invalid/format classification", err)
		}
		if got := len(model.calls()); got != StructuredOutputCorrectionAttempts+1 {
			t.Fatalf("calls = %d", got)
		}
	})

	t.Run("missing", func(t *testing.T) {
		model := &terminalTestModel{provider: "anthropic", model: "m"}
		for range StructuredOutputCorrectionAttempts + 1 {
			model.responses = append(model.responses, &fantasy.Response{FinishReason: fantasy.FinishReasonStop})
		}
		eng, orch := testTerminalEngine(model)
		_, err := eng.generateTerminalStructuredOutput(context.Background(), model, "", nil, json.RawMessage(terminalTestSchema), 1000, orch)
		if !errors.Is(err, ErrStructuredOutputMissing) {
			t.Fatalf("error = %v, want missing classification", err)
		}
		if got := len(model.calls()); got != StructuredOutputCorrectionAttempts+1 {
			t.Fatalf("calls = %d", got)
		}
	})
}

func TestStructuredCorrectionDetailIsByteBounded(t *testing.T) {
	got := clampCorrectionDetail(strings.Repeat("x", maxStructuredCorrectionDetailBytes+100))
	if len(got) != maxStructuredCorrectionDetailBytes || !strings.HasSuffix(got, "...") {
		t.Fatalf("clamped detail length/suffix = %d/%q", len(got), got[len(got)-3:])
	}
}

func TestTerminalStructuredOutput_RefusalFailsImmediately(t *testing.T) {
	model := &terminalTestModel{
		provider: "openrouter",
		model:    "refusing",
		responses: []*fantasy.Response{{
			FinishReason: fantasy.FinishReasonContentFilter,
		}},
	}
	eng, orch := testTerminalEngine(model)
	_, err := eng.generateTerminalStructuredOutput(context.Background(), model, "", nil, json.RawMessage(terminalTestSchema), 1000, orch)
	if !errors.Is(err, ErrStructuredOutputRefusal) {
		t.Fatalf("error = %v, want refusal", err)
	}
	if got := len(model.calls()); got != 1 {
		t.Fatalf("refusal calls = %d, want 1", got)
	}
}

func TestTerminalStructuredOutput_OpenRouterUsesRawStrictNativeSchema(t *testing.T) {
	raw := json.RawMessage(`{
      "type":"object",
      "properties":{"value":{"oneOf":[{"type":"string"},{"type":"integer"}]}},
      "required":["value"],
      "additionalProperties":false
    }`)
	model := &terminalTestModel{
		provider: openrouter.Name,
		model:    "openai/gpt-test",
		responses: []*fantasy.Response{{
			Content:      fantasy.ResponseContent{fantasy.TextContent{Text: `{"value":"ok"}`}},
			FinishReason: fantasy.FinishReasonStop,
		}},
	}
	eng, orch := testTerminalEngine(model)
	if _, err := eng.generateTerminalStructuredOutput(context.Background(), model, "", nil, raw, 1000, orch); err != nil {
		t.Fatal(err)
	}
	call := model.calls()[0]
	if len(call.Tools) != 0 || call.ToolChoice != nil {
		t.Fatalf("native strict call exposed tools: tools=%v choice=%v", call.Tools, call.ToolChoice)
	}
	opts, ok := call.ProviderOptions[openrouter.Name].(*openrouter.ProviderOptions)
	if !ok || opts == nil {
		t.Fatalf("OpenRouter options = %#v", call.ProviderOptions)
	}
	format, ok := opts.ExtraBody["response_format"].(map[string]any)
	if !ok || format["type"] != "json_schema" {
		t.Fatalf("response_format = %#v", opts.ExtraBody["response_format"])
	}
	js := format["json_schema"].(map[string]any)
	if js["strict"] != true {
		t.Fatalf("strict = %#v", js["strict"])
	}
	encoded, _ := json.Marshal(js["schema"])
	var want any
	_ = json.Unmarshal(raw, &want)
	wantEncoded, _ := json.Marshal(want)
	if string(encoded) != string(wantEncoded) {
		t.Fatalf("native schema was lossy:\n got %s\nwant %s", encoded, wantEncoded)
	}
	if opts.Provider == nil || opts.Provider.RequireParameters == nil || !*opts.Provider.RequireParameters {
		t.Fatal("native strict request did not require upstream parameter support")
	}
}

type captureOpenRouterTransport struct {
	body []byte
}

func (t *captureOpenRouterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	t.body = body
	response := `{
      "id":"chatcmpl-test","object":"chat.completion","created":1,
      "model":"openai/gpt-test",
      "choices":[{"index":0,"message":{"role":"assistant","content":"{\"answer\":42}"},"finish_reason":"stop"}],
      "usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}
    }`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(response)),
		Request:    req,
	}, nil
}

func TestTerminalStructuredOutput_OpenRouterWireRequestIsStrict(t *testing.T) {
	transport := &captureOpenRouterTransport{}
	provider, err := openrouter.New(
		openrouter.WithAPIKey("sk-or-test"),
		openrouter.WithHTTPClient(&http.Client{Transport: transport}),
	)
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.LanguageModel(t.Context(), "openai/gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	eng, orch := testTerminalEngine(model)
	out, err := eng.generateTerminalStructuredOutput(
		t.Context(), model, "system", nil, json.RawMessage(terminalTestSchema), 1000, orch,
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"answer":42}` {
		t.Fatalf("output = %s", out)
	}

	var request map[string]any
	if err := json.Unmarshal(transport.body, &request); err != nil {
		t.Fatalf("decode request %q: %v", transport.body, err)
	}
	format, ok := request["response_format"].(map[string]any)
	if !ok || format["type"] != "json_schema" {
		t.Fatalf("wire response_format = %#v", request["response_format"])
	}
	js, ok := format["json_schema"].(map[string]any)
	if !ok || js["strict"] != true {
		t.Fatalf("wire json_schema = %#v", format["json_schema"])
	}
	routing, ok := request["provider"].(map[string]any)
	if !ok || routing["require_parameters"] != true {
		t.Fatalf("wire provider routing = %#v", request["provider"])
	}
	if _, exists := request["tools"]; exists {
		t.Fatalf("native wire request unexpectedly exposed tools: %#v", request["tools"])
	}
}

func TestRunStructuredOutputUsesModelAfterProviderFallback(t *testing.T) {
	t.Setenv("FLEET_RETRY_MAX_ATTEMPTS", "0")
	primary := &terminalTestModel{provider: "primary", model: "primary-model"}
	primary.streamFunc = func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		return nil, &fantasy.ProviderError{StatusCode: http.StatusServiceUnavailable, Message: "capacity"}
	}
	fallback := &terminalTestModel{
		provider: "fallback",
		model:    "fallback-model",
		responses: []*fantasy.Response{
			terminalToolResponse(`{"answer":7}`),
		},
	}
	fallback.streamFunc = streamStop()

	res, err := Run(context.Background(), ModeInteractive, RunConfig{
		OutputSchema: json.RawMessage(terminalTestSchema),
	}, Deps{
		Input:         stubInput{system: "sys", user: "work", label: "structured"},
		Observer:      &captureObserver{},
		Policy:        NewInteractivePolicy(0, 0, nil, nil),
		Executor:      &stubExecutor{},
		Model:         primary,
		FallbackModel: fallback,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.SwappedToFallback || res.ModelSlug != "fallback-model" || string(res.OutputJSON) != `{"answer":7}` {
		t.Fatalf("result = %+v", res)
	}
	if len(primary.calls()) != 0 {
		t.Fatal("terminal phase used failed primary provider")
	}
	if len(fallback.calls()) != 1 || len(fallback.calls()[0].Tools) != 1 {
		t.Fatal("terminal phase did not select fallback provider's forced-tool path")
	}
}
