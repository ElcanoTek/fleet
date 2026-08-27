package agentcore

// Post-compaction plan re-announcement tests (#990). The task-tracker plan is
// host-side state that survives a compaction, but the summarized history may
// lose it while checkFinishEnforcement keeps enforcing it — so both compaction
// paths re-insert a bounded plan-state message right after the summary when
// open items remain. These tests pin the capture (bounded rendering from the
// tool result), the gating (only with open items and a usable rendering), the
// insertion on both paths, and the at-most-one-live-copy dedup.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// trackerResultJSON builds a synthetic task_tracker structured result in the
// tool's real envelope shape (internal/tools/task_tracker.go renderResult).
func trackerResultJSON(todo, inProgress, done int) string {
	total := todo + inProgress + done
	output := fmt.Sprintf("Task List:\n==========\n\nSummary: %d total (%d todo, %d in progress, %d done)\n\n[ ] [t1] first task\n",
		total, todo, inProgress, done)
	return fmt.Sprintf(`{"status":"success","command":"plan","output":%q,"summary":{"total":%d,"todo":%d,"in_progress":%d,"done":%d}}`,
		output, total, todo, inProgress, done)
}

func TestExtractTaskTrackerRendered(t *testing.T) {
	if got := extractTaskTrackerRendered(trackerResultJSON(2, 1, 0)); !strings.Contains(got, "Task List:") {
		t.Fatalf("JSON envelope rendering not extracted: %q", got)
	}
	// A JSON envelope whose output is not the tracker rendering yields nothing.
	if got := extractTaskTrackerRendered(`{"output":"No tasks in the task list."}`); got != "" {
		t.Fatalf("non-plan output must yield empty, got %q", got)
	}
	// Raw (non-JSON) text carrying the rendering — the hook-fragment /
	// truncation-envelope shapes — falls back to the raw text.
	raw := "Task List:\nSummary: 1 total (1 todo, 0 in progress, 0 done)\n[ ] [a] x\n[hook context follows]"
	if got := extractTaskTrackerRendered(raw); !strings.Contains(got, "Task List:") {
		t.Fatalf("raw fallback not used: %q", got)
	}
	if got := extractTaskTrackerRendered("unrelated text"); got != "" {
		t.Fatalf("unrelated text must yield empty, got %q", got)
	}
	// The kept rendering is bounded.
	huge := trackerResultJSON(1, 0, 0)
	huge = strings.Replace(huge, "first task", strings.Repeat("y", 3*maxTaskTrackerRenderedBytes), 1)
	if got := extractTaskTrackerRendered(huge); len(got) > maxTaskTrackerRenderedBytes {
		t.Fatalf("rendering not bounded: %d bytes", len(got))
	}
}

func TestPlanStateForReannounceGating(t *testing.T) {
	orch := newOrchestrationState(nil, 0)
	if _, ok := orch.planStateForReannounce(); ok {
		t.Fatal("tracker never seen: must not re-announce")
	}
	orch.recordToolResult(toolNameTaskTracker, "{}", trackerResultJSON(0, 0, 3), true)
	if _, ok := orch.planStateForReannounce(); ok {
		t.Fatal("completed plan (no open items) must not re-announce")
	}
	orch.recordToolResult(toolNameTaskTracker, "{}", trackerResultJSON(2, 1, 0), true)
	rendered, ok := orch.planStateForReannounce()
	if !ok || !strings.Contains(rendered, "Task List:") {
		t.Fatalf("open plan must re-announce, got ok=%v rendered=%q", ok, rendered)
	}
}

// planMessageCount counts live plan re-announcement messages in a history.
func planMessageCount(msgs []fantasy.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == fantasy.MessageRoleUser && strings.HasPrefix(msgText(m), planReannouncePrefix) {
			n++
		}
	}
	return n
}

func TestForceCompact_ReannouncesOpenPlan(t *testing.T) {
	e := newMockEngine(t, &mockModel{})
	orch := newOrchestrationState(nil, 0)
	orch.recordToolResult(toolNameTaskTracker, "{}", trackerResultJSON(2, 1, 0), true)
	e.bindRunUsage(orch)

	msgs := make([]fantasy.Message, 0, 40)
	msgs = append(msgs, fantasy.NewUserMessage("HEAD"))
	msgs = append(msgs, fillerMessages(30, 8)...)

	out := e.forceCompactMessageHistory(context.Background(), msgs)
	if !strings.Contains(msgText(out[1]), compactionSummaryPrefix) {
		t.Fatalf("summary must stay first after head: %q", msgText(out[1]))
	}
	if !strings.HasPrefix(msgText(out[2]), planReannouncePrefix) {
		t.Fatalf("plan re-announcement must follow the summary: %q", msgText(out[2]))
	}
	if !strings.Contains(msgText(out[2]), "Task List:") {
		t.Fatalf("plan message must carry the rendering: %q", msgText(out[2]))
	}

	// A second compaction over a history already carrying a plan message keeps
	// exactly one live copy.
	grown := append(out, fillerMessages(30, 8)...)
	out2 := e.forceCompactMessageHistory(context.Background(), grown)
	if got := planMessageCount(out2); got != 1 {
		t.Fatalf("repeated compaction must keep exactly one plan message, got %d", got)
	}
}

func TestProactiveCompact_ReannouncesOpenPlan(t *testing.T) {
	e := newMockEngine(t, &mockModel{})
	orch := newOrchestrationState(nil, 0)
	orch.recordToolResult(toolNameTaskTracker, "{}", trackerResultJSON(1, 0, 0), true)
	e.bindRunUsage(orch)

	msgs := make([]fantasy.Message, 0, 40)
	msgs = append(msgs, fantasy.NewUserMessage("HEAD"))
	msgs = append(msgs, fillerMessages(20, 8)...)

	res := e.proactiveCompact(context.Background(), msgs)
	if !res.compacted {
		t.Fatal("expected compaction")
	}
	if !strings.HasPrefix(msgText(res.messages[2]), planReannouncePrefix) {
		t.Fatalf("plan re-announcement must follow the summary: %q", msgText(res.messages[2]))
	}
	if got := planMessageCount(res.messages); got != 1 {
		t.Fatalf("want exactly one plan message, got %d", got)
	}
}

func TestProactiveCompact_NoPlanNoMessage(t *testing.T) {
	e := newMockEngine(t, &mockModel{})
	e.bindRunUsage(newOrchestrationState(nil, 0))

	msgs := make([]fantasy.Message, 0, 40)
	msgs = append(msgs, fantasy.NewUserMessage("HEAD"))
	msgs = append(msgs, fillerMessages(20, 8)...)

	res := e.proactiveCompact(context.Background(), msgs)
	if !res.compacted {
		t.Fatal("expected compaction")
	}
	if got := planMessageCount(res.messages); got != 0 {
		t.Fatalf("no tracker state: want no plan message, got %d", got)
	}
}
