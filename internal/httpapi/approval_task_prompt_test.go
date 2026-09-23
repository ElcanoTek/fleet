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
		{tools.ManageTasksToolName, `{"action":"stop","task_ids":["x"]}`}, // changes no prompt
		{tools.ScheduleTaskToolName, `not json`},                          // left to the card and the executor
		{"bash", bad},                                                     // not a task tool
	} {
		if err := prevalidateStagedTaskPrompt(tc.tool, tc.raw); err != nil {
			t.Fatalf("%s %q refused: %v", tc.tool, tc.raw, err)
		}
	}
}
