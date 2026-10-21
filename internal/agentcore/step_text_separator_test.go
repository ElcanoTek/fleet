package agentcore

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

func TestStepTextSeparator(t *testing.T) {
	cases := []struct {
		name, prev, next, want string
	}{
		{"nothing streamed yet", "", "Found it.", ""},
		{"whitespace-only prev", "  \n", "Found it.", ""},
		{"empty next", "Let me check.", "", ""},
		{"sentence then sentence", "Let me check the deal.", "Found it.", "\n\n"},
		{"prev ends with a space", "Let me check the deal. ", "Found it.", "\n\n"},
		{"prev ends with one newline", "Let me check.\n", "Found it.", "\n"},
		{"next starts with one newline", "Let me check.", "\nFound it.", "\n"},
		{"one newline each side", "Let me check.\n", "\nFound it.", ""},
		{"prev already ends a paragraph", "Let me check.\n\n", "Found it.", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StepTextSeparator(tc.prev, tc.next); got != tc.want {
				t.Fatalf("StepTextSeparator(%q, %q) = %q, want %q", tc.prev, tc.next, got, tc.want)
			}
		})
	}
}

// The sink adds the break to the first chunk of a new step only — not to later
// chunks of that step, and not between chunks of one step, which a provider
// may split mid-word.
func TestStreamSinkSeparatesStepsNotChunks(t *testing.T) {
	obs := &streamEventObserver{}
	sink := newStreamSink(obs)
	sink.onTextDelta("Let me check ")
	sink.onTextDelta("the deal.")
	sink.onStepFinish()
	sink.onTextDelta("Found")
	sink.onTextDelta(" it.")

	_, text := sink.snapshot()
	if want := "Let me check the deal.\n\nFound it."; text != want {
		t.Fatalf("accumulated text = %q, want %q", text, want)
	}
	if got := reconstructVisibleText(obs.events); got != text {
		t.Fatalf("streamed text %q != accumulated %q", got, text)
	}
}

// A step that streamed no text (a bare tool call) sets no boundary: the first
// text of the run is never prefixed with a blank line.
func TestStreamSinkNoBreakBeforeFirstText(t *testing.T) {
	sink := newStreamSink(nil)
	sink.onStepFinish()
	sink.onTextDelta("Found it.")
	if _, text := sink.snapshot(); text != "Found it." {
		t.Fatalf("accumulated text = %q, want no leading break", text)
	}
}

// A rolled-back attempt is re-driven from a step boundary, so its first text
// carries the break again; the abandoned attempt's break is gone with its text.
func TestStreamSinkRollbackRestoresStepBoundary(t *testing.T) {
	sink := newStreamSink(nil)
	sink.onTextDelta("Let me check.")
	sink.onStepFinish()
	m := sink.mark()
	sink.onTextDelta("partial")
	sink.rollbackTo(m)
	sink.onTextDelta("Found it.")
	if _, text := sink.snapshot(); text != "Let me check.\n\nFound it." {
		t.Fatalf("accumulated text = %q", text)
	}
}

// End to end through Run: the narration a model writes before a tool call and
// the text of the next step reach live clients as two paragraphs, not
// "deal.Found it." The persisted answer is still the final step alone.
func TestRunStreamsStepTextAsSeparateParagraphs(t *testing.T) {
	tool := fantasy.NewAgentTool("lookup_deal", "looks up a deal",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("deal 42"), nil
		})
	var streams atomic.Int32
	model := &mockModel{streamFunc: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		n := streams.Add(1)
		return func(yield func(fantasy.StreamPart) bool) {
			if n == 1 {
				for _, part := range []fantasy.StreamPart{
					{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
					{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "Let me check the deal."},
					{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
					{Type: fantasy.StreamPartTypeToolCall, ID: "call-1", ToolCallName: "lookup_deal", ToolCallInput: "{}"},
				} {
					if !yield(part) {
						return
					}
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				return
			}
			for _, part := range []fantasy.StreamPart{
				{Type: fantasy.StreamPartTypeTextStart, ID: "t2"},
				{Type: fantasy.StreamPartTypeTextDelta, ID: "t2", Delta: "Found it."},
				{Type: fantasy.StreamPartTypeTextEnd, ID: "t2"},
			} {
				if !yield(part) {
					return
				}
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}}
	obs := &streamEventObserver{}
	res, err := Run(context.Background(), ModeInteractive, RunConfig{
		EnvPrefix:   CanonicalEnvPrefix,
		NativeTools: []fantasy.AgentTool{tool},
	}, Deps{
		Input:    stubInput{system: "sys", user: "check deal 42", label: "step-paragraphs"},
		Policy:   passPolicy{},
		Model:    model,
		Observer: obs,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalText != "Found it." {
		t.Fatalf("FinalText = %q, want the last step's text only", res.FinalText)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	var live strings.Builder
	for _, e := range obs.events {
		if e.typ == "text.delta" {
			text, _ := e.payload["text"].(string)
			live.WriteString(text)
		}
	}
	if want := "Let me check the deal.\n\nFound it."; live.String() != want {
		t.Fatalf("live text.delta stream = %q, want %q", live.String(), want)
	}
}
