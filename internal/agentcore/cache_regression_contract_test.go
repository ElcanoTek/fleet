package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
)

// capturedWireRequest records a raw OpenRouter /chat/completions HTTP request
// body as serialized on the wire by fantasy + openai-go + openRouterCacheControlTransport.
type capturedWireRequest struct {
	RawBody  []byte
	Parsed   map[string]any
	Model    string
	Messages []map[string]any
	Tools    []any
	Provider map[string]any
}

func parseCapturedWireRequest(t *testing.T, body []byte) capturedWireRequest {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal wire request JSON: %v\nbody=%s", err, string(body))
	}
	model, _ := parsed["model"].(string)
	rawMsgs, _ := parsed["messages"].([]any)
	msgs := make([]map[string]any, 0, len(rawMsgs))
	for _, rm := range rawMsgs {
		if m, ok := rm.(map[string]any); ok {
			msgs = append(msgs, m)
		}
	}
	tools, _ := parsed["tools"].([]any)
	prov, _ := parsed["provider"].(map[string]any)
	return capturedWireRequest{
		RawBody:  append([]byte(nil), body...),
		Parsed:   parsed,
		Model:    model,
		Messages: msgs,
		Tools:    tools,
		Provider: prov,
	}
}

// canonicalizeWireMessageWithoutCacheControl returns the canonical JSON of a wire
// message with cache_control stripped and single-text-part arrays collapsed back
// to plain text so we can assert 100% byte-for-byte prefix invariance across
// consecutive steps and turns as breakpoints advance.
func canonicalizeWireMessageWithoutCacheControl(t *testing.T, msg map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	var clone map[string]any
	if err := json.Unmarshal(raw, &clone); err != nil {
		t.Fatalf("unmarshal message clone: %v", err)
	}
	delete(clone, "cache_control")
	if tcs, ok := clone["tool_calls"].([]any); ok {
		for _, tc := range tcs {
			if tcm, ok := tc.(map[string]any); ok {
				delete(tcm, "cache_control")
			}
		}
	}
	if parts, ok := clone["content"].([]any); ok {
		cleanedParts := make([]any, 0, len(parts))
		for _, p := range parts {
			if pm, ok := p.(map[string]any); ok {
				delete(pm, "cache_control")
				cleanedParts = append(cleanedParts, pm)
			} else {
				cleanedParts = append(cleanedParts, p)
			}
		}
		if len(cleanedParts) == 1 {
			if pm, ok := cleanedParts[0].(map[string]any); ok && pm["type"] == "text" {
				if txt, ok := pm["text"].(string); ok && len(pm) == 2 {
					clone["content"] = txt
				} else {
					clone["content"] = cleanedParts
				}
			} else {
				clone["content"] = cleanedParts
			}
		} else {
			clone["content"] = cleanedParts
		}
	}
	canon, err := json.Marshal(clone)
	if err != nil {
		t.Fatalf("marshal canonical message: %v", err)
	}
	return string(canon)
}

// messageHasWireCacheControl reports whether a wire message has cache_control
// on any content block or at top level, and fails the test if a user or
// assistant message ever has a top-level cache_control alongside non-empty
// string content without tool_calls (which OpenRouter silently drops).
func messageHasWireCacheControl(t *testing.T, msgIdx int, msg map[string]any) bool {
	t.Helper()
	role, _ := msg["role"].(string)
	_, hasTopLevel := msg["cache_control"]
	contentStr, contentIsString := msg["content"].(string)
	toolCalls, _ := msg["tool_calls"].([]any)
	if (role == "user" || role == "assistant") && hasTopLevel && contentIsString && contentStr != "" && len(toolCalls) == 0 {
		t.Fatalf("message[%d] (role=%q) has top-level cache_control on non-empty string content — OpenRouter silently drops this! msg=%v", msgIdx, role, msg)
	}
	if hasTopLevel {
		return true
	}
	for tcIdx, tc := range toolCalls {
		if tcm, ok := tc.(map[string]any); ok {
			if cc, hasCC := tcm["cache_control"]; hasCC {
				ccMap, ok := cc.(map[string]any)
				if !ok || ccMap["type"] != "ephemeral" {
					t.Fatalf("message[%d].tool_calls[%d] has invalid cache_control=%v", msgIdx, tcIdx, cc)
				}
				return true
			}
		}
	}
	parts, ok := msg["content"].([]any)
	if !ok {
		return false
	}
	found := false
	for partIdx, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if cc, hasCC := pm["cache_control"]; hasCC {
			ccMap, ok := cc.(map[string]any)
			if !ok || ccMap["type"] != "ephemeral" {
				t.Fatalf("message[%d].content[%d] has invalid cache_control=%v", msgIdx, partIdx, cc)
			}
			found = true
		}
	}
	return found
}

// buildFakeOpenRouterSSEResponse constructs a minimal OpenAI-compatible SSE
// stream for fantasy's openrouter provider. When toolName != "", it emits a
// reasoning chunk followed by a tool_calls chunk; otherwise it emits a text stop.
func buildFakeOpenRouterSSEResponse(model, toolCallID, toolName, toolArgs, text string) string {
	var b strings.Builder
	writeChunk := func(payload map[string]any) {
		raw, _ := json.Marshal(payload)
		b.WriteString("data: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	if toolName != "" {
		writeChunk(map[string]any{
			"id":      "chatcmpl-" + toolCallID,
			"object":  "chat.completion.chunk",
			"created": 1700000000,
			"model":   model,
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{
						"role":      "assistant",
						"reasoning": "Thinking through step for " + toolCallID,
					},
				},
			},
		})
		writeChunk(map[string]any{
			"id":      "chatcmpl-" + toolCallID,
			"object":  "chat.completion.chunk",
			"created": 1700000000,
			"model":   model,
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"index": 0,
								"id":    toolCallID,
								"type":  "function",
								"function": map[string]any{
									"name":      toolName,
									"arguments": toolArgs,
								},
							},
						},
					},
					"finish_reason": "tool_calls",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     2048,
				"completion_tokens": 64,
				"total_tokens":      2112,
				"prompt_tokens_details": map[string]any{
					"cached_tokens": 1536,
				},
			},
		})
	} else {
		writeChunk(map[string]any{
			"id":      "chatcmpl-final",
			"object":  "chat.completion.chunk",
			"created": 1700000000,
			"model":   model,
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{
						"role":    "assistant",
						"content": text,
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     4096,
				"completion_tokens": 32,
				"total_tokens":      4128,
				"prompt_tokens_details": map[string]any{
					"cached_tokens": 3800,
				},
			},
		})
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// TestWireCacheContract_EndToEndMultiTurnAndMultiStep exercises the full
// OpenRouter wire pipeline (PrepareStep -> fantasy OpenRouter hook -> openai-go
// JSON serializer -> openRouterCacheControlTransport -> HTTP wire) across:
//   - A 12-tool-round Turn 1 (>24 content blocks, exceeding Anthropic's 20-block
//     lookback window) with assistant reasoning/thinking parts
//   - Turn 2 after replayHistory (which strips assistant reasoning parts)
//   - Terminal structured output generation
//
// It verifies for Anthropic, Gemini, and OpenAI models:
//  1. User and assistant cache_control markers are NEVER serialized at top-level
//     on string content; they are always inside content[i].cache_control.
//  2. On long multi-step turns (>20 blocks), Anthropic places its 4th breakpoint
//     on the runtime_today user message, and Turn 2's wire prefix up to
//     runtime_today is 100% byte-for-byte identical to Turn 1's even after
//     replayHistory strips Turn 1's thinking blocks.
//  3. Across all steps and turns, the serialized tools array and message prefix
//     (modulo moving cache_control markers) are 100% byte-for-byte identical.
//  4. Sticky upstream provider routing (provider.order + require_parameters) is
//     present on every wire request.
type wireContractTestFamily string

const (
	familyAnthropic wireContractTestFamily = "anthropic"
	familyGoogle    wireContractTestFamily = "google"
	familyImplicit  wireContractTestFamily = "implicit"
)

type wireContractModelSpec struct {
	slug                string
	family              wireContractTestFamily
	expectNoProviderPin bool
	allowFallbacks      bool
	expectedOrder       []string
	expectedOnly        []string
}

func TestWireCacheContract_EndToEndMultiTurnAndMultiStep(t *testing.T) {
	models := []wireContractModelSpec{
		{
			slug:           "anthropic/claude-opus-5.5",
			family:         familyAnthropic,
			allowFallbacks: true,
			expectedOrder:  []string{"Anthropic"},
			expectedOnly:   []string{"Anthropic", "Claude Platform on AWS", "Amazon Bedrock", "Azure", "Google"},
		},
		{
			slug:           "anthropic/claude-sonnet-5.5",
			family:         familyAnthropic,
			allowFallbacks: true,
			expectedOrder:  []string{"Anthropic"},
			expectedOnly:   nil,
		},
		{
			slug:           "anthropic/claude-haiku-5.5",
			family:         familyAnthropic,
			allowFallbacks: true,
			expectedOrder:  []string{"Anthropic"},
			expectedOnly:   []string{"Anthropic", "Claude Platform on AWS", "Amazon Bedrock", "Azure", "Google"},
		},
		{
			slug:           "~anthropic/claude-sonnet-latest",
			family:         familyAnthropic,
			allowFallbacks: true,
			expectedOrder:  []string{"Anthropic"},
			expectedOnly:   nil,
		},
		{
			slug:           "google/gemini-4-argon",
			family:         familyGoogle,
			allowFallbacks: false,
			expectedOrder:  nil,
			expectedOnly:   []string{"Google"},
		},
		{
			slug:           "google/gemini-3.8-flash",
			family:         familyGoogle,
			allowFallbacks: false,
			expectedOrder:  nil,
			expectedOnly:   []string{"Google"},
		},
		{
			slug:           "openai/gpt-6-luna-pro",
			family:         familyImplicit,
			allowFallbacks: true,
			expectedOrder:  []string{"OpenAI"},
			expectedOnly:   []string{"OpenAI", "Azure", "Amazon Bedrock"},
		},
		{
			slug:           "openai/gpt-5.4",
			family:         familyImplicit,
			allowFallbacks: true,
			expectedOrder:  []string{"OpenAI"},
			expectedOnly:   nil,
		},
		{
			slug:           "deepseek/deepseek-v4.1-flash",
			family:         familyImplicit,
			allowFallbacks: true,
			expectedOrder:  []string{"DeepSeek"},
			expectedOnly:   nil,
		},
		{
			slug:           "moonshotai/kimi-k3",
			family:         familyImplicit,
			allowFallbacks: true,
			expectedOrder:  []string{"Moonshot AI"},
			expectedOnly:   nil,
		},
		{
			slug:           "z-ai/glm-5.3",
			family:         familyImplicit,
			allowFallbacks: true,
			expectedOrder:  []string{"Z.AI"},
			expectedOnly:   nil,
		},
		{
			slug:                "x-ai/grok-4.7",
			family:              familyImplicit,
			expectNoProviderPin: true,
		},
		{
			slug:           "qwen/qwen3.8-27b",
			family:         familyImplicit,
			allowFallbacks: true,
			expectedOrder:  nil,
			expectedOnly:   nil,
		},
		{
			slug:                "mistralai/mistral-large-4-0",
			family:              familyImplicit,
			expectNoProviderPin: true,
		},
	}

	for _, tc := range models {
		t.Run(tc.slug, func(t *testing.T) {
			runWireCacheContractForModel(t, tc)
		})
	}
}

func runWireCacheContractForModel(t *testing.T, tc wireContractModelSpec) {
	t.Helper()
	var (
		mu       sync.Mutex
		requests []capturedWireRequest
		stepSeq  int
	)
	const totalToolRounds = 12

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req := parseCapturedWireRequest(t, body)
		mu.Lock()
		idx := stepSeq
		stepSeq++
		requests = append(requests, req)
		mu.Unlock()

		isStream, _ := req.Parsed["stream"].(bool)
		if !isStream {
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{
				"id":      "chatcmpl-structured",
				"object":  "chat.completion",
				"created": 1700000000,
				"model":   tc.slug,
				"choices": []any{
					map[string]any{
						"index": 0,
						"message": map[string]any{
							"role":    "assistant",
							"content": `{"status":"ok","summary":"cached"}`,
						},
						"finish_reason": "stop",
					},
				},
				"usage": map[string]any{
					"prompt_tokens":     4200,
					"completion_tokens": 20,
					"total_tokens":      4220,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		if idx < totalToolRounds {
			callID := fmt.Sprintf("call_%02d", idx+1)
			args := fmt.Sprintf(`{"key":"step_%02d"}`, idx+1)
			_, _ = io.WriteString(w, buildFakeOpenRouterSSEResponse(tc.slug, callID, "inspect_metric", args, ""))
			return
		}
		_, _ = io.WriteString(w, buildFakeOpenRouterSSEResponse(tc.slug, "", "", "", fmt.Sprintf("Done at request %d", idx)))
	}))
	defer srv.Close()

	t.Setenv("OPENROUTER_BASE_URL", srv.URL)

	provider, err := newOpenRouterProvider("test-openrouter-key")
	if err != nil {
		t.Fatalf("newOpenRouterProvider: %v", err)
	}
	model, err := provider.LanguageModel(context.Background(), tc.slug)
	if err != nil {
		t.Fatalf("LanguageModel(%s): %v", tc.slug, err)
	}

	type inspectInput struct {
		Key string `json:"key" description:"Metric key to inspect"`
	}
	inspectTool := fantasy.NewAgentTool(
		"inspect_metric",
		"Inspect a named production metric and return its series.",
		func(_ context.Context, in inspectInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse(fmt.Sprintf("metric=%s values=%s", in.Key, strings.Repeat("1024,", 220))), nil
		},
	)
	tools := []fantasy.AgentTool{inspectTool}

	systemPrompt := "You are Victoria, the Elcano production assistant.\n" + strings.Repeat(" Governed invariant line for system prompt stability.", 90)
	eng := &engine{maxIterations: 30}
	eng.setModelContextPrefix(systemPrompt, tools)
	ag := fantasy.NewAgent(model,
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithTools(tools...),
	)

	turn1History := appendRuntimeDateMessage(
		[]fantasy.Message{fantasy.NewUserMessage("Run the 12-step metric inspection and summarize.")},
		time.Date(2026, time.November, 3, 20, 0, 0, 0, time.UTC),
	)

	rs1 := newRoundState(eng, newOrchestrationState(nil), 4096)
	res1, err := rs1.stream(context.Background(), ag, model, turn1History)
	if err != nil {
		t.Fatalf("Turn 1 stream failed: %v", err)
	}
	if res1 == nil || len(res1.Steps) != totalToolRounds+1 {
		t.Fatalf("Turn 1 steps = %d, want %d", len(res1.Steps), totalToolRounds+1)
	}

	turn2History := append([]fantasy.Message(nil), turn1History...)
	turn2History = append(turn2History, eng.roundCarry(res1)...)
	turn2History = append(turn2History, fantasy.NewUserMessage("Now compare step_01 with step_12."))

	rs2 := newRoundState(eng, newOrchestrationState(nil), 4096)
	if _, err := rs2.stream(context.Background(), ag, model, turn2History); err != nil {
		t.Fatalf("Turn 2 stream failed: %v", err)
	}

	schemaRaw := json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"},"summary":{"type":"string"}},"required":["status","summary"],"additionalProperties":false}`)
	if _, err := eng.generateTerminalStructuredOutput(
		context.Background(),
		model,
		systemPrompt,
		turn2History,
		schemaRaw,
		2048,
		newOrchestrationState(nil),
	); err != nil {
		t.Fatalf("generateTerminalStructuredOutput failed: %v", err)
	}

	mu.Lock()
	captured := append([]capturedWireRequest(nil), requests...)
	mu.Unlock()

	if len(captured) != totalToolRounds+3 {
		t.Fatalf("captured %d wire requests, want %d", len(captured), totalToolRounds+3)
	}

	assertWireProviderRouting(t, tc, captured)
	assertWireCacheBreakpoints(t, tc, captured, totalToolRounds)
	assertWirePrefixInvariance(t, captured, totalToolRounds)
}

func assertWireProviderRouting(t *testing.T, tc wireContractModelSpec, captured []capturedWireRequest) {
	t.Helper()
	for reqIdx, req := range captured {
		if tc.expectNoProviderPin {
			if req.Provider != nil {
				// Terminal structured output sets provider.require_parameters=true even
				// when no upstream family pin is present.
				if len(req.Provider) == 1 && req.Provider["require_parameters"] == true {
					continue
				}
				t.Fatalf("request[%d] unexpectedly has provider pin %v for unpinned slug %s", reqIdx, req.Provider, tc.slug)
			}
			continue
		}
		if req.Provider == nil {
			t.Fatalf("request[%d] missing top-level provider routing object", reqIdx)
		}
		if req.Provider["allow_fallbacks"] != tc.allowFallbacks {
			t.Fatalf("request[%d] provider.allow_fallbacks = %v, want %v", reqIdx, req.Provider["allow_fallbacks"], tc.allowFallbacks)
		}
		gotOrder := stringSliceFromAny(req.Provider["order"])
		if strings.Join(gotOrder, ",") != strings.Join(tc.expectedOrder, ",") {
			t.Fatalf("request[%d] provider.order = %v, want %v", reqIdx, gotOrder, tc.expectedOrder)
		}
		gotOnly := stringSliceFromAny(req.Provider["only"])
		if strings.Join(gotOnly, ",") != strings.Join(tc.expectedOnly, ",") {
			t.Fatalf("request[%d] provider.only = %v, want %v", reqIdx, gotOnly, tc.expectedOnly)
		}
	}
}

func stringSliceFromAny(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func assertWireCacheBreakpoints(t *testing.T, tc wireContractModelSpec, captured []capturedWireRequest, totalToolRounds int) {
	t.Helper()
	for reqIdx, req := range captured {
		msgBreakpointIndices := []int{}
		for msgIdx, msg := range req.Messages {
			if messageHasWireCacheControl(t, msgIdx, msg) {
				msgBreakpointIndices = append(msgBreakpointIndices, msgIdx)
			}
		}
		switch tc.family {
		case familyAnthropic:
			if len(msgBreakpointIndices) == 0 || len(msgBreakpointIndices) > 4 {
				t.Fatalf("request[%d] on %s has %d message breakpoints (%v), want 1..4", reqIdx, tc.slug, len(msgBreakpointIndices), msgBreakpointIndices)
			}
		case familyGoogle:
			if len(msgBreakpointIndices) > 1 {
				t.Fatalf("request[%d] on %s has %d message breakpoints (%v), want <=1", reqIdx, tc.slug, len(msgBreakpointIndices), msgBreakpointIndices)
			}
		case familyImplicit:
			if len(msgBreakpointIndices) != 0 {
				t.Fatalf("request[%d] on implicit-cache model %s has explicit message breakpoints %v, want none", reqIdx, tc.slug, msgBreakpointIndices)
			}
		}
	}

	turn1Step0 := captured[0]
	turn1FinalStep := captured[totalToolRounds]
	structuredReq := captured[totalToolRounds+2]

	switch tc.family {
	case familyAnthropic:
		if !messageHasWireCacheControl(t, 0, turn1FinalStep.Messages[0]) || !messageHasWireCacheControl(t, 2, turn1FinalStep.Messages[2]) {
			t.Fatalf("Turn 1 Step 12 missing system or runtime_today 4th cache_control breakpoint")
		}
		if !messageHasWireCacheControl(t, 0, structuredReq.Messages[0]) || !messageHasWireCacheControl(t, 2, structuredReq.Messages[2]) {
			t.Fatalf("structured output request missing system or runtime_today cache_control breakpoint")
		}
	case familyGoogle:
		if !messageHasWireCacheControl(t, 0, turn1Step0.Messages[0]) {
			t.Fatalf("Gemini Step 0 messages[0] (system) missing cache_control")
		}
		for i := 1; i < len(turn1Step0.Messages); i++ {
			if messageHasWireCacheControl(t, i, turn1Step0.Messages[i]) {
				t.Fatalf("Gemini Step 0 messages[%d] unexpectedly has explicit cache_control", i)
			}
		}
		for i := 0; i < len(turn1FinalStep.Messages); i++ {
			if messageHasWireCacheControl(t, i, turn1FinalStep.Messages[i]) {
				t.Fatalf("Gemini Step 12 messages[%d] unexpectedly kept explicit cache_control after tail outgrew prefix", i)
			}
		}
	case familyImplicit:
	}
}

func assertWirePrefixInvariance(t *testing.T, captured []capturedWireRequest, totalToolRounds int) {
	t.Helper()
	turn1Step0 := captured[0]
	turn2Step0 := captured[totalToolRounds+1]
	step0ToolsJSON, _ := json.Marshal(turn1Step0.Tools)

	for stepIdx := 1; stepIdx <= totalToolRounds; stepIdx++ {
		req := captured[stepIdx]
		reqToolsJSON, _ := json.Marshal(req.Tools)
		if string(reqToolsJSON) != string(step0ToolsJSON) {
			t.Fatalf("Turn 1 Step %d tools JSON drifted from Step 0", stepIdx)
		}
		prev := captured[stepIdx-1]
		for mIdx := 0; mIdx < len(prev.Messages); mIdx++ {
			wantCanon := canonicalizeWireMessageWithoutCacheControl(t, prev.Messages[mIdx])
			gotCanon := canonicalizeWireMessageWithoutCacheControl(t, req.Messages[mIdx])
			if gotCanon != wantCanon {
				t.Fatalf("Turn 1 Step %d message[%d] drifted from Step %d", stepIdx, mIdx, stepIdx-1)
			}
		}
	}

	turn2ToolsJSON, _ := json.Marshal(turn2Step0.Tools)
	if string(turn2ToolsJSON) != string(step0ToolsJSON) {
		t.Fatalf("Turn 2 tools JSON drifted from Turn 1")
	}
	for mIdx := 0; mIdx < len(turn1Step0.Messages); mIdx++ {
		wantCanon := canonicalizeWireMessageWithoutCacheControl(t, turn1Step0.Messages[mIdx])
		gotCanon := canonicalizeWireMessageWithoutCacheControl(t, turn2Step0.Messages[mIdx])
		if gotCanon != wantCanon {
			t.Fatalf("Turn 2 message[%d] prefix drifted from Turn 1", mIdx)
		}
	}
}

// TestAllLLMCallSitesWirePromptCachingAndRouting_ASTGuard scans all production
// Go files in internal/agentcore, internal/agent, and internal/scheduledrun to
// ensure no new fantasy.NewAgent, .Stream(, or .Generate( call site can be
// added without wiring PromptCachingStep/PromptCachingStep and provider routing.
func TestAllLLMCallSitesWirePromptCachingAndRouting_ASTGuard(t *testing.T) {
	// Every production file that invokes fantasy.NewAgent, model.Stream, or
	// model.Generate must appear in this allowlist AND reference the caching +
	// routing helpers in the same file.
	allowedCallSiteFiles := map[string]struct {
		requireCacheSymbol      string
		requireRoutingSymbol    string
		requireStablePrefixOnly bool
	}{
		"internal/agentcore/engine.go": {
			requireCacheSymbol:   "PromptCachingStep",
			requireRoutingSymbol: "providerOptions",
		},
		"internal/agentcore/run.go": {
			requireCacheSymbol:   "setModelContextPrefix",
			requireRoutingSymbol: "streamRoundWithResilience",
		},
		"internal/agentcore/structured_output.go": {
			requireCacheSymbol:   "PromptCachingStep",
			requireRoutingSymbol: "providerOptions",
		},
		"internal/agent/interactive.go": {
			requireCacheSymbol:   "PromptCachingStep",
			requireRoutingSymbol: "RoutingProviderOptions",
		},
		"internal/agent/verifier.go": {
			requireCacheSymbol:      "PromptCachingStep",
			requireRoutingSymbol:    "RoutingProviderOptions",
			requireStablePrefixOnly: true,
		},
		"internal/agent/summarize.go": {
			requireCacheSymbol:      "PromptCachingStep",
			requireRoutingSymbol:    "RoutingProviderOptions",
			requireStablePrefixOnly: true,
		},
		// generateOnce: the one-shot aux helper every short-lived host-side
		// call goes through (title, error analysis, memory extraction and
		// graph, learned instructions, the library-prompt, recurring-task and
		// patrol-draft synthesizers).
		"internal/agent/session_log.go": {
			requireCacheSymbol:      "PromptCachingStep",
			requireRoutingSymbol:    "RoutingProviderOptions",
			requireStablePrefixOnly: true,
		},
		"internal/scheduledrun/loop.go": {
			requireCacheSymbol:      "PromptCachingStep",
			requireRoutingSymbol:    "RoutingProviderOptions",
			requireStablePrefixOnly: true,
		},
	}

	// Pass-through fantasy.LanguageModel decorators that delegate .Generate/.Stream
	// to the underlying model rather than constructing an uncached call site.
	decoratorFiles := map[string]bool{
		"internal/agentcore/gemini_schema.go": true,
		// The tracing decorator (#2052): spans around the delegated call.
		"internal/agentcore/telemetry.go": true,
	}

	dirs := []string{
		"..", // relative from internal/agentcore -> internal/
	}
	targetPkgs := []string{"agentcore", "agent", "scheduledrun"}
	fset := token.NewFileSet()
	seenFiles := make(map[string]bool)

	for _, base := range dirs {
		for _, pkg := range targetPkgs {
			pkgDir := filepath.Join(base, pkg)
			entries, err := os.ReadDir(pkgDir)
			if err != nil {
				t.Fatalf("readdir %s: %v", pkgDir, err)
			}
			for _, entry := range entries {
				name := entry.Name()
				if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
					continue
				}
				fullPath := filepath.Join(pkgDir, name)
				relKey := filepath.ToSlash(filepath.Join("internal", pkg, name))
				if decoratorFiles[relKey] {
					continue
				}
				src, err := os.ReadFile(fullPath)
				if err != nil {
					t.Fatalf("read %s: %v", fullPath, err)
				}
				fileAST, err := parser.ParseFile(fset, fullPath, src, 0)
				if err != nil {
					t.Fatalf("parse %s: %v", fullPath, err)
				}

				var llmCalls []string
				ast.Inspect(fileAST, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if pkgIdent, ok := sel.X.(*ast.Ident); ok && pkgIdent.Name == "fantasy" && sel.Sel.Name == "NewAgent" {
						pos := fset.Position(call.Pos())
						llmCalls = append(llmCalls, fmt.Sprintf("%s:%d fantasy.NewAgent", relKey, pos.Line))
					}
					if sel.Sel.Name == "Stream" || sel.Sel.Name == "Generate" {
						// Ignore non-LLM Stream/Generate calls if any; in these 3 packages,
						// all .Stream( and .Generate( calls on agents/models are LLM calls.
						pos := fset.Position(call.Pos())
						llmCalls = append(llmCalls, fmt.Sprintf("%s:%d .%s(", relKey, pos.Line, sel.Sel.Name))
					}
					return true
				})

				if len(llmCalls) == 0 {
					continue
				}
				seenFiles[relKey] = true
				rule, allowed := allowedCallSiteFiles[relKey]
				if !allowed {
					t.Errorf("unregistered LLM call site(s) in %s: %v — every LLM call site must wire agentcore.PromptCachingStep + agentcore.RoutingProviderOptions and be registered in TestAllLLMCallSitesWirePromptCachingAndRouting_ASTGuard", relKey, llmCalls)
					continue
				}
				text := string(src)
				if !strings.Contains(text, rule.requireCacheSymbol) {
					t.Errorf("%s has LLM call site(s) %v but does not reference %q", relKey, llmCalls, rule.requireCacheSymbol)
				}
				if !strings.Contains(text, rule.requireRoutingSymbol) {
					t.Errorf("%s has LLM call site(s) %v but does not reference %q", relKey, llmCalls, rule.requireRoutingSymbol)
				}
				if rule.requireStablePrefixOnly && !strings.Contains(text, "WithStablePrefixOnly") {
					t.Errorf("%s has one-shot LLM call site(s) %v but does not pass WithStablePrefixOnly() to avoid paying 1.25x cache-write charges on unique per-call user prompts", relKey, llmCalls)
				}
			}
		}
	}

	for expectedFile := range allowedCallSiteFiles {
		if !seenFiles[expectedFile] {
			t.Errorf("expected LLM call site file %s was not found or no longer has LLM calls; update TestAllLLMCallSitesWirePromptCachingAndRouting_ASTGuard", expectedFile)
		}
	}
}

// TestPromptCachingStep_WithStablePrefixOnly_AuxiliaryCalls verifies that one-shot
// auxiliary calls (title generation,verifier, reviewer, memory extraction, etc.)
// mark ONLY the reusable system prompt prefix and never attach a cache_control
// breakpoint to the unique per-request user message.
func TestPromptCachingStep_WithStablePrefixOnly_AuxiliaryCalls(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("Static verifier system instructions shared across runs."),
		fantasy.NewUserMessage("Unique transcript for conversation 98123 that will never be sent again."),
	}
	for _, slug := range []string{
		"anthropic/claude-opus-5.5",
		"anthropic/claude-haiku-5.5",
		"claude-sonnet-4.6",
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	} {
		step := PromptCachingStep(slug, WithStablePrefixOnly())
		_, out, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
		if err != nil {
			t.Fatalf("%s step: %v", slug, err)
		}
		if !hasCacheMarker(out.Messages[0]) {
			t.Errorf("%s: system message must carry cache_control marker", slug)
		}
		if hasCacheMarker(out.Messages[1]) {
			t.Errorf("%s: unique one-shot user message must NOT carry cache_control marker when WithStablePrefixOnly is set", slug)
		}
	}
}

// TestNativeProviderCacheContract_AcrossProviderTypes verifies that non-OpenRouter
// native providers built via buildProvider (Anthropic, Bedrock, OpenAI, Azure,
// OpenAICompat) preserve prompt caching semantics on the wire:
//   - Native Anthropic (/v1/messages) and Bedrock (/model/.../invoke) emit
//     explicit `cache_control: {"type": "ephemeral"}` on the system prompt and
//     rolling conversation messages.
//   - Native OpenAI, Azure, and OpenAICompat emit byte-identical tool and
//     message prefixes across turns with no stray cache_control fields.
func TestNativeProviderCacheContract_AcrossProviderTypes(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")

	t.Run("native_anthropic_and_bedrock_emit_explicit_cache_control", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			provType ProviderType
			slug     string
			region   string
		}{
			{
				name:     "Anthropic_native",
				provType: ProviderTypeAnthropic,
				slug:     "claude-sonnet-4.6",
			},
			{
				name:     "Bedrock_native_inference_profile",
				provType: ProviderTypeBedrock,
				slug:     "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
				region:   "us-east-1",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var capturedBodies [][]byte
				var mu sync.Mutex
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					mu.Lock()
					capturedBodies = append(capturedBodies, body)
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{
						"id": "msg_01",
						"type": "message",
						"role": "assistant",
						"model": "`+tc.slug+`",
						"content": [{"type": "text", "text": "Ack"}],
						"stop_reason": "end_turn",
						"usage": {
							"input_tokens": 120,
							"output_tokens": 10,
							"cache_creation_input_tokens": 50,
							"cache_read_input_tokens": 1800
						}
					}`)
				}))
				defer srv.Close()

				prov, err := buildProvider(ProviderConfig{
					Name:    tc.name,
					Type:    tc.provType,
					APIKey:  "test-native-key",
					BaseURL: srv.URL,
					Region:  tc.region,
				})
				if err != nil {
					t.Fatalf("buildProvider(%s): %v", tc.provType, err)
				}
				model, err := prov.LanguageModel(context.Background(), tc.slug)
				if err != nil {
					t.Fatalf("LanguageModel(%s): %v", tc.slug, err)
				}
				ag := fantasy.NewAgent(model, fantasy.WithSystemPrompt("Native provider system prompt."))
				_, err = ag.Generate(context.Background(), fantasy.AgentCall{
					Messages: []fantasy.Message{
						fantasy.NewUserMessage("Turn 1 question"),
						newAssistantMessage("Turn 1 answer"),
					},
					Prompt:      "Turn 2 follow-up",
					PrepareStep: PromptCachingStep(tc.slug),
				})
				if err != nil {
					t.Fatalf("Generate(%s): %v", tc.name, err)
				}
				mu.Lock()
				defer mu.Unlock()
				if len(capturedBodies) != 1 {
					t.Fatalf("captured %d requests, want 1", len(capturedBodies))
				}
				var wire map[string]any
				if err := json.Unmarshal(capturedBodies[0], &wire); err != nil {
					t.Fatalf("unmarshal native wire body: %v", err)
				}
				// Verify system block has cache_control: {"type": "ephemeral"}
				sysBlocks, _ := wire["system"].([]any)
				if len(sysBlocks) == 0 {
					t.Fatalf("native %s request missing system blocks: %s", tc.name, string(capturedBodies[0]))
				}
				lastSys, _ := sysBlocks[len(sysBlocks)-1].(map[string]any)
				if cc, _ := lastSys["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
					t.Errorf("native %s system block missing ephemeral cache_control: %v", tc.name, lastSys)
				}
				// Verify last two messages carry ephemeral cache_control on their content blocks
				msgs, _ := wire["messages"].([]any)
				if len(msgs) != 3 {
					t.Fatalf("native %s messages len = %d, want 3", tc.name, len(msgs))
				}
				for _, idx := range []int{1, 2} {
					m, _ := msgs[idx].(map[string]any)
					content, _ := m["content"].([]any)
					if len(content) == 0 {
						t.Fatalf("native %s message[%d] has empty content", tc.name, idx)
					}
					lastBlock, _ := content[len(content)-1].(map[string]any)
					if cc, _ := lastBlock["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
						t.Errorf("native %s message[%d] missing ephemeral cache_control: %v", tc.name, idx, lastBlock)
					}
				}
			})
		}
	})

	t.Run("native_openai_azure_openaicompat_preserve_prefix_invariance", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			provType ProviderType
			slug     string
		}{
			{name: "OpenAI_native", provType: ProviderTypeOpenAI, slug: "gpt-5.4"},
			{name: "Azure_native", provType: ProviderTypeAzure, slug: "gpt-5-prod"},
			{name: "OpenAICompat_native", provType: ProviderTypeOpenAICompat, slug: "qwen3.8-27b"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var captured []capturedWireRequest
				var mu sync.Mutex
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					req := parseCapturedWireRequest(t, body)
					mu.Lock()
					captured = append(captured, req)
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{
						"id": "chatcmpl-native",
						"object": "chat.completion",
						"created": 1700000000,
						"model": "`+tc.slug+`",
						"choices": [{"index": 0, "message": {"role": "assistant", "content": "Ack"}, "finish_reason": "stop"}],
						"usage": {
							"prompt_tokens": 1024,
							"completion_tokens": 12,
							"total_tokens": 1036,
							"prompt_tokens_details": {"cached_tokens": 896}
						}
					}`)
				}))
				defer srv.Close()

				prov, err := buildProvider(ProviderConfig{
					Name:    tc.name,
					Type:    tc.provType,
					APIKey:  "test-native-key",
					BaseURL: srv.URL,
				})
				if err != nil {
					t.Fatalf("buildProvider(%s): %v", tc.provType, err)
				}
				model, err := prov.LanguageModel(context.Background(), tc.slug)
				if err != nil {
					t.Fatalf("LanguageModel(%s): %v", tc.slug, err)
				}
				ag := fantasy.NewAgent(model, fantasy.WithSystemPrompt("Native implicit-cache system prompt."))
				res1, err := ag.Generate(context.Background(), fantasy.AgentCall{
					Prompt:      "Turn 1 question",
					PrepareStep: PromptCachingStep(tc.slug),
				})
				if err != nil {
					t.Fatalf("Turn 1 Generate(%s): %v", tc.name, err)
				}
				if res1.TotalUsage.CacheReadTokens != 896 {
					t.Errorf("native %s CacheReadTokens = %d, want 896", tc.name, res1.TotalUsage.CacheReadTokens)
				}
				_, err = ag.Generate(context.Background(), fantasy.AgentCall{
					Messages: []fantasy.Message{
						fantasy.NewUserMessage("Turn 1 question"),
						newAssistantMessage("Ack"),
					},
					Prompt:      "Turn 2 follow-up",
					PrepareStep: PromptCachingStep(tc.slug),
				})
				if err != nil {
					t.Fatalf("Turn 2 Generate(%s): %v", tc.name, err)
				}

				mu.Lock()
				defer mu.Unlock()
				if len(captured) != 2 {
					t.Fatalf("captured %d requests, want 2", len(captured))
				}
				for reqIdx, req := range captured {
					if strings.Contains(string(req.RawBody), "cache_control") {
						t.Errorf("native %s request[%d] unexpectedly contains cache_control", tc.name, reqIdx)
					}
				}
				for mIdx := 0; mIdx < len(captured[0].Messages); mIdx++ {
					want := canonicalizeWireMessageWithoutCacheControl(t, captured[0].Messages[mIdx])
					got := canonicalizeWireMessageWithoutCacheControl(t, captured[1].Messages[mIdx])
					if got != want {
						t.Errorf("native %s Turn 2 message[%d] drifted from Turn 1:\ngot:  %s\nwant: %s", tc.name, mIdx, got, want)
					}
				}
			})
		}
	})
}
