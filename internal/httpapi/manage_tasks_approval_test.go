package httpapi

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// The card is the safety mechanism, so what it says has to be exactly what the
// approved call will do (#1152).
func TestSummarizeManageTasksInput(t *testing.T) {
	update := summarizeManageTasksInput(tools.ManageTasksToolName,
		`{"action":"update","match":{"tag":"pages"},"cron":"0 7 * * *","model":"x-ai/grok-4.6"}`)
	if update["action"] != "update" {
		t.Fatalf("action = %v, want update", update["action"])
	}
	if _, destructive := update["destructive"]; destructive {
		t.Error("an update must not read as destructive; that word is reserved for stop")
	}
	match, _ := update["match"].([]string)
	if len(match) != 1 || !strings.Contains(match[0], "tagged pages") {
		t.Errorf("match = %v, want the tag selector rendered", match)
	}
	changes, _ := update["changes"].([]string)
	if len(changes) != 2 {
		t.Errorf("changes = %v, want both the schedule and the model", changes)
	}
	// The blast-radius ceiling belongs on the card: approving a filter means
	// approving whatever it matches.
	if update["max_tasks"] != tools.MaxManagedTasks {
		t.Errorf("max_tasks = %v, want %d", update["max_tasks"], tools.MaxManagedTasks)
	}

	// A stop ends a schedule with no undo from chat, so the card says so
	// instead of leaving the user to infer it.
	stop := summarizeManageTasksInput(tools.ManageTasksToolName, `{"action":"stop","task_ids":["a","b"]}`)
	if stop["destructive"] != true {
		t.Error("a stop must be marked destructive")
	}
	consequence, _ := stop["consequence"].(string)
	if !strings.Contains(consequence, "will not run again") {
		t.Errorf("consequence = %q, want it to state the irreversible part", consequence)
	}
	ids, _ := stop["task_ids"].([]string)
	if len(ids) != 2 {
		t.Errorf("task_ids = %v, want both", ids)
	}

	// Unparseable args degrade to the raw payload rather than a half-rendered
	// card that misstates the action.
	broken := summarizeManageTasksInput(tools.ManageTasksToolName, "{not json")
	if broken["raw"] != "{not json" {
		t.Errorf("broken summary = %v, want the raw payload", broken)
	}
}

// The args sat in a row while a human read a card, so every rule that does not
// need storage is re-checked at execution rather than trusted from staging.
func TestRunStagedManageTasksRevalidates(t *testing.T) {
	called := false
	s := &Server{manageTasks: func(context.Context, TaskMutationRequest) (*TaskMutationResult, error) {
		called = true
		return &TaskMutationResult{}, nil
	}}
	// A filtered stop is refused at execution too, not only at staging.
	_, err := s.runStagedManageTasks(context.Background(), &store.Approval{
		ToolName: tools.ManageTasksToolName,
		ArgsJSON: `{"action":"stop","match":{"tag":"pages"}}`,
	})
	if err == nil || !strings.Contains(err.Error(), "requires explicit task_ids") {
		t.Fatalf("err = %v, want the stop-needs-ids refusal", err)
	}
	if called {
		t.Error("a refused call must never reach the orchestrator seam")
	}
}

// An unconfigured deployment says so rather than reporting a change it did not
// make.
func TestRunStagedManageTasksUnconfigured(t *testing.T) {
	s := &Server{}
	_, err := s.runStagedManageTasks(context.Background(), &store.Approval{
		ToolName: tools.ManageTasksToolName,
		ArgsJSON: `{"action":"update","task_ids":["a"],"cron":"0 7 * * *"}`,
	})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v, want an unconfigured-server error", err)
	}
}

// A bulk edit that reports only a count is a bulk edit nobody can check — and
// the operator asking for this was fixing jobs that had drifted, so "which
// ones" is the whole question.
func TestFormatTaskMutationReportNamesEveryTask(t *testing.T) {
	report := formatTaskMutationReport("update", &TaskMutationResult{
		Matched: 3,
		Changed: []TaskMutationEntry{
			{ID: "id-1", Label: "TWC daily refresh", Detail: "schedule 0 7 * * *"},
			{ID: "id-2", Label: "Comfluence daily refresh", Detail: "schedule 0 7 * * *"},
		},
		Skipped: []TaskMutationEntry{
			{ID: "id-3", Label: "Someone else's job", Detail: "you did not create this task"},
		},
	}, "https://fleet.example/orchestrator")

	for _, want := range []string{"Updated 2 task(s)", "TWC daily refresh", "id-1", "Skipped 1", "Someone else's job", "you did not create this task", "https://fleet.example/orchestrator"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}

	// A selector that matched nothing must say so plainly rather than reading
	// as a silent success.
	empty := formatTaskMutationReport("stop", &TaskMutationResult{}, "")
	if !strings.Contains(empty, "Nothing changed") {
		t.Errorf("empty report = %q, want it to state that nothing changed", empty)
	}
	if !strings.Contains(formatTaskMutationReport("stop", &TaskMutationResult{
		Changed: []TaskMutationEntry{{ID: "x", Label: "Job", Detail: "stopped"}},
	}, ""), "Stopped 1 task(s)") {
		t.Error("a stop report must use the stop verb")
	}
}
