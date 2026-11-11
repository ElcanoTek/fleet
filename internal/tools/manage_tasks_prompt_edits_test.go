package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"
)

// A user asked for a small addition to a scheduled email. The only prompt
// change manage_tasks offered was a full replacement, and the model could not
// read the prompt it was replacing, so it retyped it from memory and the rules
// the working prompt had accumulated were silently dropped. Edits apply to the
// stored text, so what an edit does not name survives word for word.
func TestApplyPromptEdits(t *testing.T) {
	const stored = "Pull yesterday's report.\nNever pass a country filter.\nColumns: date, spend.\nSend it to the team."

	t.Run("an edit changes only the text it names", func(t *testing.T) {
		got, err := ApplyPromptEdits(stored, []PromptEdit{{Old: "Pull yesterday's report.", New: "Pull the last 14 days of the report."}}, "")
		if err != nil {
			t.Fatalf("ApplyPromptEdits: %v", err)
		}
		want := "Pull the last 14 days of the report.\nNever pass a country filter.\nColumns: date, spend.\nSend it to the team."
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("edits apply in order, each to the text the earlier ones left", func(t *testing.T) {
		got, err := ApplyPromptEdits(stored, []PromptEdit{
			{Old: "date, spend", New: "date, spend, trend"},
			{Old: "spend, trend", New: "spend, 14-day trend"},
			{Old: "\nSend it to the team.", New: ""},
		}, "")
		if err != nil {
			t.Fatalf("ApplyPromptEdits: %v", err)
		}
		if !strings.Contains(got, "Columns: date, spend, 14-day trend.") || strings.Contains(got, "Send it") {
			t.Fatalf("got %q", got)
		}
		if !strings.Contains(got, "Never pass a country filter.") {
			t.Fatalf("an unrelated rule was lost: %q", got)
		}
	})

	t.Run("old text that is missing refuses the whole task", func(t *testing.T) {
		_, err := ApplyPromptEdits(stored, []PromptEdit{
			{Old: "Columns: date, spend.", New: "Columns: date, spend, trend."},
			{Old: "a rule the model only remembered", New: "x"},
		}, "")
		if err == nil || !strings.Contains(err.Error(), "prompt edit 2") || !strings.Contains(err.Error(), "is not in this task's prompt") {
			t.Fatalf("err = %v, want edit 2 reported as not found", err)
		}
	})

	t.Run("old text that is ambiguous refuses rather than guessing", func(t *testing.T) {
		_, err := ApplyPromptEdits("spend. spend.", []PromptEdit{{Old: "spend.", New: "cost."}}, "")
		if err == nil || !strings.Contains(err.Error(), "appears 2 times") {
			t.Fatalf("err = %v, want the occurrence count", err)
		}
	})

	t.Run("append keeps the prompt and adds a paragraph", func(t *testing.T) {
		for _, tc := range []struct{ current, want string }{
			{"Line.", "Line.\n\nAdd a trend."},
			{"Line.\n", "Line.\n\nAdd a trend."},
			{"Line.\n\n", "Line.\n\nAdd a trend."},
			{"", "Add a trend."},
		} {
			got, err := ApplyPromptEdits(tc.current, nil, "  Add a trend.\n")
			if err != nil || got != tc.want {
				t.Errorf("append to %q = %q, %v; want %q", tc.current, got, err, tc.want)
			}
		}
	})

	t.Run("multibyte text is matched and reported whole", func(t *testing.T) {
		got, err := ApplyPromptEdits("Résumé — 日本語 ✓", []PromptEdit{{Old: "日本語", New: "español"}}, "")
		if err != nil || got != "Résumé — español ✓" {
			t.Fatalf("got %q, %v", got, err)
		}
		_, err = ApplyPromptEdits("x", []PromptEdit{{Old: strings.Repeat("é", 200), New: "y"}}, "")
		if err == nil || !utf8.ValidString(err.Error()) {
			t.Fatalf("err = %v, want a valid-UTF-8 not-found error", err)
		}
	})
}

func TestManageTasksValidatePromptEditsAndShow(t *testing.T) {
	ids := []string{"abc"}
	cases := []struct {
		name    string
		params  ManageTasksParams
		wantErr string
	}{
		{name: "edits alone are a change", params: ManageTasksParams{Action: "update", TaskIDs: ids, PromptEdits: []PromptEdit{{Old: "a", New: "b"}}}},
		{name: "append alone is a change", params: ManageTasksParams{Action: "update", Match: &TaskMatch{Tag: "x"}, PromptAppend: "more"}},
		{name: "edits with a full prompt contradict", params: ManageTasksParams{Action: "update", TaskIDs: ids, Prompt: "all new", PromptEdits: []PromptEdit{{Old: "a", New: "b"}}}, wantErr: "one or the other"},
		{name: "append with a full prompt contradict", params: ManageTasksParams{Action: "update", TaskIDs: ids, Prompt: "all new", PromptAppend: "more"}, wantErr: "one or the other"},
		{name: "an empty old is refused", params: ManageTasksParams{Action: "update", TaskIDs: ids, PromptEdits: []PromptEdit{{Old: "", New: "b"}}}, wantErr: "old is empty"},
		{name: "a no-op edit is refused", params: ManageTasksParams{Action: "update", TaskIDs: ids, PromptEdits: []PromptEdit{{Old: "a", New: "a"}}}, wantErr: "with itself"},
		{name: "too many edits", params: ManageTasksParams{Action: "update", TaskIDs: ids, PromptEdits: make([]PromptEdit, MaxPromptEdits+1)}, wantErr: "at most"},
		{name: "a stop carries no edit", params: ManageTasksParams{Action: "stop", TaskIDs: ids, PromptAppend: "x"}, wantErr: "does not change fields"},
		{name: "show by id", params: ManageTasksParams{Action: "show", TaskIDs: ids}},
		{name: "show by match", params: ManageTasksParams{Action: "show", Match: &TaskMatch{Query: "daily email"}}},
		{name: "show needs a selector", params: ManageTasksParams{Action: "show"}, wantErr: "which tasks to show"},
		{name: "show refuses both selectors", params: ManageTasksParams{Action: "show", TaskIDs: ids, Match: &TaskMatch{Tag: "x"}}, wantErr: "not both"},
		{name: "show only reads", params: ManageTasksParams{Action: "show", TaskIDs: ids, PromptAppend: "x"}, wantErr: "only reads"},
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

// The approver most needs to know whether the prompt is being edited or
// replaced, so the change line says which, in words.
func TestManageTasksChangeSummaryNamesThePromptChangeKind(t *testing.T) {
	full := strings.Join(ManageTasksParams{Prompt: "Résumé\nnew"}.ChangeSummary(), " | ")
	if !strings.Contains(full, "REPLACED IN FULL (10 characters)") || !strings.Contains(full, "anything not in the new text is removed") {
		t.Errorf("full replacement line = %q", full)
	}
	edits := strings.Join(ManageTasksParams{PromptEdits: []PromptEdit{{Old: "a", New: "b"}, {Old: "c", New: "d"}}, PromptAppend: "x"}.ChangeSummary(), " | ")
	for _, want := range []string{"2 exact-text edits; the rest of the prompt is kept", "text added at the end"} {
		if !strings.Contains(edits, want) {
			t.Errorf("edit lines = %q, want %q", edits, want)
		}
	}
	if strings.Contains(edits, "REPLACED") {
		t.Errorf("an edit must not read as a replacement: %q", edits)
	}
}

func TestIsManageTasksShow(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"action":"show","task_ids":["a"]}`:      true,
		`{"action":" show ","match":{"tag":"x"}}`: true,
		`{"action":"update","task_ids":["a"]}`:    false,
		`{"action":"Show"}`:                       false,
		`{"task_ids":["a"]}`:                      false,
		`not json`:                                false,
	} {
		if got := IsManageTasksShow(raw); got != want {
			t.Errorf("IsManageTasksShow(%s) = %v, want %v", raw, got, want)
		}
	}
}

func runManageTasks(ctx context.Context, t *testing.T, input string) (fantasy.ToolResponse, error) {
	t.Helper()
	return NewManageTasksTool().Run(ctx, fantasy.ToolCall{ID: "tc-1", Name: ManageTasksToolName, Input: input})
}

// show is the one action the tool runs itself, and it only reads; every other
// action is still a tripwire, because the approval card is what guards them.
func TestManageTasksToolRunShow(t *testing.T) {
	var got ManageTasksParams
	long := strings.Repeat("é", MaxShownPromptRunes+5)
	ctx := WithManageTasksShowHandler(context.Background(), func(_ context.Context, p ManageTasksParams) (*ManageTasksShowResult, error) {
		got = p
		return &ManageTasksShowResult{
			Tasks: []ManagedTaskView{
				{ID: "t1", Name: "Daily email", Prompt: "Never pass a country filter."},
				{ID: "t2", Name: "Huge", Prompt: long},
			},
			NotShown: []ManagedTaskSkip{{ID: "t3", Name: "Theirs", Reason: "you did not create this task"}},
		}, nil
	})

	resp, err := runManageTasks(ctx, t, `{"action":"show","match":{"query":"daily"}}`)
	if err != nil || resp.IsError {
		t.Fatalf("show: resp=%+v err=%v", resp, err)
	}
	if got.Match == nil || got.Match.Query != "daily" {
		t.Fatalf("handler got %+v, want the selector passed through", got)
	}
	var res ManageTasksShowResult
	if err := json.Unmarshal([]byte(resp.Content), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, resp.Content)
	}
	if len(res.Tasks) != 2 || res.Tasks[0].Prompt != "Never pass a country filter." {
		t.Fatalf("tasks = %+v", res.Tasks)
	}
	if !res.Tasks[1].PromptTruncated || utf8.RuneCountInString(res.Tasks[1].Prompt) != MaxShownPromptRunes || !utf8.ValidString(res.Tasks[1].Prompt) {
		t.Errorf("an oversized prompt must be cut on a rune boundary and flagged")
	}
	if res.Tasks[0].PromptTruncated {
		t.Error("a normal prompt must not be flagged as cut")
	}
	if len(res.NotShown) != 1 || !strings.Contains(res.Note, "prompt_edits") {
		t.Errorf("not_shown=%+v note=%q", res.NotShown, res.Note)
	}

	// A show that tries to carry a change is refused, not half-run.
	resp, err = runManageTasks(ctx, t, `{"action":"show","task_ids":["t1"],"prompt":"x"}`)
	if err != nil || !resp.IsError || !strings.Contains(resp.Content, "only reads") {
		t.Fatalf("show with a change: resp=%+v err=%v", resp, err)
	}

	// No handler (a headless or unconfigured context) says so plainly.
	resp, err = runManageTasks(context.Background(), t, `{"action":"show","task_ids":["t1"]}`)
	if err != nil || !resp.IsError || !strings.Contains(resp.Content, "not available") {
		t.Fatalf("show without a handler: resp=%+v err=%v", resp, err)
	}

	// Every write action stays a tripwire even with the handler present.
	for _, input := range []string{
		`{"action":"update","task_ids":["t1"],"prompt_append":"x"}`,
		`{"action":"stop","task_ids":["t1"]}`,
	} {
		if _, err := runManageTasks(ctx, t, input); err == nil {
			t.Errorf("%s ran directly; it must only ever run after approval", input)
		}
	}
}
