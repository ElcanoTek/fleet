package tools

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

// ask / notify (#510): two human-in-the-loop message types for SCHEDULED runs.
//   - notify: a non-blocking progress update, fired out-of-band (the notifier,
//     #208). The run continues.
//   - ask: a BLOCKING question. Its handler records the question and ends the
//     run so the sandbox/lease is released and the task parks in
//     paused_awaiting_input; a human answer re-queues the task, and the next
//     run is told the answer. The tool's result tells the model to stop.
//
// The handlers are installed on the run context by the runner pool (which holds
// the DB + notifier + the per-task cancel). Keeping the seam here — colocated
// with the tools — avoids an import cycle (runner/scheduledrun import tools,
// not the other way round).

// AskHandler records a blocking question and ends the run. nil = ask has no
// sink (interactive / tests) → a clear error, never a silent hang.
type AskHandler func(question string) error

// NotifyHandler delivers a non-blocking progress update out-of-band.
type NotifyHandler func(message string)

type askHandlerKey struct{}
type notifyHandlerKey struct{}

// WithAskHandler / WithNotifyHandler install the handlers on ctx (nil leaves it
// untouched).
func WithAskHandler(ctx context.Context, h AskHandler) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, askHandlerKey{}, h)
}

func WithNotifyHandler(ctx context.Context, h NotifyHandler) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, notifyHandlerKey{}, h)
}

func askHandlerFromContext(ctx context.Context) AskHandler {
	if h, ok := ctx.Value(askHandlerKey{}).(AskHandler); ok {
		return h
	}
	return nil
}

func notifyHandlerFromContext(ctx context.Context) NotifyHandler {
	if h, ok := ctx.Value(notifyHandlerKey{}).(NotifyHandler); ok {
		return h
	}
	return nil
}

// AskParams / NotifyParams are the typed tool inputs.
type AskParams struct {
	Question string `json:"question" description:"A clear, self-contained question for the human. Include the context and the specific decision or value you need — the run pauses until they answer, and their answer is added to your next run."`
}

type NotifyParams struct {
	Message string `json:"message" description:"A short progress update for the human. Non-blocking — the run continues."`
}

const askDescription = `Pause the run and ask a human a BLOCKING question.

Use this ONLY when you genuinely cannot proceed without a human decision or a
value only they have (an ambiguous choice, a missing credential the task must
not guess, an irreversible action needing sign-off). The run STOPS: the task
enters "paused, awaiting input", releases its sandbox, and resumes as a fresh
run once the human answers — their answer will be given to you then. Ask ONE
focused question. If you can reasonably proceed, do NOT use this.`

const notifyDescription = `Send a non-blocking progress update to the human.

Use for a meaningful milestone or a heads-up during a long run ("finished
stage 1 of 3", "found 4 anomalies, continuing"). The run CONTINUES — this does
not pause or wait. Use sparingly; it is out-of-band (it may notify by email/
webhook), not a log line.`

// NewNotifyTool creates the non-blocking notify tool (scheduled set).
func NewNotifyTool() fantasy.AgentTool {
	return fantasy.NewAgentTool("notify", notifyDescription,
		func(ctx context.Context, params NotifyParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			msg := strings.TrimSpace(params.Message)
			if msg == "" {
				return fantasy.NewTextErrorResponse("notify: message is required"), nil
			}
			if h := notifyHandlerFromContext(ctx); h != nil {
				h(msg)
			}
			return fantasy.NewTextResponse("Progress update sent. Continue the task."), nil
		})
}

// NewAskTool creates the blocking ask tool (scheduled set). Its handler ends
// the run; the returned response is what the model sees before the run stops.
func NewAskTool() fantasy.AgentTool {
	return fantasy.NewAgentTool("ask", askDescription,
		func(ctx context.Context, params AskParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			q := strings.TrimSpace(params.Question)
			if q == "" {
				return fantasy.NewTextErrorResponse("ask: question is required"), nil
			}
			h := askHandlerFromContext(ctx)
			if h == nil {
				return fantasy.NewTextErrorResponse("ASK_UNAVAILABLE: pausing for human input is not supported on this transport. Do the best you can without it, or explain what you would need."), nil
			}
			if err := h(q); err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("ask: could not pause for input (%v)", err)), nil
			}
			return fantasy.NewTextResponse("Question posed to a human; the run is now pausing. Stop here — you will be re-run with their answer."), nil
		})
}

// AskHandlerInstalled / NotifyHandlerInstalled report whether a handler is on
// ctx, so the scheduled driver registers each tool only when it can actually
// act (keeping the model's tool list honest).
func AskHandlerInstalled(ctx context.Context) bool    { return askHandlerFromContext(ctx) != nil }
func NotifyHandlerInstalled(ctx context.Context) bool { return notifyHandlerFromContext(ctx) != nil }
