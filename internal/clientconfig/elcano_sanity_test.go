package clientconfig

import (
	"os"
	"testing"
)

// TestElcanoBundleLoads is a SANITY check (skipped unless /root/elcano-config
// exists) asserting the real Elcano bundle parses and its MCP catalog gates
// the expected DSP servers on when their credentials are present. It is not a
// fixture the generic fleet ships — it validates the pluggable contract end to
// end against a real client bundle.
func TestElcanoBundleLoads(t *testing.T) {
	const dir = "/root/elcano-config"
	if _, err := os.Stat(dir); err != nil {
		t.Skip("elcano-config bundle not present; skipping sanity check")
	}
	t.Setenv("XANDR_USERNAME", "u")
	t.Setenv("XANDR_PASSWORD", "p")
	t.Setenv("OPENX_API_KEY", "k")
	t.Setenv("MAGNITE_ACCESS_KEY", "a")
	t.Setenv("MAGNITE_SECRET_KEY", "s")
	t.Setenv("SENDGRID_API_KEY", "sg")

	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load elcano bundle: %v", err)
	}
	if b.Branding.AppName != "Elcano" {
		t.Errorf("AppName = %q, want Elcano", b.Branding.AppName)
	}
	if len(b.MCPCatalog) == 0 {
		t.Fatal("elcano MCP catalog should be non-empty")
	}
	cfgs := b.MCPServerConfigs()
	// deal_sheet is always-on; xandr/openx_mcp/magnite_mcp/sendgrid gated on the
	// creds set above.
	for _, want := range []string{"deal_sheet", "xandr_mcp", "openx_mcp", "magnite_mcp", "sendgrid"} {
		if _, ok := cfgs[want]; !ok {
			t.Errorf("expected enabled server %q in catalog (have: %v)", want, keys(cfgs))
		}
	}
	// xandr env resolved from process env.
	if x := cfgs["xandr_mcp"]; x.Env["XANDR_USERNAME"] != "u" {
		t.Errorf("xandr XANDR_USERNAME = %q, want u", x.Env["XANDR_USERNAME"])
	}
	// agent policy ported.
	if len(b.AgentPolicyConfig.ParallelSafeTools) == 0 {
		t.Error("expected non-empty parallel-safe tools in elcano agent_policy")
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
