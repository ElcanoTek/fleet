package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
)

// self-wake (docs/SELF-WAKE.md): two suspend-and-resume tools for SCHEDULED
// runs, the timer/event counterpart of ask (#510).
//   - sleep: park the task until a deadline. The run ends, releasing the
//     sandbox/lease; the scheduler's wake sweep re-queues it when the
//     deadline passes.
//   - wake_on_event: park the task until a named event arrives
//     (POST /tasks/{id}/wake with the matching key) or the timeout passes,
//     whichever is first. Nothing waits forever: a timeout is always set.
//
// Both require a note — the agent's message to its future self — because the
// resumed run is a FRESH run: the note (plus the wake reason) is what
// carries intent across the gap.
//
// The handler seam mirrors AskHandler: installed on the run context by the
// runner pool, which owns the DB + the per-task cancel. Absent handler → the
// tools aren't registered, so the model never sees a capability it can't use.

// Wake bounds. The minimum stops a busy-loop of sub-tick sleeps (the sweep
// runs every ~30s anyway); the maximum keeps a mistyped deadline from
// parking a task for a year; the default event timeout means "watch for the
// event this week" without the agent doing deadline math.
const (
	WakeMinDelay           = time.Minute
	WakeMaxDelay           = 30 * 24 * time.Hour
	WakeDefaultEventExpiry = 7 * 24 * time.Hour
	wakeEventKeyMaxLen     = 200
	wakeNoteMaxLen         = 4000
)

// WakeSpec is what a wake tool asks the runner to do: park until WakeAt
// (always set), optionally waking early on EventKey, carrying Note to the
// resumed run.
type WakeSpec struct {
	WakeAt   time.Time
	EventKey string
	Note     string
}

// WakeHandler records the wake spec and ends the run. nil = self-wake has no
// sink (interactive / tests) → a clear error, never a silent hang. An error
// return (e.g. the cycle cap) is surfaced to the model so it can finish the
// task normally instead.
type WakeHandler func(spec WakeSpec) error

type wakeHandlerKey struct{}

// WithWakeHandler installs the handler on ctx (nil leaves it untouched).
func WithWakeHandler(ctx context.Context, h WakeHandler) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, wakeHandlerKey{}, h)
}

func wakeHandlerFromContext(ctx context.Context) WakeHandler {
	if h, ok := ctx.Value(wakeHandlerKey{}).(WakeHandler); ok {
		return h
	}
	return nil
}

// WakeHandlerInstalled reports whether a handler is on ctx, so the scheduled
// driver registers the wake tools only when it can actually act.
func WakeHandlerInstalled(ctx context.Context) bool { return wakeHandlerFromContext(ctx) != nil }

// SleepParams / WakeOnEventParams are the typed tool inputs.
type SleepParams struct {
	Seconds int    `json:"seconds,omitempty" description:"How long to sleep, in seconds. Provide seconds OR until, not both. Minimum 60, maximum 30 days."`
	Until   string `json:"until,omitempty" description:"RFC3339 instant to sleep until (e.g. 2026-08-01T09:00:00Z). Provide until OR seconds, not both. At most 30 days out."`
	Note    string `json:"note" description:"Message to your future self: where you left off and what to do on waking. REQUIRED — the resumed run is fresh and this note (plus your task memory) is what survives."`
}

type WakeOnEventParams struct {
	Event          string `json:"event" description:"Name of the event to wait for (e.g. \"deploy-finished\"). An operator or webhook wakes you early via POST /tasks/{id}/wake with this exact key."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" description:"How long to wait before waking anyway, in seconds. Default 7 days, maximum 30 days, minimum 60. On timeout you are told the event never arrived."`
	Note           string `json:"note" description:"Message to your future self: where you left off and what to do when the event fires (or times out). REQUIRED."`
}

const sleepDescription = `Pause this task and resume it later — the task's own alarm clock.

Use when the task should continue after a known delay ("check the results in
an hour", "give the deploy 10 minutes"). The run STOPS and releases its
sandbox; at the deadline the task re-queues as a FRESH run that receives your
note and the wake reason. Prefer finishing the task when nothing is left to
wait for; do not sleep to poll rapidly (minimum 1 minute).`

const wakeOnEventDescription = `Pause this task until a named event wakes it (or a timeout passes).

Use for a standing watch: "resume when the webhook fires". The run STOPS and
releases its sandbox; POST /tasks/{id}/wake with your event key re-queues it
as a FRESH run carrying your note and what woke it. A timeout (default 7
days) always applies — on timeout you are woken and told the event never
arrived. Do not use this to wait for a HUMAN decision — use ask for that.`

// validateWakeNote enforces the note contract shared by both tools.
func validateWakeNote(note string) (string, string) {
	note = strings.TrimSpace(note)
	if note == "" {
		return "", "note is required: write a message to your future self (where you left off, what to do on waking)"
	}
	if len(note) > wakeNoteMaxLen {
		note = note[:wakeNoteMaxLen]
	}
	return note, ""
}

// NewSleepTool creates the timer-wake tool (scheduled set). Its handler ends
// the run; the returned response is what the model sees before the run stops.
func NewSleepTool(now func() time.Time) fantasy.AgentTool {
	return fantasy.NewAgentTool("sleep", sleepDescription,
		func(ctx context.Context, params SleepParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			h := wakeHandlerFromContext(ctx)
			if h == nil {
				return fantasy.NewTextErrorResponse("SLEEP_UNAVAILABLE: self-wake is not supported on this transport. Finish the task instead."), nil
			}
			note, problem := validateWakeNote(params.Note)
			if problem != "" {
				return fantasy.NewTextErrorResponse("sleep: " + problem), nil
			}
			if (params.Seconds > 0) == (strings.TrimSpace(params.Until) != "") {
				return fantasy.NewTextErrorResponse("sleep: provide exactly one of seconds or until"), nil
			}
			var wakeAt time.Time
			if params.Seconds > 0 {
				wakeAt = now().Add(time.Duration(params.Seconds) * time.Second)
			} else {
				t, err := time.Parse(time.RFC3339, strings.TrimSpace(params.Until))
				if err != nil {
					//nolint:nilerr // intentional: a malformed deadline is surfaced to the MODEL as a tool error so it can correct the call; a Go error would fail the turn.
					return fantasy.NewTextErrorResponse("sleep: until must be RFC3339 (e.g. 2026-08-01T09:00:00Z): " + err.Error()), nil
				}
				wakeAt = t
			}
			switch {
			case wakeAt.Before(now().Add(WakeMinDelay)):
				wakeAt = now().Add(WakeMinDelay)
			case wakeAt.After(now().Add(WakeMaxDelay)):
				return fantasy.NewTextErrorResponse(fmt.Sprintf("sleep: deadline is more than %v out; schedule a separate task for horizons that long", WakeMaxDelay)), nil
			}
			if err := h(WakeSpec{WakeAt: wakeAt, Note: note}); err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("sleep: could not park the task (%v). Finish the task normally.", err)), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf(
				"Sleeping until %s. The run is now pausing — stop here; you will be re-run with your note when the timer fires.",
				wakeAt.UTC().Format(time.RFC3339))), nil
		})
}

// NewWakeOnEventTool creates the event-wake tool (scheduled set).
func NewWakeOnEventTool(now func() time.Time) fantasy.AgentTool {
	return fantasy.NewAgentTool("wake_on_event", wakeOnEventDescription,
		func(ctx context.Context, params WakeOnEventParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			h := wakeHandlerFromContext(ctx)
			if h == nil {
				return fantasy.NewTextErrorResponse("WAKE_UNAVAILABLE: self-wake is not supported on this transport. Finish the task instead."), nil
			}
			note, problem := validateWakeNote(params.Note)
			if problem != "" {
				return fantasy.NewTextErrorResponse("wake_on_event: " + problem), nil
			}
			event := strings.TrimSpace(params.Event)
			if event == "" {
				return fantasy.NewTextErrorResponse("wake_on_event: event is required"), nil
			}
			if len(event) > wakeEventKeyMaxLen {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("wake_on_event: event key longer than %d chars", wakeEventKeyMaxLen)), nil
			}
			timeout := WakeDefaultEventExpiry
			if params.TimeoutSeconds > 0 {
				timeout = time.Duration(params.TimeoutSeconds) * time.Second
			}
			switch {
			case timeout < WakeMinDelay:
				timeout = WakeMinDelay
			case timeout > WakeMaxDelay:
				return fantasy.NewTextErrorResponse(fmt.Sprintf("wake_on_event: timeout is more than %v; use a shorter watch (the task can re-arm on waking)", WakeMaxDelay)), nil
			}
			if err := h(WakeSpec{WakeAt: now().Add(timeout), EventKey: event, Note: note}); err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("wake_on_event: could not park the task (%v). Finish the task normally.", err)), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf(
				"Waiting for event %q (timeout %v). The run is now pausing — stop here; you will be re-run with your note when the event fires or the timeout passes.",
				event, timeout)), nil
		})
}
