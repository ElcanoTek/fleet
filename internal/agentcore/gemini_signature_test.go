package agentcore

// Gemini thought signatures: a Gemini model's tool call carries one, and
// Gemini wants it back with the call on the turn's later requests. Replay keeps
// it for a Gemini model and for no other.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openrouter"
)

// openRouterModel reports itself as an OpenRouter model: the provider that
// sends a Gemini signature back, to its google/ models.
type openRouterModel struct{ namedMockModel }

func (m *openRouterModel) Provider() string { return openrouter.Name }

func signedCall(callID, sig string) fantasy.Message {
	return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.ReasoningPart{ProviderOptions: fantasy.ProviderOptions{google.Name: &google.ReasoningMetadata{Signature: sig, ToolID: callID}}},
		fantasy.ToolCallPart{ToolCallID: callID, ToolName: "save_record", Input: `{}`},
	}}
}

func reasoningParts(messages []fantasy.Message) []fantasy.ReasoningPart {
	var out []fantasy.ReasoningPart
	for _, msg := range messages {
		for _, part := range msg.Content {
			if r, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part); ok {
				out = append(out, r)
			}
		}
	}
	return out
}

// Replay keeps a Gemini signature, with any thought text dropped, and still
// drops every other reasoning part; a message left with nothing but a
// signature goes too.
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

// A Gemini model signs a call, runs it, then fails. The resume from that step
// goes to a non-Gemini fallback, which is shown no signature (OpenRouter would
// send it an empty reasoning detail), signs nothing, runs a call of its own
// and fails too. The next fallback is Gemini again, and it gets the first
// call's signature back in front of that call.
func TestStreamRecoveryShowsGeminiSignaturesOnlyToGemini(t *testing.T) {
	tool := fantasy.NewAgentTool("save_record", "save a record",
		func(context.Context, panicTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse(`{"saved":true}`), nil
		})
	failure := func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: &fantasy.ProviderError{
			Title: "stream error", Message: "Provider returned error",
			ResponseBody: []byte(`{"error":{"code":503,"message":"Provider returned error"}}`),
		}})
	}
	callThenFail := func(model *openRouterModel, call func(yield func(fantasy.StreamPart) bool), seen *[][]fantasy.Message) {
		var n atomic.Int32
		model.streamFunc = func(_ context.Context, c fantasy.Call) (fantasy.StreamResponse, error) {
			*seen = append(*seen, c.Prompt)
			if n.Add(1) == 1 {
				return call, nil
			}
			return failure, nil
		}
	}

	primary := &openRouterModel{namedMockModel{name: "google/gemini-primary"}}
	var primarySaw [][]fantasy.Message
	callThenFail(primary, func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: "thinking"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: "thinking", Delta: "thought text"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: "thinking", ProviderMetadata: fantasy.ProviderMetadata{
			google.Name: &google.ReasoningMetadata{Signature: "sig-1", ToolID: "gemini-call"},
		}})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "gemini-call", ToolCallName: "save_record", ToolCallInput: `{}`})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls, Usage: fantasy.Usage{InputTokens: 20, OutputTokens: 10}})
	}, &primarySaw)

	other := &openRouterModel{namedMockModel{name: "deepseek/deepseek-fallback"}}
	var otherSaw [][]fantasy.Message
	callThenFail(other, func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "other-call", ToolCallName: "save_record", ToolCallInput: `{}`})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls, Usage: fantasy.Usage{InputTokens: 20, OutputTokens: 10}})
	}, &otherSaw)

	gemini := &openRouterModel{namedMockModel{name: "google/gemini-fallback"}}
	var geminiSaw [][]fantasy.Message
	gemini.streamFunc = func(_ context.Context, c fantasy.Call) (fantasy.StreamResponse, error) {
		geminiSaw = append(geminiSaw, c.Prompt)
		return streamTextThenFinish("saved twice"), nil
	}

	e := newMockEngine(t, primary)
	e.fallbackModel = other
	e.fallbackModels = []fantasy.LanguageModel{other, gemini}
	e.maxIterations = 10
	build := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"), fantasy.WithTools(tool))
	}
	orch := newOrchestrationState(e.logSession, 50)
	out, err := e.streamRoundWithResilience(context.Background(), orch, newStreamSink(nil), 1000,
		[]fantasy.Message{fantasy.NewUserMessage("save twice")}, build(primary), primary, false, build)
	if err != nil {
		t.Fatal(err)
	}
	if out.activeModel != gemini || len(primarySaw) != 2 || len(otherSaw) != 2 || len(geminiSaw) != 1 {
		t.Fatalf("active=%s calls: primary=%d other=%d gemini=%d; want the run to end on the Gemini fallback after 2+2+1 calls",
			out.activeModel.Model(), len(primarySaw), len(otherSaw), len(geminiSaw))
	}
	// The primary's own next step gets its signature, as fantasy sends it.
	if got := reasoningParts(primarySaw[1]); len(got) != 1 {
		t.Errorf("primary's second step: %d reasoning parts, want its own signed thought", len(got))
	}
	for i, prompt := range otherSaw {
		if got := reasoningParts(prompt); len(got) != 0 {
			t.Errorf("non-Gemini fallback call %d was shown %d reasoning parts: %+v", i+1, len(got), got)
		}
	}
	var signed []fantasy.MessagePart
	for _, msg := range geminiSaw[0] {
		for _, part := range msg.Content {
			if c, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok && c.ToolCallID == "gemini-call" {
				signed = msg.Content
			}
		}
	}
	if len(signed) != 2 {
		t.Fatalf("Gemini fallback's copy of the signed call = %+v, want the signature then the call", signed)
	}
	r, _ := fantasy.AsMessagePart[fantasy.ReasoningPart](signed[0])
	if md := google.GetReasoningMetadata(r.Options()); r.Text != "" || md == nil || md.Signature != "sig-1" || md.ToolID != "gemini-call" {
		t.Errorf("Gemini fallback got %+v in front of the call, want signature sig-1 and no thought text", signed[0])
	}
	if got := reasoningParts(geminiSaw[0]); len(got) != 1 {
		t.Errorf("Gemini fallback got %d reasoning parts, want only the one signature", len(got))
	}
}

// The finalize hook, the compaction summary and the terminal structured
// output each ask with a user message of their own, which starts a new Gemini
// turn, and may call a model that is not Gemini: none of them is given a
// signature.
func TestNewTurnRequestsGetNoGeminiSignatures(t *testing.T) {
	carried := []fantasy.Message{
		fantasy.NewUserMessage("save it"),
		signedCall("c1", "sig-1"),
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "c1"}}},
	}

	in := finalizeInputFor(ModeInteractive, "", carried, 0, nil, nil, "", nil, panicAttribution{}, nil, nil, 0, nil)
	if got := reasoningParts(in.Messages); len(got) != 0 || len(in.Messages) != 3 {
		t.Errorf("finalize input: %d messages, %d reasoning parts; want the 3 messages and no signature", len(in.Messages), len(got))
	}

	e := newMockEngine(t, &openRouterModel{namedMockModel{name: "google/gemini-x"}})
	if got := reasoningParts(e.compactionSummarizeInput(carried).Droppable); len(got) != 0 {
		t.Errorf("compaction summary input kept %d reasoning parts", len(got))
	}

	model := &terminalTestModel{provider: openrouter.Name, model: "google/gemini-x", responses: []*fantasy.Response{{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: `{"answer":1}`}},
		FinishReason: fantasy.FinishReasonStop,
	}}}
	eng, orch := testTerminalEngine(model)
	if _, err := eng.generateTerminalStructuredOutput(context.Background(), model, "system", carried,
		json.RawMessage(terminalTestSchema), 1000, orch); err != nil {
		t.Fatalf("generateTerminalStructuredOutput: %v", err)
	}
	calls := model.calls()
	if len(calls) != 1 {
		t.Fatalf("terminal calls = %d, want 1", len(calls))
	}
	if got := reasoningParts(calls[0].Prompt); len(got) != 0 {
		t.Errorf("terminal structured output kept %d reasoning parts", len(got))
	}
	// The caller's slice is not edited.
	if got := reasoningParts(carried); len(got) != 1 {
		t.Errorf("the carried messages lost their signature: %d reasoning parts", len(got))
	}
}
