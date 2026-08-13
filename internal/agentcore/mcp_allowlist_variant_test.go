package agentcore

import (
	"reflect"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

func variantTool(server, name string) mcp.ServerTool {
	return mcp.ServerTool{ServerName: server, Tool: mcp.Tool{
		Name:        name,
		Description: name,
		InputSchema: map[string]interface{}{"type": "object"},
	}}
}

// TestGate2AllowlistGovernsAccountVariantSeats: Gate-2 allowlists are keyed by
// manifest server name, but a named-account seat registers (and appears in the
// scoped catalog) as "<server>_<account>" — the variant must still be filtered
// by its manifest server's entry, exactly like the default seat.
func TestGate2AllowlistGovernsAccountVariantSeats(t *testing.T) {
	catalog := []mcp.ServerTool{
		variantTool("srv", "read"),
		variantTool("srv", "purge"),
		variantTool("srv_clienta", "read"),
		variantTool("srv_clienta", "purge"),
	}
	allow := mcpAllowlist{"srv": {"read"}}
	registered, err := buildFantasyTools(nil, catalog, &fakeBroker{}, allow, passPolicy{}, nil, nil, toolBuildConfig{})
	if err != nil {
		t.Fatalf("buildFantasyTools: %v", err)
	}
	names := toolNamesOf(registered)
	for _, want := range []string{"mcp_srv_read", "mcp_srv_clienta_read"} {
		if !names[want] {
			t.Errorf("allowlisted tool %s was not registered", want)
		}
	}
	for _, blocked := range []string{"mcp_srv_purge", "mcp_srv_clienta_purge"} {
		if names[blocked] {
			t.Errorf("tool %s escaped the manifest server's Gate-2 allowlist", blocked)
		}
	}
}

func TestMCPAllowlistToolsFor(t *testing.T) {
	al := mcpAllowlist{
		"srv":         {"read"},
		"srv_special": {"write"},
	}
	cases := []struct {
		registered string
		want       []string
	}{
		{"srv", []string{"read"}},
		{"srv_special", []string{"write"}},         // exact key wins over the "srv" prefix
		{"srv_clienta", []string{"read"}},          // named-account seat of srv
		{"srv_special_clienta", []string{"write"}}, // longest manifest key wins
		{"srvette", nil},                           // shared prefix without the underscore boundary
		{"other", nil},                             // no entry = allow all
	}
	for _, tc := range cases {
		if got := al.toolsFor(tc.registered); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("toolsFor(%q) = %v, want %v", tc.registered, got, tc.want)
		}
	}
}
