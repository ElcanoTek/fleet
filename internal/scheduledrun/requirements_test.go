package scheduledrun

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestExecutionRequirementsCopyablePreflight(t *testing.T) {
	req, err := parseExecutionRequirements("TASK\nEXECUTION REQUIREMENTS (JSON):\n{\"mcp_servers\":[\"reports\"],\"required_tools\":[\"mcp_reports_download\"],\"network\":true,\"model_required\":true,\"mode\":\"inventory_check\"}\nRun only once.")
	if err != nil || req == nil {
		t.Fatalf("parse: %+v %v", req, err)
	}
	if err := req.checkNetwork(false); err == nil || !strings.Contains(err.Error(), "Allow network egress") {
		t.Fatalf("sealed task not diagnosed: %v", err)
	}
	if err := req.checkNetwork(true); err != nil {
		t.Fatal(err)
	}
	catalog := []mcp.ServerTool{{ServerName: "reports", Tool: mcp.Tool{Name: "download"}}}
	if err := req.checkTools(catalog, nil); err != nil {
		t.Fatal(err)
	}
	catalog[0].Tool.Name = "resolve"
	if err := req.checkTools(catalog, nil); err == nil || !strings.Contains(err.Error(), "mcp_reports_download") {
		t.Fatalf("partial tool roster accepted: %v", err)
	}
	if err := req.checkTools(nil, nil); err == nil || !strings.Contains(err.Error(), "server reports") {
		t.Fatalf("missing source accepted: %v", err)
	}
}

func TestExecutionRequirementsLegacyAndInvalid(t *testing.T) {
	req, err := parseExecutionRequirements("An ordinary existing scheduled prompt")
	if err != nil || req != nil || req.checkNetwork(false) != nil || req.checkTools(nil, nil) != nil {
		t.Fatal("legacy behavior changed")
	}
	for _, body := range []string{"", "null", "[]", "{bad}", `{"mcp_servers":["invalid secret value"]}`, strings.Repeat("x", 16385), "{}\n" + executionRequirementsMarker + "\n{}"} {
		if _, err := parseExecutionRequirements(executionRequirementsMarker + "\n" + body); err == nil {
			t.Fatalf("invalid requirements accepted: %.50q", body)
		}
	}
}

func TestRunWorkerChecksRequirementsBeforeModelSetup(t *testing.T) {
	// No manager/config is installed: reaching model setup would panic. A bad
	// handoff must fail before any provider work or source processing happens.
	for _, prompt := range []string{
		executionRequirementsMarker + "\n{\"network\":true}",
		executionRequirementsMarker + "\nnull",
	} {
		task := &models.Task{Prompt: prompt}
		session, _, _, err := (&Runner{}).runWorker(context.Background(), task, "", nil, "")
		if err == nil || !strings.Contains(err.Error(), "execution requirements:") || session != nil {
			t.Fatalf("late/missing preflight: %v %v", session, err)
		}
		if task.AllowNetwork {
			t.Fatal("preflight widened task permissions")
		}
	}
}

// The dispatch parser and the save-time validator (models, #1601) accept and
// refuse exactly the same prompts: a declaration that reaches dispatch has
// been validated the same way when the task was saved.
func TestParseExecutionRequirementsAgreesWithSaveTimeValidation(t *testing.T) {
	if executionRequirementsMarker != models.ExecutionRequirementsMarker {
		t.Fatal("dispatch and save-time validation must key on the same marker")
	}
	for _, body := range []string{
		`{"mcp_servers":["reports"],"required_tools":["mcp_reports_download"],"network":true}`,
		`{"mcp_servers":["fast_io + fastio_helpers"]}`, `{"required_tools":[" x"]}`, `{}`, `null`, `[]`, `{bad}`,
		`{"network":"yes"}`, `{"mcp_servers":"x"}`, strings.Repeat("x", 16385), "{}\n" + executionRequirementsMarker + "\n{}",
	} {
		prompt := "TASK\n" + executionRequirementsMarker + "\n" + body
		_, dispatchErr := parseExecutionRequirements(prompt)
		saveErr := models.ValidateExecutionRequirements(prompt)
		if (dispatchErr == nil) != (saveErr == nil) {
			t.Fatalf("dispatch and save-time validation disagree on %.60q: dispatch=%v save=%v", body, dispatchErr, saveErr)
		}
		if dispatchErr != nil && dispatchErr.Error() != saveErr.Error() {
			t.Fatalf("dispatch must report the save-time message, got %q vs %q", dispatchErr, saveErr)
		}
	}
}
