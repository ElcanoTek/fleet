package scheduledrun

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// The roster opt-in (#1603): "required_tools_only" narrows the MCP roster to
// required_tools; anything else fails dispatch.

func TestParseRequirementsRoster(t *testing.T) {
	for _, tc := range []struct {
		prompt, want string
		wantErr      bool
	}{
		{"An ordinary prompt", "", false},
		{executionRequirementsMarker + "\n" + `{"required_tools":["mcp_pages_get_page_data"]}`, "", false},
		{executionRequirementsMarker + "\n" + `{"roster":null}`, "", false},
		{executionRequirementsMarker + "\n" + `{"required_tools":["mcp_pages_get_page_data"],"roster":"required_tools_only"}`, rosterRequiredToolsOnly, false},
		{executionRequirementsMarker + "\n" + `{"roster":"everything"}`, "", true},
		{executionRequirementsMarker + "\n" + `{"roster":["required_tools_only"]}`, "", true},
	} {
		got, err := parseRequirementsRoster(tc.prompt)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Fatalf("parseRequirementsRoster(%.60q) = %q, %v; want %q, err=%t", tc.prompt, got, err, tc.want, tc.wantErr)
		}
		if tc.wantErr && !strings.Contains(err.Error(), "execution requirements:") {
			t.Fatalf("roster error must read like the other requirement errors: %v", err)
		}
	}
}

// An unknown roster fails the run at dispatch, before model setup (no manager
// is installed here: reaching model setup would panic).
func TestRunWorkerRejectsAnUnknownRosterBeforeModelSetup(t *testing.T) {
	task := &models.Task{Prompt: executionRequirementsMarker + "\n" + `{"roster":"all_tools"}`}
	session, _, _, err := (&Runner{}).runWorker(context.Background(), task, "", nil, "")
	if err == nil || !strings.Contains(err.Error(), `unknown roster "all_tools"`) || session != nil {
		t.Fatalf("late/missing roster preflight: %v %v", session, err)
	}
}

func pagesCatalog() []mcp.ServerTool {
	var out []mcp.ServerTool
	for _, server := range []string{"pages", "pages_acct"} {
		for _, tool := range []string{"get_page_data", "record_refresh_check", "update_page_data_upload", "deploy_page_upload", "patch_page"} {
			out = append(out, mcp.ServerTool{ServerName: server, Tool: mcp.Tool{Name: tool}})
		}
	}
	return append(out,
		mcp.ServerTool{ServerName: "fast_io", Tool: mcp.Tool{Name: "upload"}},
		mcp.ServerTool{ServerName: "fastio_helpers", Tool: mcp.Tool{Name: "stage"}})
}

func TestNarrowedAllowlist(t *testing.T) {
	req := &executionRequirements{Tools: []string{
		"mcp_pages_get_page_data", "mcp_pages_record_refresh_check", "mcp_pages_update_page_data_upload",
		"run_python", // native: not in the MCP roster, untouched
	}}
	got := req.narrowedAllowlist(pagesCatalog(), nil)
	if fmt.Sprint(got) != fmt.Sprint(agentcore.MCPAllowlist{"pages": {"get_page_data", "record_refresh_check", "update_page_data_upload"}}) {
		t.Fatalf("narrowed = %v, want exactly the required pages tools (fast_io, fastio_helpers and the layout tools gone)", got)
	}
	// The seat is not named in its own full form, so it gets no entry of its
	// own: Gate-2's keying rule falls back to the base server's entry.
	if list := agentcore.AllowlistToolsFor(got, "pages_acct"); fmt.Sprint(list) != fmt.Sprint(got["pages"]) {
		t.Fatalf("seat narrows to %v, want the base server's %v", list, got["pages"])
	}

	// A bare name narrows every server that has it, the seat's own entry included.
	bare := (&executionRequirements{Tools: []string{"get_page_data"}}).narrowedAllowlist(pagesCatalog(), nil)
	if fmt.Sprint(bare) != fmt.Sprint(agentcore.MCPAllowlist{"pages": {"get_page_data"}, "pages_acct": {"get_page_data"}}) {
		t.Fatalf("bare-name narrowing = %v", bare)
	}

	// Narrowing only subtracts: a required tool the base Gate-2 already
	// removes stays removed.
	base := agentcore.MCPAllowlist{"pages": {"get_page_data", "patch_page"}}
	if got := req.narrowedAllowlist(pagesCatalog(), base); fmt.Sprint(got) != fmt.Sprint(agentcore.MCPAllowlist{"pages": {"get_page_data"}}) {
		t.Fatalf("narrowing widened the base allowlist: %v", got)
	}

	// No required MCP tool at all: a non-nil, empty (exhaustive) allowlist.
	if got := (&executionRequirements{Tools: []string{"run_python"}}).narrowedAllowlist(pagesCatalog(), nil); got == nil || len(got) != 0 {
		t.Fatalf("native-only required_tools must narrow to no MCP tool, got %v", got)
	}
}
