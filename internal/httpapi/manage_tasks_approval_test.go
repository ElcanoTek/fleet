package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// The card is the safety mechanism, so what it says has to be exactly what the
// approved call will do (#1152).
func TestSummarizeManageTasksInput(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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

// The card used to show a full prompt replacement as its first 80 characters,
// so a user approving a "slight adjustment" could not see that the rest of the
// prompt was being rewritten. Prompt text now gets its own fields, whole up to
// a generous bound, cut on rune boundaries.
func TestSummarizeManageTasksPromptChanges(t *testing.T) {
	t.Parallel()

	raw, _ := json.Marshal(map[string]any{
		"action":        "update",
		"match":         map[string]any{"query": "daily email"},
		"prompt_edits":  []map[string]string{{"old": "last 7 days", "new": "last 14 days"}, {"old": "Send.", "new": ""}},
		"prompt_append": "  Add a 14-day trend section.  ",
	})
	card := summarizeManageTasksInput(tools.ManageTasksToolName, string(raw))
	edits, _ := card["prompt_edits"].([]map[string]string)
	if len(edits) != 2 || edits[0]["old"] != "last 7 days" || edits[0]["new"] != "last 14 days" || edits[1]["new"] != "" {
		t.Fatalf("prompt_edits = %#v, want each edit as old → new", card["prompt_edits"])
	}
	if card["prompt_append"] != "Add a 14-day trend section." {
		t.Errorf("prompt_append = %q", card["prompt_append"])
	}
	if _, ok := card["prompt_replacement"]; ok {
		t.Error("an edit must not render a replacement block")
	}

	long := strings.Repeat("日本", manageTasksCardPromptRunes) // multibyte, twice the bound
	raw, _ = json.Marshal(map[string]any{"action": "update", "task_ids": []string{"a"}, "prompt": long})
	card = summarizeManageTasksInput(tools.ManageTasksToolName, string(raw))
	full, _ := card["prompt_replacement"].(string)
	if !utf8.ValidString(full) || !strings.HasSuffix(full, cardTruncatedMarker) ||
		utf8.RuneCountInString(full) != manageTasksCardPromptRunes+utf8.RuneCountInString(cardTruncatedMarker) {
		t.Fatalf("prompt_replacement is not a rune-bounded, marked cut (%d runes)", utf8.RuneCountInString(full))
	}
	changes := strings.Join(card["changes"].([]string), " | ")
	if !strings.Contains(changes, "REPLACED IN FULL") {
		t.Errorf("changes = %q, want the full replacement named as one", changes)
	}
}

// Edits are exact text and reach the seam untrimmed; show is never approved.
func TestRunStagedManageTasksPromptEdits(t *testing.T) {
	t.Parallel()

	var got TaskMutationRequest
	s := &Server{manageTasks: func(_ context.Context, req TaskMutationRequest) (*TaskMutationResult, error) {
		got = req
		return &TaskMutationResult{Matched: 1, Changed: []TaskMutationEntry{{ID: "a", Label: "Daily email", Detail: "prompt (1 edit)"}}}, nil
	}}
	out, err := s.runStagedManageTasks(context.Background(), &store.Approval{
		ToolName:  tools.ManageTasksToolName,
		UserEmail: "owner@example.com",
		ArgsJSON:  `{"action":"update","task_ids":["a"],"prompt_edits":[{"old":" 7 days\n","new":" 14 days\n"}],"prompt_append":" more "}`,
	})
	if err != nil {
		t.Fatalf("runStagedManageTasks: %v", err)
	}
	if len(got.PromptEdits) != 1 || got.PromptEdits[0].Old != " 7 days\n" || got.PromptEdits[0].New != " 14 days\n" {
		t.Fatalf("edits reached the seam as %#v, want them byte-exact", got.PromptEdits)
	}
	if got.PromptAppend != "more" || got.Prompt != "" || got.RequestedBy != "owner@example.com" {
		t.Fatalf("request = %#v", got)
	}
	if !strings.Contains(out, "prompt (1 edit)") {
		t.Errorf("report = %q", out)
	}

	got = TaskMutationRequest{}
	if _, err := s.runStagedManageTasks(context.Background(), &store.Approval{
		ToolName: tools.ManageTasksToolName,
		ArgsJSON: `{"action":"show","task_ids":["a"]}`,
	}); err == nil || !strings.Contains(err.Error(), "never approved") {
		t.Fatalf("err = %v, want a staged show refused", err)
	}
	if got.Action != "" {
		t.Error("a staged show must never reach the seam")
	}
}

// show reads through the update's own seam, as the person at the keyboard, so
// it sees exactly the tasks the same selector would change — and names the
// others without their prompts.
func TestManageTasksShowHandler(t *testing.T) {
	t.Parallel()

	if (&Server{}).newManageTasksShowHandler("u@example.com") != nil {
		t.Fatal("an unconfigured server must not offer show")
	}
	var got TaskMutationRequest
	s := &Server{manageTasks: func(_ context.Context, req TaskMutationRequest) (*TaskMutationResult, error) {
		got = req
		return &TaskMutationResult{
			Matched: 2,
			Shown:   []TaskSnapshot{{ID: "a", Label: "Daily email", Status: "scheduled", Cron: "0 6 * * *", Prompt: "Never pass a country filter."}},
			Skipped: []TaskMutationEntry{{ID: "b", Label: "Theirs", Detail: "you did not create this task"}},
		}, nil
	}}
	h := s.newManageTasksShowHandler("u@example.com")
	res, err := h(context.Background(), tools.ManageTasksParams{Action: "show", Match: &tools.TaskMatch{Query: " daily "}})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if got.Action != tools.ManageTasksActionShow || got.RequestedBy != "u@example.com" || got.Match == nil || got.Match.Query != "daily" {
		t.Fatalf("seam request = %#v", got)
	}
	if len(res.Tasks) != 1 || res.Tasks[0].Prompt != "Never pass a country filter." || res.Tasks[0].Cron != "0 6 * * *" {
		t.Fatalf("tasks = %#v", res.Tasks)
	}
	if len(res.NotShown) != 1 || res.NotShown[0].Reason != "you did not create this task" {
		t.Fatalf("not_shown = %#v", res.NotShown)
	}
}
