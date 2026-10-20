package agentcore

import (
	"context"
	"encoding/json"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// show_ui by reference (ADR-0086, docs/GENERATIVE-UI.md "A card a tool
// built"). The run gives show_ui two things through its context:
//
//   - the tool results the model can see: the replayed history the turn was
//     given (seed) plus this run's results so far (the sink), newest first;
//   - a place to put the validated card it resolved, which the sink attaches
//     to that show_ui result's RunEntry and tool.result event as display data
//     only. The model is sent the same short UI_DISPLAYED acknowledgement as
//     for an inline card, never the spec.
//
// The seam only reads what this run already holds; it opens no scope, calls
// no tool and reads no other conversation.

// withShowUISeam installs the seam for this run on ctx.
func withShowUISeam(ctx context.Context, sink *streamSink, seed []fantasy.Message) context.Context {
	if sink == nil {
		return ctx
	}
	seedResults := priorToolResultsFromMessages(seed)
	return tools.WithShowUISeam(ctx, tools.ShowUISeam{
		Results: func() []tools.PriorToolResult {
			run := sink.priorToolResults()
			out := make([]tools.PriorToolResult, 0, len(run)+len(seedResults))
			for i := len(run) - 1; i >= 0; i-- {
				out = append(out, run[i])
			}
			for i := len(seedResults) - 1; i >= 0; i-- {
				out = append(out, seedResults[i])
			}
			return out
		},
		ShowCard: sink.noteShownCard,
	})
}

// bridgedToolName names a call by the tool it ran: a deferred tool reached
// through the tool_call bridge is recorded as "tool_call", with the real name
// in its input.
func bridgedToolName(name, input string) string {
	if name != toolNameToolCall {
		return name
	}
	var p struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(input), &p) == nil && strings.TrimSpace(p.Name) != "" {
		return strings.TrimSpace(p.Name)
	}
	return name
}

// priorToolResultsFromMessages lists the tool results in the replayed
// history, oldest first, each named by its call.
func priorToolResultsFromMessages(msgs []fantasy.Message) []tools.PriorToolResult {
	names := map[string]string{}
	var out []tools.PriorToolResult
	for _, m := range msgs {
		for _, part := range m.Content {
			if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				names[tc.ToolCallID] = bridgedToolName(tc.ToolName, tc.Input)
				continue
			}
			tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				continue
			}
			r := tools.PriorToolResult{CallID: tr.ToolCallID, Tool: names[tr.ToolCallID]}
			if e, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Output); ok {
				r.IsErr = true
				if e.Error != nil {
					r.Text = e.Error.Error()
				}
			} else if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output); ok {
				r.Text = txt.Text
			} else {
				continue // media: never a card
			}
			out = append(out, r)
		}
	}
	return out
}

// priorToolResults lists this run's tool results so far, oldest first, each
// named by its call.
func (s *streamSink) priorToolResults() []tools.PriorToolResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := map[string]string{}
	var out []tools.PriorToolResult
	for _, e := range s.entries {
		switch e.Type {
		case "tool_call":
			names[e.ToolCallID] = bridgedToolName(e.ToolName, e.ToolInput)
		case "tool_result":
			name := names[e.ToolCallID]
			if name == "" {
				name = e.ToolName
			}
			out = append(out, tools.PriorToolResult{CallID: e.ToolCallID, Tool: name, Text: e.Text, IsErr: e.IsErr})
		}
	}
	return out
}

// noteShownCard holds the validated spec show_ui resolved for callID until
// that call's result is recorded (onToolResult).
func (s *streamSink) noteShownCard(callID, spec string) {
	if callID == "" || spec == "" {
		return
	}
	s.mu.Lock()
	if s.shownCards == nil {
		s.shownCards = map[string]string{}
	}
	s.shownCards[callID] = spec
	s.mu.Unlock()
}

// takeShownCard returns and forgets the card held for a show_ui result. It is
// attached only to the success it was resolved for: a result that is an
// error, or does not start with UI_DISPLAYED (a governance rewrite, a
// contained panic), gets none. A spec the secret redaction would change is
// dropped rather than shown altered. Caller holds s.mu.
func (s *streamSink) takeShownCard(id, name, text string, isErr bool) string {
	spec, ok := s.shownCards[id]
	if !ok {
		return ""
	}
	delete(s.shownCards, id)
	if isErr || name != tools.ShowUIToolName || !strings.HasPrefix(text, "UI_DISPLAYED") {
		return ""
	}
	if toolRedactor().Redact(spec) != spec {
		return ""
	}
	return spec
}
