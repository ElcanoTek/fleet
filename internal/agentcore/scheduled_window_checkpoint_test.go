package agentcore

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

// "Compact only near the model's limit" for scheduled runs: with the resend
// budget off (FLEET_CONTEXT_RESEND_BUDGET_TOKENS=0) and proactive compaction
// opted into (FLEET_SCHEDULED_AUTO_COMPACT=1), the window checkpoint governs a
// scheduled round the way it governs a chat turn.
func TestWindowCheckpointActive(t *testing.T) {
	model := newStopModel("ctx-sched-window-active")
	for _, tc := range []struct {
		name        string
		scheduled   bool
		budget      string
		autoCompact string
		want        bool
	}{
		{"interactive always", false, "", "", true},
		{"interactive ignores the scheduled knobs", false, "1000", "", true},
		{"scheduled default: resend budget", true, "", "", false},
		{"scheduled budget on, auto-compact on: still the budget", true, "1000", "1", false},
		{"scheduled budget off, no auto-compact: no checkpoint", true, "0", "", false},
		{"scheduled budget off + auto-compact: window", true, "0", "1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", tc.budget)
			t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", tc.autoCompact)
			e := newMockEngine(t, model)
			e.envPrefix = CanonicalEnvPrefix
			e.requireCompactionOptIn = tc.scheduled
			if got := e.windowCheckpointActive(); got != tc.want {
				t.Fatalf("windowCheckpointActive = %v, want %v", got, tc.want)
			}
			threshold := e.checkpointThreshold(model)
			switch {
			case tc.want && threshold != e.windowCheckpointLimit(model):
				t.Errorf("threshold = %d, want the window limit %d", threshold, e.windowCheckpointLimit(model))
			case !tc.want && tc.scheduled && tc.budget == "0" && threshold != 0:
				t.Errorf("threshold = %d, want 0 (no checkpoint without the opt-in)", threshold)
			}
		})
	}
}

func runScheduledNearWindow(t *testing.T, autoCompact string) (ticks int32, events []string, crumbs []string, final string) {
	t.Helper()
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "0")
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", autoCompact)
	const toolSteps = 4
	var n atomic.Int32
	tick := fantasy.NewAgentTool("tick", "advance the fake work by one step",
		func(context.Context, tickInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			n.Add(1)
			return fantasy.NewTextResponse("tick ok"), nil
		})
	model := newStopModel("ctx-sched-window-" + autoCompact)
	var calls atomic.Int32
	model.streamFunc = func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		c := calls.Add(1)
		if int(c) <= toolSteps {
			// 95% of the pinned window: past any window threshold.
			return toolStep(fantasy.Usage{InputTokens: testContextWindow * 95 / 100, OutputTokens: 1}, fmt.Sprintf("call-%d", c), c), nil
		}
		return streamTextThenFinish("all ticks done"), nil
	}
	obs := &payloadObserver{}
	session := NewLogSession()
	res, err := Run(context.Background(), ModeScheduled,
		RunConfig{EnvPrefix: CanonicalEnvPrefix, RequireCompactionOptIn: true, NativeTools: []fantasy.AgentTool{tick}},
		Deps{
			Input:      historyInput{system: "s", msgs: fillerMessages(8, 200), label: "sched-window"},
			Observer:   obs,
			Policy:     finishableScheduledPolicy(session),
			Model:      model,
			LogSession: session,
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range session.Messages {
		if strings.Contains(m.Content, "[context_checkpoint]") {
			crumbs = append(crumbs, m.Content)
		}
	}
	return n.Load(), obs.events, crumbs, res.FinalText
}

func TestRun_ScheduledWindowCheckpoint_CompactsNearTheLimitAndResumes(t *testing.T) {
	ticks, events, crumbs, final := runScheduledNearWindow(t, "1")
	if ticks != 4 {
		t.Fatalf("tool ran %d times, want 4: a checkpoint must never drop or replay a step", ticks)
	}
	if !strings.Contains(final, "all ticks done") {
		t.Fatalf("final text = %q", final)
	}
	if len(crumbs) == 0 || !strings.Contains(crumbs[0], "window-compaction threshold") {
		t.Fatalf("expected a window [context_checkpoint] breadcrumb, got %v", crumbs)
	}
	compacted := false
	for _, e := range events {
		if e == evtContextCheckpoint {
			t.Errorf("a window pause streams no %s (the compaction event follows)", evtContextCheckpoint)
		}
		if e == evtContextCompacted {
			compacted = true
		}
	}
	if !compacted {
		t.Fatalf("expected a compaction after the window checkpoint, events=%v", events)
	}
}

func TestRun_ScheduledResendBudgetOff_NoAutoCompact_DoesNotPause(t *testing.T) {
	ticks, events, crumbs, _ := runScheduledNearWindow(t, "")
	if ticks != 4 {
		t.Fatalf("tool ran %d times, want 4", ticks)
	}
	if len(crumbs) != 0 {
		t.Fatalf("without the opt-in a scheduled run must not pause, got %v", crumbs)
	}
	for _, e := range events {
		if e == evtContextCompacted {
			t.Fatalf("without the opt-in a scheduled run must not rewrite its transcript, events=%v", events)
		}
	}
}
