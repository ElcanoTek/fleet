package scheduledrun

import (
	"context"
	"fmt"
	"reflect"
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
	for _, body := range []string{"", "null", "[]", "{bad}", `{"mcp_servers":["invalid secret value"]}`, strings.Repeat("x", 16385), "{}\n" + models.ExecutionRequirementsMarker + "\n{}"} {
		if _, err := parseExecutionRequirements(models.ExecutionRequirementsMarker + "\n" + body); err == nil {
			t.Fatalf("invalid requirements accepted: %.50q", body)
		}
	}
}

func TestRunWorkerChecksRequirementsBeforeModelSetup(t *testing.T) {
	// No manager/config is installed: reaching model setup would panic. A bad
	// handoff must fail before any provider work or source processing happens.
	for _, prompt := range []string{
		models.ExecutionRequirementsMarker + "\n{\"network\":true}",
		models.ExecutionRequirementsMarker + "\nnull",
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

// Dispatch reads the declaration through the one parser (models, #1601): the
// same prompts are refused with the same message as at save time, and an
// accepted one carries exactly the parsed declaration, roster included. There
// is no second grammar left to drift.
func TestParseExecutionRequirementsIsTheModelsParser(t *testing.T) {
	for _, body := range []string{
		`{"mcp_servers":["reports"],"required_tools":["mcp_reports_download"],"network":true}`,
		`{"mcp_servers":["fast_io + fastio_helpers"]}`, `{"required_tools":[" x"]}`, `{}`, `null`, `[]`, `{bad}`, "",
		`{"network":"yes"}`, `{"mcp_servers":"x"}`, strings.Repeat("x", 16385), "{}\n" + models.ExecutionRequirementsMarker + "\n{}",
		`{"completion":{"any_succeeded":["mcp_pages_record_refresh_check"]}}`, `{"completion":{"any_succeeded":["a + b"]}}`,
		`{"completion":["x"]}`, `{"completion":{"any_succeeded":"x"}}`,
		`{"roster":"required_tools_only"}`, `{"roster":null}`, `{"roster":""}`, `{"roster":"everything"}`, `{"roster":["required_tools_only"]}`,
	} {
		prompt := "TASK\n" + models.ExecutionRequirementsMarker + "\n" + body
		got, dispatchErr := parseExecutionRequirements(prompt)
		want, saveErr := models.ParseExecutionRequirements(prompt)
		if fmt.Sprint(dispatchErr) != fmt.Sprint(saveErr) {
			t.Fatalf("dispatch and save-time parsing disagree on %.60q: dispatch=%v save=%v", body, dispatchErr, saveErr)
		}
		if want == nil {
			if got != nil {
				t.Fatalf("%.60q: dispatch parsed %+v where the parser found nothing", body, got)
			}
			continue
		}
		if !reflect.DeepEqual(got.ExecutionRequirements, *want) {
			t.Fatalf("%.60q: dispatch = %+v, want the parsed declaration %+v", body, got.ExecutionRequirements, *want)
		}
	}
}
