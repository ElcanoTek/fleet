package agent

import (
	"slices"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

// TestRenderMCPCatalog_SurfacesAccountSeats is the regression guard for #88: the
// model-facing roster lists each server's provisioned credential-account seats
// (from AccountVars via creds.AccountsFor) so the agent can discover valid
// `client`/account names for mcp_load_servers. A server whose AccountVars have
// no suffixed env reports no accounts.
func TestRenderMCPCatalog_SurfacesAccountSeats(t *testing.T) {
	t.Setenv("DEMO_TOKEN_CLIENTB", "secret-value") // provisions the "clientb" seat for DEMO_TOKEN
	a := &Agent{
		config: &config.Config{
			MCPServers: map[string]config.MCPServerConfig{
				"demo":    {Type: "stdio", Command: "python3", Enabled: true, AccountVars: []string{"DEMO_TOKEN"}},
				"plainsv": {Type: "stdio", Command: "python3", Enabled: true, AccountVars: []string{"PLAINSV_TOKEN"}}, // no suffixed env
			},
		},
		loadedServers: map[string]bool{},
	}
	out := a.renderMCPCatalog()
	if !strings.Contains(out, "demo — accounts: clientb") {
		t.Fatalf("expected demo's 'clientb' seat in the roster, got:\n%s", out)
	}
	if strings.Contains(out, "plainsv — accounts:") {
		t.Fatalf("plainsv has no suffixed env → should report no accounts, got:\n%s", out)
	}
}

// The loader's prose has always called client optional, but without omitempty
// fantasy advertised it as required and providers rejected the only valid call
// for HTTP servers (mcp_load_servers(names=["fast_io"])). Keep the generated
// schema aligned with the runtime contract.
func TestMCPLoadServers_ClientIsOptionalInSchema(t *testing.T) {
	a := &Agent{}
	loader := a.buildLoaderTools()[1]
	info := loader.Info()
	if !slices.Contains(info.Required, "names") {
		t.Fatalf("required fields = %v, want names", info.Required)
	}
	if slices.Contains(info.Required, "client") {
		t.Fatalf("required fields = %v; client must be optional", info.Required)
	}
}
