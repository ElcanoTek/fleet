package agentcore

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

// Bodies as OpenRouter relays them for an over-window prompt. fantasy's
// OpenAI-compatible parser flags none of these, so before contextLengthRejection
// each one ended a chat turn on "pick a different model" instead of compacting.
const (
	openAIOverflowBody = `{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","code":"context_length_exceeded","type":"invalid_request_error"}}`
	// Anthropic via OpenRouter: the gateway's own message is opaque; the
	// upstream wording (with both token counts) rides in error.metadata.raw.
	anthropicOverflowBody = `{"error":{"message":"Provider returned error","code":400,"metadata":{"raw":"{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"prompt is too long: 1314963 tokens > 1000000 maximum\"}}","provider_name":"Claude Platform on AWS"}}}`
	googleOverflowBody    = `{"error":{"message":"Provider returned error","code":400,"metadata":{"raw":"The input token count (1196265) exceeds the maximum number of tokens allowed (1048575).","provider_name":"Google AI Studio"}}}`
	// Recorded live through OpenRouter (google/gemini-4-argon, 2026-10-14): no
	// used count, and the limit without parentheses.
	gemini4OverflowBody  = `{"error":{"message":"Provider returned error","code":400,"metadata":{"raw":"{ \"error\": { \"code\": 400, \"message\": \"The input token count exceeds the maximum number of tokens allowed 1048576.\", \"status\": \"INVALID_ARGUMENT\" } }","provider_name":"Google AI Studio"}}}`
	providerCodeOnlyBody = `{"error":{"message":"Provider returned error","code":400,"metadata":{"provider_code":"context_length_exceeded","error_type":"invalid_request","provider_name":"OpenAI"}}}`
)

func TestClassifyStreamError_RelayedContextOverflow(t *testing.T) {
	cases := []struct {
		name      string
		err       *fantasy.ProviderError
		want      streamErrorClass
		wantMax   int
		wantUsed  int
		wantFlags bool
	}{
		{
			name:      "OpenAI wording and code",
			err:       &fantasy.ProviderError{StatusCode: 400, Message: "Your input exceeds the context window of this model. Please adjust your input and try again.", ResponseBody: []byte(openAIOverflowBody)},
			want:      streamErrorContextTooLarge,
			wantFlags: true,
		},
		{
			name:      "OpenAI wording on an in-band stream error (no HTTP status)",
			err:       &fantasy.ProviderError{Title: "stream error", Message: "Your input exceeds the context window of this model. Please adjust your input and try again.", ResponseBody: []byte(openAIOverflowBody)},
			want:      streamErrorContextTooLarge,
			wantFlags: true,
		},
		{
			name:      "Anthropic wording relayed in metadata.raw",
			err:       &fantasy.ProviderError{StatusCode: 400, Message: "Provider returned error", ResponseBody: []byte(anthropicOverflowBody)},
			want:      streamErrorContextTooLarge,
			wantMax:   1_000_000,
			wantUsed:  1_314_963,
			wantFlags: true,
		},
		{
			name:      "Google wording relayed in metadata.raw",
			err:       &fantasy.ProviderError{StatusCode: 400, Message: "Provider returned error", ResponseBody: []byte(googleOverflowBody)},
			want:      streamErrorContextTooLarge,
			wantMax:   1_048_575,
			wantUsed:  1_196_265,
			wantFlags: true,
		},
		{
			name:      "Gemini 4 wording (live-recorded), limit only",
			err:       &fantasy.ProviderError{StatusCode: 400, Message: "Provider returned error", ResponseBody: []byte(gemini4OverflowBody)},
			want:      streamErrorContextTooLarge,
			wantMax:   1_048_576,
			wantFlags: true,
		},
		{
			name:      "classic OpenAI maximum-context wording",
			err:       &fantasy.ProviderError{StatusCode: 400, Message: "This model's maximum context length is 128000 tokens. However, your messages resulted in 131072 tokens."},
			want:      streamErrorContextTooLarge,
			wantMax:   128_000,
			wantUsed:  131_072,
			wantFlags: true,
		},
		{
			name:      "provider_code alone",
			err:       &fantasy.ProviderError{StatusCode: 400, Message: "Provider returned error", ResponseBody: []byte(providerCodeOnlyBody)},
			want:      streamErrorContextTooLarge,
			wantFlags: true,
		},
		{
			name: "an unrelated 400 stays a rejection",
			err:  &fantasy.ProviderError{StatusCode: 400, Message: "Invalid schema for function 'tick'"},
			want: streamErrorProviderRejected,
		},
		{
			name: "a credential failure never compacts, whatever it says",
			err:  &fantasy.ProviderError{StatusCode: 401, Message: "exceeds the context window", AuthError: true},
			want: streamErrorFatal,
		},
		{
			name: "a rate limit is retried, not compacted",
			err:  &fantasy.ProviderError{StatusCode: 429, Message: "context_length_exceeded"},
			want: streamErrorRetryExhausted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, got := classifyStreamError(tc.err)
			if class != tc.want {
				t.Fatalf("class = %v, want %v", class, tc.want)
			}
			if !tc.wantFlags {
				return
			}
			if got == nil || !got.IsContextTooLarge() {
				t.Fatalf("returned error must be flagged context-too-large, got %+v", got)
			}
			if got.ContextMaxTokens != tc.wantMax || got.ContextUsedTokens != tc.wantUsed {
				t.Errorf("tokens used/max = %d/%d, want %d/%d", got.ContextUsedTokens, got.ContextMaxTokens, tc.wantUsed, tc.wantMax)
			}
			if tc.err.ContextTooLargeErr {
				t.Error("the caller's error must not be mutated")
			}
		})
	}
}

// The production shape end to end: a chat turn runs a tool step, then the
// provider rejects the grown prompt with OpenRouter's relayed Anthropic error.
// The turn must compact, resume from the completed step (the tool is not run
// again), finish, and tell the client the history was summarized.
func TestRun_RelayedContextOverflow_CompactsAndResumes(t *testing.T) {
	var ticks atomic.Int32
	tick := fantasy.NewAgentTool("tick", "advance the fake work by one step",
		func(context.Context, tickInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			ticks.Add(1)
			return fantasy.NewTextResponse("tick ok"), nil
		})
	model := newStopModel("ctx-overflow-relayed")
	var calls atomic.Int32
	model.streamFunc = func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		switch calls.Add(1) {
		case 1:
			return toolStep(fantasy.Usage{InputTokens: 500, OutputTokens: 1}, "call-1", 1), nil
		case 2:
			return nil, &fantasy.ProviderError{StatusCode: 400, Message: "Provider returned error", ResponseBody: []byte(anthropicOverflowBody)}
		default:
			return streamTextThenFinish("done after compaction"), nil
		}
	}
	obs := &payloadObserver{}
	res, err := Run(context.Background(), ModeInteractive,
		RunConfig{EnvPrefix: CanonicalEnvPrefix, NativeTools: []fantasy.AgentTool{tick}},
		Deps{
			Input:    historyInput{system: "s", msgs: fillerMessages(compactionKeepTail+6, 200), label: "overflow"},
			Observer: obs,
			Policy:   NewInteractivePolicy(0, 0, nil, nil),
			Executor: &stubExecutor{},
			Model:    model,
		})
	if err != nil {
		t.Fatalf("Run: %v — a relayed over-window rejection must compact and retry, not end the turn", err)
	}
	if got := ticks.Load(); got != 1 {
		t.Fatalf("tool ran %d times, want 1: recovery must resume after the completed step, never replay it", got)
	}
	if !strings.Contains(res.FinalText, "done after compaction") {
		t.Fatalf("final text = %q, want the answer from the retried call", res.FinalText)
	}
	p := obs.payloadOf(evtContextCompacted)
	if p == nil || p[evtFieldTrigger] != "overflow" {
		t.Fatalf("expected a %s event with trigger=overflow, got %v (events %v)", evtContextCompacted, p, obs.events)
	}
	if removed, _ := p[evtFieldRemovedTurns].(int); removed <= 0 {
		t.Errorf("removed_turns = %v, want > 0", p[evtFieldRemovedTurns])
	}
}

// A chat turn is a single round. Once a tool step's real prompt size crosses
// the compaction threshold, the round pauses, the run loop compacts, and the
// SAME conversation resumes: every tool step runs exactly once, the turn
// finishes, and no scheduled-only checkpoint event reaches the chat client.
func TestRun_WindowCheckpoint_InteractivePausesCompactsAndResumes(t *testing.T) {
	const toolSteps = 4
	var ticks atomic.Int32
	tick := fantasy.NewAgentTool("tick", "advance the fake work by one step",
		func(context.Context, tickInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			ticks.Add(1)
			return fantasy.NewTextResponse("tick ok"), nil
		})
	model := newStopModel("ctx-window-checkpoint") // window pinned at testContextWindow
	var calls atomic.Int32
	model.streamFunc = func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		n := calls.Add(1)
		if int(n) <= toolSteps {
			// 95% of the window: over the 0.90 compaction threshold.
			usage := fantasy.Usage{InputTokens: testContextWindow * 95 / 100, OutputTokens: 1}
			return toolStep(usage, fmt.Sprintf("call-%d", n), n), nil
		}
		return streamTextThenFinish("all ticks done"), nil
	}
	obs := &payloadObserver{}
	session := NewLogSession()
	res, err := Run(context.Background(), ModeInteractive,
		RunConfig{EnvPrefix: CanonicalEnvPrefix, NativeTools: []fantasy.AgentTool{tick}},
		Deps{
			Input:      historyInput{system: "s", msgs: fillerMessages(8, 200), label: "window"},
			Observer:   obs,
			Policy:     NewInteractivePolicy(0, 0, nil, nil),
			Executor:   &stubExecutor{},
			Model:      model,
			LogSession: session,
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := ticks.Load(); got != toolSteps {
		t.Fatalf("tool ran %d times, want %d — a checkpoint must never drop or replay a step", got, toolSteps)
	}
	if !strings.Contains(res.FinalText, "all ticks done") {
		t.Fatalf("final text = %q, want the model's closing answer", res.FinalText)
	}
	compacted := false
	for i, e := range obs.events {
		if e == evtContextCheckpoint {
			t.Errorf("interactive runs must not stream %s (the chat client hears about the compaction instead)", evtContextCheckpoint)
		}
		if e == evtContextCompacted && obs.payloads[i][evtFieldTrigger] == "window" {
			compacted = true
		}
	}
	if !compacted {
		t.Fatalf("expected a window compaction after the mid-turn checkpoint, events=%v", obs.events)
	}
	crumb := false
	for _, m := range session.Messages {
		if strings.Contains(m.Content, "[context_checkpoint] prompt 95000 tokens reached the window-compaction threshold") {
			crumb = true
		}
	}
	if !crumb {
		t.Error("expected a window [context_checkpoint] breadcrumb in the session log")
	}
}

func TestCheckpointThreshold(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "1000")
	model := newStopModel("ctx-checkpoint-threshold")

	interactive := newMockEngine(t, model)
	interactive.envPrefix = CanonicalEnvPrefix
	if got, want := interactive.checkpointThreshold(model), testContextWindow*90/100; got != want {
		t.Errorf("interactive threshold = %d, want %d (0.90 of the window)", got, want)
	}
	if interactive.checkpointThreshold(nil) != 0 {
		t.Error("no active model, no window checkpoint")
	}
	interactive.windowCompactionStalled = true
	if interactive.checkpointThreshold(model) != 0 {
		t.Error("a stalled window compaction must make the checkpoint inert")
	}

	scheduled := newMockEngine(t, model)
	scheduled.envPrefix = CanonicalEnvPrefix
	scheduled.requireCompactionOptIn = true
	if got := scheduled.checkpointThreshold(model); got != 1000 {
		t.Errorf("scheduled threshold = %d, want the 1000-token resend budget", got)
	}
}
