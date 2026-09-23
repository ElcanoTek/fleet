package scheduledrun

import (
	"reflect"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// The dispatch-level Gate-2 allowlist of a task (taskRosterAllowlist, #1603).

func rosterAllowlistRunner() *Runner {
	return &Runner{mcpServerInventory: func() map[string]TaskMCPServerInfo {
		return map[string]TaskMCPServerInfo{
			"pages":   {ToolAllowlist: []string{"get_page_data", "update_page_data_upload", "patch_page"}},
			"fast_io": {},
		}
	}}
}

// parsedRequirements reads a declaration through the dispatch parser, the
// shape a real run sees.
func parsedRequirements(t *testing.T, body string) *executionRequirements {
	t.Helper()
	req, err := parseExecutionRequirements(models.ExecutionRequirementsMarker + "\n" + body)
	if err != nil || req == nil {
		t.Fatalf("parse %s: %+v, %v", body, req, err)
	}
	return req
}

func remoteCRMOverlay() *agent.RemoteMCPOverlay {
	return &agent.RemoteMCPOverlay{Catalog: []mcp.ServerTool{
		{ServerName: "remote_crm", Tool: mcp.Tool{Name: "search"}},
		{ServerName: "remote_crm", Tool: mcp.Tool{Name: "delete_contact"}},
		{ServerName: "remote_wiki", Tool: mcp.Tool{Name: "read_page"}},
	}}
}

// Without the roster key a task's allowlist IS the manifest's, byte for byte —
// whatever its requirements declare and whatever catalog or remote overlay the
// run binds. Only the opt-in narrows.
func TestTaskRosterAllowlistWithoutTheKeyIsTheManifestAllowlist(t *testing.T) {
	r := rosterAllowlistRunner()
	want := r.taskMCPToolAllowlist()
	req := parsedRequirements(t, `{"required_tools":["mcp_pages_get_page_data","mcp_remote_crm_search"]}`)
	binding := taskMCPBinding{catalog: pagesCatalog()}
	for _, tc := range []struct {
		name    string
		req     *executionRequirements
		overlay *agent.RemoteMCPOverlay
	}{
		{"no requirements", nil, nil},
		{"requirements without a roster", req, nil},
		{"requirements without a roster, remote overlay", req, remoteCRMOverlay()},
	} {
		if got := r.taskRosterAllowlist(tc.req, "", binding, tc.overlay); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: allowlist = %v, want the manifest's %v", tc.name, got, want)
		}
	}
}

// A narrowed roster covers the owner's remote overlay too: an overlay server
// whose tool required_tools names keeps exactly that tool, and one it names
// nothing of is denied explicitly, the same as a dispatch-catalog server.
// Without the overlay in the narrowed catalog its servers would have no entry
// at all and register nothing, although checkTools accepted the required tool.
func TestTaskRosterAllowlistNarrowsTheRemoteOverlay(t *testing.T) {
	r := rosterAllowlistRunner()
	req := parsedRequirements(t, `{"required_tools":["mcp_pages_get_page_data","mcp_remote_crm_search"],"roster":"required_tools_only"}`)
	got := r.taskRosterAllowlist(req, rosterRequiredToolsOnly, taskMCPBinding{catalog: pagesCatalog()}, remoteCRMOverlay())
	deny := []string{rosterNarrowingDeniesAll}
	want := agentcore.MCPAllowlist{
		"pages":          {"get_page_data"},
		"pages_acct":     deny,
		"fast_io":        deny,
		"fastio_helpers": deny,
		"remote_crm":     {"search"},
		"remote_wiki":    deny,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("narrowed allowlist = %v, want %v", got, want)
	}

	// The narrowing still intersects with the manifest allowlist: a required
	// tool the manifest denies stays denied.
	denied := parsedRequirements(t, `{"required_tools":["mcp_pages_deploy_page_upload"]}`)
	if got := r.taskRosterAllowlist(denied, rosterRequiredToolsOnly, taskMCPBinding{catalog: pagesCatalog()}, nil); !reflect.DeepEqual(got["pages"], deny) {
		t.Fatalf("a tool the manifest denies was narrowed in: pages = %v", got["pages"])
	}
}
