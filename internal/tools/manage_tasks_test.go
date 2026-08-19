package tools

import (
	"strings"
	"testing"
)

// "In fleet chat you are unable to request a job be amended in the operations
// center. I tried to update all the update dashboard jobs and it refused to do
// so." — 2026-08-13. The bulk shape IS the request, so a selector that names a
// property has to be first-class (#1152).
func TestManageTasksValidate(t *testing.T) {
	cases := []struct {
		name    string
		params  ManageTasksParams
		wantErr string // "" = must validate
	}{
		{
			name:   "bulk update by tag is the actual request",
			params: ManageTasksParams{Action: "update", Match: &TaskMatch{Tag: "pages"}, Cron: "0 7 * * *"},
		},
		{
			name:   "single update by id",
			params: ManageTasksParams{Action: "update", TaskIDs: []string{"abc"}, Model: "x-ai/grok-4.6"},
		},
		{
			name:   "stop by explicit id",
			params: ManageTasksParams{Action: "stop", TaskIDs: []string{"abc", "def"}},
		},
		{
			// Stopping cannot be undone from chat, so it never runs against a
			// filter the human has not seen resolved.
			name:    "stop refuses a filter",
			params:  ManageTasksParams{Action: "stop", Match: &TaskMatch{Tag: "pages"}},
			wantErr: "requires explicit task_ids",
		},
		{
			name:    "stop refuses a filter alongside ids",
			params:  ManageTasksParams{Action: "stop", TaskIDs: []string{"abc"}, Match: &TaskMatch{Tag: "pages"}},
			wantErr: "does not accept match",
		},
		{
			name:    "stop does not silently carry a field change",
			params:  ManageTasksParams{Action: "stop", TaskIDs: []string{"abc"}, Cron: "0 7 * * *"},
			wantErr: "does not change fields",
		},
		{
			// A selector that means two things is a selector nobody can review.
			name:    "update refuses both selectors",
			params:  ManageTasksParams{Action: "update", TaskIDs: []string{"abc"}, Match: &TaskMatch{Tag: "p"}, Cron: "0 7 * * *"},
			wantErr: "not both",
		},
		{
			// An empty match must never be read as "every task".
			name:    "update refuses an empty match",
			params:  ManageTasksParams{Action: "update", Match: &TaskMatch{}, Cron: "0 7 * * *"},
			wantErr: "naming which tasks",
		},
		{
			name:    "update must change something",
			params:  ManageTasksParams{Action: "update", TaskIDs: []string{"abc"}},
			wantErr: "must change something",
		},
		{
			name:    "unknown action",
			params:  ManageTasksParams{Action: "delete", TaskIDs: []string{"abc"}},
			wantErr: "unknown action",
		},
		{
			name:    "missing action",
			params:  ManageTasksParams{TaskIDs: []string{"abc"}},
			wantErr: "action is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.params.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// A mass stop is capped far below a mass update: one is reversible by editing
// the task back, the other ends a schedule.
func TestManageTasksStopIsCappedTighterThanUpdate(t *testing.T) {
	if MaxStopTaskIDs >= MaxManagedTasks {
		t.Fatalf("MaxStopTaskIDs (%d) must be tighter than MaxManagedTasks (%d)", MaxStopTaskIDs, MaxManagedTasks)
	}
	ids := make([]string, MaxStopTaskIDs+1)
	for i := range ids {
		ids[i] = string(rune('a' + i))
	}
	err := ManageTasksParams{Action: "stop", TaskIDs: ids}.Validate()
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("oversized stop = %v, want a cap error", err)
	}
}

// The card renders these lines verbatim, so a change the user cannot read is a
// change they cannot refuse.
func TestManageTasksCardSummaries(t *testing.T) {
	p := ManageTasksParams{
		Action:        "update",
		Match:         &TaskMatch{Query: "update dashboard", Tag: "pages", Model: "old/model"},
		Cron:          "0 7 * * *",
		Model:         "x-ai/grok-4.6",
		MaxIterations: 40,
		Prompt:        "Refresh   the\ndashboard from the newest export",
		AddTags:       []string{"daily", " "},
		RemoveTags:    []string{"legacy"},
	}
	changes := strings.Join(p.ChangeSummary(), " | ")
	for _, want := range []string{"schedule → 0 7 * * *", "model → x-ai/grok-4.6", "max_iterations → 40", "add tags → daily", "remove tags → legacy"} {
		if !strings.Contains(changes, want) {
			t.Errorf("ChangeSummary() = %q, want it to contain %q", changes, want)
		}
	}
	// The prompt preview is flattened to one line: the card is not a transcript.
	if strings.ContainsAny(changes, "\n") {
		t.Errorf("ChangeSummary() = %q, want no line breaks", changes)
	}
	// A blank tag is not a tag.
	if strings.Contains(changes, "daily,") {
		t.Errorf("ChangeSummary() = %q, want the whitespace-only tag dropped", changes)
	}

	match := strings.Join(p.MatchSummary(), " | ")
	for _, want := range []string{"name or prompt contains update dashboard", "tagged pages", "currently on model old/model"} {
		if !strings.Contains(match, want) {
			t.Errorf("MatchSummary() = %q, want it to contain %q", match, want)
		}
	}
}

// A duplicated id would be applied twice and reported twice.
func TestManageTasksCleanTaskIDs(t *testing.T) {
	got := ManageTasksParams{TaskIDs: []string{" a ", "a", "", "b", "  "}}.CleanTaskIDs()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("CleanTaskIDs() = %v, want [a b]", got)
	}
}

// The tool must never execute directly: the approval card is the safety
// mechanism, and a Run that worked would bypass it entirely.
func TestManageTasksToolRunIsATripwire(t *testing.T) {
	tool := NewManageTasksTool()
	if tool.Info().Name != ManageTasksToolName {
		t.Fatalf("tool name = %q, want %q", tool.Info().Name, ManageTasksToolName)
	}
	if !interactiveOnlyToolNames[ManageTasksToolName] {
		t.Error("manage_tasks must be interactive-only: a headless run has no card and no user to read one")
	}
}
