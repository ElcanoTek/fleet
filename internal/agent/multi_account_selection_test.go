package agent

import (
	"reflect"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

func TestInteractiveRunSelectionSeveralAccounts(t *testing.T) {
	t.Parallel()
	got := interactiveRunSelection(
		[]string{"email", "ix_mcp"},
		map[string]string{"ix_mcp": "twc"},
		// "twc" again is the first account repeated; "TWC" is a different
		// account name, and the binder decides what to do with that one.
		map[string][]string{"ix_mcp": {"", "twc", "arena"}, "not_enabled": {"x"}},
	)
	want := agentcore.MCPSelection{
		{Server: "email"},
		{Server: "ix_mcp", Account: "twc"},
		{Server: "ix_mcp"},
		{Server: "ix_mcp", Account: "arena"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selection = %#v, want %#v", got, want)
	}
	// No extras: the selection is what it always was.
	got = interactiveRunSelection([]string{"email", "ix_mcp"}, map[string]string{"ix_mcp": "twc"}, nil)
	if want := (agentcore.MCPSelection{{Server: "email"}, {Server: "ix_mcp", Account: "twc"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("single selection = %#v, want %#v", got, want)
	}
}
