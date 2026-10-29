package httpapi

import (
	"errors"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// A schedule_task / manage_tasks call whose prompt carries a malformed
// EXECUTION REQUIREMENTS line is refused before a card is staged (#1601), with
// the validator's message, so the model can fix it before a human is asked to
// approve a call the create/edit seam would refuse.
func TestStageRefusesAMalformedTaskPromptBeforeStaging(t *testing.T) {
	bad := `{"prompt":"Refresh.\n` + models.ExecutionRequirementsMarker + `\n{\"mcp_servers\":[\"fast_io + fastio_helpers\"]}","action":"update","task_ids":["x"]}`
	for _, tool := range []string{tools.ScheduleTaskToolName, tools.ManageTasksToolName} {
		// A zero stager has no store or sink: reaching staging would panic, so
		// a clean error proves the refusal came first.
		id, err := (&approvalStager{}).Stage(tool, "call-1", bad)
		if err == nil || id != "" {
			t.Fatalf("%s: staged a malformed prompt (id=%q)", tool, id)
		}
		var refused *agentcore.StageRefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("%s: the refusal is not a StageRefusedError, so the gate would report it as APPROVAL_REQUIRED: %v", tool, err)
		}
		for _, want := range []string{`invalid server or tool identifier "fast_io + fastio_helpers" in mcp_servers[0]`, "Nothing was staged", tool} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: error %q lacks %q", tool, err, want)
			}
		}
	}

	for _, tc := range []struct{ tool, raw string }{
		{tools.ScheduleTaskToolName, `{"prompt":"An ordinary prompt"}`},
		{tools.ScheduleTaskToolName, `{"prompt":"x\n` + models.ExecutionRequirementsMarker + `\n{\"mcp_servers\":[\"pages\"]}"}`},
		{tools.ScheduleTaskToolName, `{"prompt":"Refresh Merrell","cron":"0 18 * * *","tags":["pages","pages:merrell-elc07361","daily_refresh"]}`},
		{tools.ManageTasksToolName, `{"action":"stop","task_ids":["x"]}`}, // changes no prompt
		{tools.ScheduleTaskToolName, `not json`},                          // left to the card and the executor
		{"bash", bad},                                                     // not a task tool
	} {
		if err := prevalidateStagedTaskPrompt(tc.tool, tc.raw); err != nil {
			t.Fatalf("%s %q refused: %v", tc.tool, tc.raw, err)
		}
	}
}

func TestStageRefusesInvalidTaskCronAndTagsBeforeStaging(t *testing.T) {
	cases := []struct {
		name string
		tool string
		raw  string
		want string
	}{
		{
			name: "invalid schedule_task cron",
			tool: tools.ScheduleTaskToolName,
			raw:  `{"prompt":"Refresh","cron":"not a cron"}`,
			want: "invalid cron expression",
		},
		{
			name: "invalid manage_tasks cron",
			tool: tools.ManageTasksToolName,
			raw:  `{"action":"update","task_ids":["x"],"cron":"99 99 * * *"}`,
			want: "invalid cron expression",
		},
		{
			name: "invalid schedule_task tag with space",
			tool: tools.ScheduleTaskToolName,
			raw:  `{"prompt":"Refresh","tags":["has space"]}`,
			want: "invalid task tags",
		},
		{
			name: "invalid manage_tasks add_tags with slash",
			tool: tools.ManageTasksToolName,
			raw:  `{"action":"update","task_ids":["x"],"add_tags":["slash/x"]}`,
			want: "invalid task tags",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := (&approvalStager{}).Stage(tc.tool, "call-1", tc.raw)
			if err == nil || id != "" {
				t.Fatalf("%s: staged invalid args (id=%q)", tc.name, id)
			}
			var refused *agentcore.StageRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("%s: error is not StageRefusedError: %v", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: error %q lacks %q", tc.name, err, tc.want)
			}
		})
	}

	got := sanitizeTaskTagSlice([]string{"pages", "pages:merrell-elc07361", "daily_refresh"})
	if strings.Join(got, ",") != "pages,pages-merrell-elc07361,daily-refresh" {
		t.Fatalf("sanitizeTaskTagSlice = %v", got)
	}
}
