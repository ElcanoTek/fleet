package agentcore

import (
	"strings"
	"testing"
)

// TestResolveMCPVariant_InjectsVariantClientEnv pins the cutlass mcp_loader
// parity contract: a named-account stdio variant carries
// MCP_VARIANT_CLIENT=<lower canonical account> in its subprocess env (the
// cutlass-family servers read it for variant-scoped identity requirements and
// client-facing labels like SSP fee-partner names), and the DEFAULT seat never
// carries it.
func TestResolveMCPVariant_InjectsVariantClientEnv(t *testing.T) {
	base := MCPServerBase{
		BaseEnv: map[string]string{"PUBMATIC_API_KEY": "default-key"},
		Command: "python",
		Args:    []string{"mcp/pubmatic_mcp.py"},
	}

	t.Run("default seat has no variant marker", func(t *testing.T) {
		_, env, err := resolveMCPVariant("pubmatic_mcp", base, "")
		if err != nil {
			t.Fatalf("default account: %v", err)
		}
		if _, ok := env[MCPVariantClientEnvVar]; ok {
			t.Errorf("default seat must not carry %s, got %q", MCPVariantClientEnvVar, env[MCPVariantClientEnvVar])
		}
	})

	t.Run("named account carries lowercased canonical name", func(t *testing.T) {
		t.Setenv("PUBMATIC_API_KEY_CLIENT_A", "client-a-key")
		// Hyphen spelling folds to the canonical underscore seat AND lowers.
		_, env, err := resolveMCPVariant("pubmatic_mcp", base, "Client-A")
		if err != nil {
			t.Fatalf("named account: %v", err)
		}
		if got := env[MCPVariantClientEnvVar]; got != "client_a" {
			t.Errorf("%s = %q, want client_a (lowercased canonical account)", MCPVariantClientEnvVar, got)
		}
	})
}

// TestResolveMCPVariant_IdentityRoutingGuard pins the inherited-routing-identity
// refusal (the cutlass mcp_loader guard): a named account whose overlay
// overrides SOME vars but leaves a non-empty identity-routing var (identity_env)
// on its default-seat value must be REFUSED — it would transact in the default
// client's seat under the named account's label.
func TestResolveMCPVariant_IdentityRoutingGuard(t *testing.T) {
	base := MCPServerBase{
		BaseEnv: map[string]string{
			"PUBMATIC_API_KEY":  "default-key",
			"PUBMATIC_OWNER_ID": "60067", // identity: routes whose seat transacts
		},
		Command:     "python",
		Args:        []string{"mcp/pubmatic_mcp.py"},
		IdentityEnv: []string{"PUBMATIC_OWNER_ID"},
	}

	t.Run("partial override refused", func(t *testing.T) {
		t.Setenv("PUBMATIC_API_KEY_ACME", "acme-key")
		// PUBMATIC_OWNER_ID_ACME deliberately unset.
		_, _, err := resolveMCPVariant("pubmatic_mcp", base, "acme")
		if err == nil {
			t.Fatal("partially-suffixed account must be refused (identity var inherited from default seat)")
		}
		if !strings.Contains(err.Error(), "PUBMATIC_OWNER_ID") {
			t.Errorf("refusal should name the inherited identity var, got: %v", err)
		}
	})

	t.Run("full override allowed", func(t *testing.T) {
		t.Setenv("PUBMATIC_API_KEY_ACME", "acme-key")
		t.Setenv("PUBMATIC_OWNER_ID_ACME", "70001")
		name, env, err := resolveMCPVariant("pubmatic_mcp", base, "acme")
		if err != nil {
			t.Fatalf("fully-suffixed account should spawn: %v", err)
		}
		if name != "pubmatic_mcp_acme" {
			t.Errorf("variant name = %q, want pubmatic_mcp_acme", name)
		}
		if env["PUBMATIC_OWNER_ID"] != "70001" {
			t.Errorf("PUBMATIC_OWNER_ID = %q, want the acme override", env["PUBMATIC_OWNER_ID"])
		}
	})

	t.Run("empty default identity value needs no override", func(t *testing.T) {
		// A deployment with no default seat identity (e.g. Zeta's no-default
		// stance) must not be blocked: nothing can be inherited from a blank.
		blankBase := base
		blankBase.BaseEnv = map[string]string{
			"PUBMATIC_API_KEY":  "default-key",
			"PUBMATIC_OWNER_ID": "",
		}
		t.Setenv("PUBMATIC_API_KEY_ACME", "acme-key")
		if _, _, err := resolveMCPVariant("pubmatic_mcp", blankBase, "acme"); err != nil {
			t.Fatalf("blank default identity value must not trip the guard: %v", err)
		}
	})
}
