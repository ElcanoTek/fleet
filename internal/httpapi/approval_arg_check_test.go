package httpapi

import (
	"errors"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

func argCheckCatalog() []mcp.ServerTool {
	return []mcp.ServerTool{
		{ServerName: "fast_io", Tool: mcp.Tool{Name: "storage", InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":  map[string]any{"type": "string", "enum": []any{"describe", "list", "details", "search", "read-content"}},
				"node_id": map[string]any{"type": "string"},
				"limit":   map[string]any{"type": "integer"},
			},
			"required": []any{"action"},
		}}},
		// Discriminated by `action` at the root, as zod's discriminatedUnion emits.
		{ServerName: "deals", Tool: mcp.Tool{Name: "plan", InputSchema: map[string]any{
			"anyOf": []any{
				map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"const": "preview"}}},
				map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"const": "execute"}, "mode": map[string]any{"anyOf": []any{
					map[string]any{"const": "dry"}, map[string]any{"enum": []any{"live"}},
				}}}},
			},
		}}},
		// Nothing here is a closed set of strings.
		{ServerName: "open", Tool: mcp.Tool{Name: "write", InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind":  map[string]any{"$ref": "#/$defs/Kind"},
				"level": map[string]any{"enum": []any{"low", 2}},
				"mode":  map[string]any{"anyOf": []any{map[string]any{"const": "a"}, map[string]any{"type": "string"}}},
				"name":  map[string]any{"type": "string"},
			},
		}}},
	}
}

// A gated MCP call whose `action` the tool's schema does not list is refused
// before a card is staged. In production an Autonomous chat showed a card for
// Fast.io storage action "list-workspaces", which does not exist; the user
// approved it minutes later and the server rejected the call.
func TestStageRefusesAnMCPArgumentOutsideItsSchemaEnum(t *testing.T) {
	t.Parallel()

	// A stager with a catalog but no store or sink: reaching staging would
	// panic, so a clean error proves the refusal came first.
	a := &approvalStager{mcpCatalog: argCheckCatalog()}
	id, err := a.Stage("mcp_fast_io_storage", "call-1", `{"action":"list-workspaces"}`)
	if err == nil || id != "" {
		t.Fatalf("staged an action the schema does not list (id=%q)", id)
	}
	var refused *agentcore.StageRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("the refusal is not a StageRefusedError, so the gate would report it as APPROVAL_REQUIRED: %v", err)
	}
	for _, want := range []string{`"list-workspaces" is not a valid action for mcp_fast_io_storage`, `"describe", "list", "details", "search", "read-content"`, "Nothing was staged"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}

func TestPrevalidateStagedMCPArguments(t *testing.T) {
	t.Parallel()

	catalog := argCheckCatalog()
	for _, tc := range []struct {
		name, tool, raw, want string
	}{
		{"enum mismatch", "mcp_fast_io_storage", `{"action":"delete-everything","node_id":"n"}`, `"delete-everything" is not a valid action`},
		{"case matters", "mcp_fast_io_storage", `{"action":"List"}`, `"List" is not a valid action`},
		{"root union, unknown discriminator", "mcp_deals_plan", `{"action":"apply"}`, `expected one of: "preview", "execute"`},
		{"union of const and enum on one branch only", "mcp_deals_plan", `{"action":"execute","mode":"wet"}`, ""},
	} {
		err := prevalidateStagedMCPArguments(catalog, tc.tool, tc.raw)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: refused %s: %v", tc.name, tc.raw, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v lacks %q", tc.name, err, tc.want)
		}
	}

	// Everything the schema leaves open, and everything that is not this
	// check's to judge, passes.
	for _, tc := range []struct{ tool, raw string }{
		{"mcp_fast_io_storage", `{"action":"list","node_id":"anything","limit":5}`},
		{"mcp_fast_io_storage", `{"action":7}`},              // wrong type: the server's to report
		{"mcp_fast_io_storage", `{"node_id":"n"}`},           // missing required: the server's to report
		{"mcp_fast_io_storage", `not json`},                  // left to the card and the executor
		{"mcp_fast_io_storage", `{"undeclared":"whatever"}`}, // no declaration
		{"mcp_deals_plan", `{"action":"execute","mode":"live"}`},
		{"mcp_open_write", `{"kind":"x","level":"high","mode":"zzz","name":"n"}`},
		{"mcp_unknown_tool", `{"action":"nope"}`}, // not in the catalog
		{"bash", `{"action":"nope"}`},             // not an MCP tool
	} {
		if err := prevalidateStagedMCPArguments(catalog, tc.tool, tc.raw); err != nil {
			t.Errorf("%s %s refused: %v", tc.tool, tc.raw, err)
		}
	}
}

func TestListAllowedValuesIsBounded(t *testing.T) {
	t.Parallel()

	values := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		values = append(values, strings.Repeat("v", i+1))
	}
	got := listAllowedValues(append(values, values[0]))
	if !strings.HasSuffix(got, ", and 20 more") || strings.Count(got, `"`) != 2*maxListedAllowedValues {
		t.Fatalf("listing not bounded to %d values: %q", maxListedAllowedValues, got)
	}
}
