package agentcore

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
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

// callOrdered collects tool results in the order their CALLS were made, not
// the order they finished: two parallel calls to the same card tool finish in
// any order, and "newest" must mean the later call, the same way whether the
// results come from this run (completion order) or from replayed history
// (call order) (Codex P2 on #1819). A result whose call is not seen sorts at
// its own position.
type callOrdered struct {
	seq int
	// open holds, per call id, the calls with that id still waiting for a
	// result, oldest first. A provider may reuse an id, even for parallel
	// calls, so each occurrence keeps its own position and name, and a result
	// answers the oldest one still open: the rule the web applies when it
	// binds results to calls (applyToolResult) (Codex P2 on #1819).
	open map[string][]callOccurrence
	out  []orderedResult
}

type callOccurrence struct {
	pos  int
	name string
}

type orderedResult struct {
	pos int
	r   tools.PriorToolResult
}

func newCallOrdered() *callOrdered {
	return &callOrdered{open: map[string][]callOccurrence{}}
}

func (c *callOrdered) call(id, name, input string) {
	c.seq++
	c.open[id] = append(c.open[id], callOccurrence{pos: c.seq, name: bridgedToolName(name, input)})
}

func (c *callOrdered) result(r tools.PriorToolResult) {
	c.seq++
	pos := c.seq
	if q := c.open[r.CallID]; len(q) > 0 {
		pos = q[0].pos
		if q[0].name != "" {
			r.Tool = q[0].name
		}
		c.open[r.CallID] = q[1:]
	}
	c.out = append(c.out, orderedResult{pos: pos, r: r})
}

// oldestFirst returns the results in call order.
func (c *callOrdered) oldestFirst() []tools.PriorToolResult {
	sort.SliceStable(c.out, func(i, j int) bool { return c.out[i].pos < c.out[j].pos })
	out := make([]tools.PriorToolResult, len(c.out))
	for i, o := range c.out {
		out[i] = o.r
	}
	return out
}

// priorToolResultsFromMessages lists the tool results in the replayed
// history, oldest call first, each named by its call.
func priorToolResultsFromMessages(msgs []fantasy.Message) []tools.PriorToolResult {
	c := newCallOrdered()
	for _, m := range msgs {
		for _, part := range m.Content {
			if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				c.call(tc.ToolCallID, tc.ToolName, tc.Input)
				continue
			}
			tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				continue
			}
			r := tools.PriorToolResult{CallID: tr.ToolCallID}
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
			c.result(r)
		}
	}
	return c.oldestFirst()
}

// priorToolResults lists this run's tool results so far, oldest call first,
// each named by its call.
func (s *streamSink) priorToolResults() []tools.PriorToolResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := newCallOrdered()
	for _, e := range s.entries {
		switch e.Type {
		case "tool_call":
			c.call(e.ToolCallID, e.ToolName, e.ToolInput)
		case "tool_result":
			c.result(tools.PriorToolResult{CallID: e.ToolCallID, Tool: e.ToolName, Text: e.Text, IsErr: e.IsErr})
		}
	}
	return c.oldestFirst()
}

// shownCard is one validated spec waiting for its show_ui result: the
// acknowledgement show_ui returned with it, and the spec.
type shownCard struct{ ack, spec string }

// noteShownCard holds the validated spec show_ui resolved for callID until
// that call's result is recorded (onToolResult). A provider may reuse a call
// id, even for parallel calls, so each occurrence is queued with the
// acknowledgement it returned rather than overwriting the last (Codex P2 on
// #1819).
func (s *streamSink) noteShownCard(callID, ack, spec string) {
	if callID == "" || spec == "" {
		return
	}
	s.mu.Lock()
	if s.shownCards == nil {
		s.shownCards = map[string][]shownCard{}
	}
	s.shownCards[callID] = append(s.shownCards[callID], shownCard{ack: ack, spec: spec})
	s.mu.Unlock()
}

// takeShownCard returns and forgets the card held for a show_ui result. It is
// attached only to the success it was resolved for: the held occurrence whose
// acknowledgement is this result's text, or opens it before a post_tool_use
// hook's fragment (the first, when occurrences of a reused id returned the
// same text). A result that is an error, or whose text no held
// acknowledgement matches (a governance rewrite, a contained panic),
// gets none. A spec the secret redaction would change is dropped rather than
// shown altered. Caller holds s.mu.
func (s *streamSink) takeShownCard(id, name, text string, isErr bool) string {
	held := s.shownCards[id]
	if len(held) == 0 || isErr || name != tools.ShowUIToolName || !strings.HasPrefix(text, "UI_DISPLAYED") {
		return ""
	}
	i := heldFor(held, text)
	if i < 0 {
		return ""
	}
	c := held[i]
	held = slices.Delete(held, i, i+1)
	if len(held) > 0 {
		s.shownCards[id] = held
	} else {
		delete(s.shownCards, id)
	}
	if toolRedactor().Redact(c.spec) != c.spec {
		return ""
	}
	return c.spec
}

// heldFor picks the held occurrence a show_ui result answers: one whose
// acknowledgement is the result's text, else one whose acknowledgement the
// text continues with a post_tool_use hook fragment (appendHookContext adds
// "\n\n<fragment>" after the tool's own text; Codex P2 on #1819). -1 when
// none matches.
func heldFor(held []shownCard, text string) int {
	for i, c := range held {
		if c.ack == text {
			return i
		}
	}
	for i, c := range held {
		if strings.HasPrefix(text, c.ack+"\n\n") {
			return i
		}
	}
	return -1
}
