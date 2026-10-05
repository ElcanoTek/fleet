package scheduledrun

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// The completion clause (#1602): parsed leniently like the rest of the
// declaration, validated like required_tools, and resolved against the roster
// at dispatch.

func TestExecutionRequirementsCompletionClause(t *testing.T) {
	req, err := parseExecutionRequirements(models.ExecutionRequirementsMarker + "\n" +
		`{"mcp_servers":["pages"],"required_tools":["mcp_pages_get_page_data","mcp_pages_record_refresh_check","mcp_pages_update_page_data_upload"],` +
		`"completion":{"any_succeeded":["mcp_pages_update_page_data","update_page_data_upload","mcp_pages_record_refresh_check","finish_note"],"future_key":true}}`)
	if err != nil || req == nil || req.Completion == nil || len(req.Completion.AnySucceeded) != 4 {
		t.Fatalf("parse: %+v %v", req, err)
	}
	catalog := []mcp.ServerTool{
		{ServerName: "pages", Tool: mcp.Tool{Name: "get_page_data"}},
		{ServerName: "pages", Tool: mcp.Tool{Name: "record_refresh_check"}},
		{ServerName: "pages", Tool: mcp.Tool{Name: "update_page_data"}},
		{ServerName: "pages", Tool: mcp.Tool{Name: "update_page_data_upload"}},
	}
	native := []fantasy.AgentTool{namedNativeTool("finish_note")}
	if err := req.checkTools(catalog, native); err != nil {
		t.Fatal(err)
	}
	got := req.resolveCompletion(catalog, native)
	want := []string{"finish_note", "mcp_pages_record_refresh_check", "mcp_pages_update_page_data", "mcp_pages_update_page_data_upload"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("resolved completion = %v, want %v (full, bare and native names)", got, want)
	}

	// A completion tool the roster cannot call fails dispatch with the same
	// actionable roster error as an unavailable required tool.
	if err := req.checkTools(catalog[:3], native); err == nil ||
		!strings.Contains(err.Error(), "completion tool update_page_data_upload") ||
		!strings.Contains(err.Error(), "check selected servers, tool permissions and connected accounts") {
		t.Fatalf("unknown completion tool not diagnosed: %v", err)
	}
}

func TestExecutionRequirementsCompletionAbsentOrEmptyDeclaresNothing(t *testing.T) {
	for _, body := range []string{
		`{"mcp_servers":["pages"]}`,
		`{"completion":null}`,
		`{"completion":{}}`,
		`{"completion":{"any_succeeded":[]}}`,
		`{"completion":{"all_succeeded":["x"]}}`,
	} {
		req, err := parseExecutionRequirements(models.ExecutionRequirementsMarker + "\n" + body)
		if err != nil || req == nil {
			t.Fatalf("%s: %v", body, err)
		}
		if got := req.resolveCompletion(nil, nil); got != nil {
			t.Fatalf("%s: declared a predicate %v", body, got)
		}
	}
	var none *executionRequirements
	if none.completionTools() != nil {
		t.Fatal("a prompt with no requirements must declare no predicate")
	}
}

// A malformed clause fails closed, like every other malformed requirement.
func TestExecutionRequirementsCompletionMalformedFailsClosed(t *testing.T) {
	for _, body := range []string{
		`{"completion":{"any_succeeded":["bad name!"]}}`,
		`{"completion":{"any_succeeded":"mcp_pages_record_refresh_check"}}`,
		`{"completion":{"any_succeeded":[1]}}`,
		`{"completion":"mcp_pages_record_refresh_check"}`,
		`{"completion":["mcp_pages_record_refresh_check"]}`,
		`{"completion":{"any_succeeded":[` + strings.Repeat(`"t",`, 200) + `"t"]}}`,
	} {
		if _, err := parseExecutionRequirements(models.ExecutionRequirementsMarker + "\n" + body); err == nil {
			t.Fatalf("malformed completion clause accepted: %.80s", body)
		}
	}
}

type namedNativeTool string

func (n namedNativeTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: string(n)} }
func (n namedNativeTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextResponse("ok"), nil
}
func (n namedNativeTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (n namedNativeTool) SetProviderOptions(_ fantasy.ProviderOptions) {}

// The blocked rule (completion.blocked_when) resolves its tool against the
// roster like any_succeeded and reaches the agent whole; a run without one
// passes nil.
func TestExecutionRequirementsBlockedWhenResolves(t *testing.T) {
	req, err := parseExecutionRequirements(models.ExecutionRequirementsMarker + "\n" +
		`{"completion":{"any_succeeded":["record_refresh_check","mcp_pages_update_page_data"],` +
		`"blocked_when":{"tool":"record_refresh_check","argument":"outcome","in":["blocked","source_unreachable"],"detail_argument":"detail"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	catalog := []mcp.ServerTool{
		{ServerName: "pages", Tool: mcp.Tool{Name: "record_refresh_check"}},
		{ServerName: "pages", Tool: mcp.Tool{Name: "update_page_data"}},
	}
	if err := req.checkTools(catalog, nil); err != nil {
		t.Fatal(err)
	}
	req.completionRoster = req.resolveCompletion(catalog, nil)
	req.blockedRoster = req.resolveBlockedWhen(catalog, nil)
	got := req.completionBlockedWhen()
	if got == nil || fmt.Sprint(got.Tools) != "[mcp_pages_record_refresh_check]" || got.Argument != "outcome" ||
		fmt.Sprint(got.In) != "[blocked source_unreachable]" || got.DetailArgument != "detail" {
		t.Fatalf("resolved rule = %+v", got)
	}
	none, err := parseExecutionRequirements(models.ExecutionRequirementsMarker + "\n" + `{"completion":{"any_succeeded":["record_refresh_check"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	none.blockedRoster = none.resolveBlockedWhen(catalog, nil)
	if none.completionBlockedWhen() != nil {
		t.Fatal("no blocked_when must reach the agent as nil")
	}
}
