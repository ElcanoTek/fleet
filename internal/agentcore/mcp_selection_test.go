package agentcore

import (
	"reflect"
	"testing"
)

// TestRunConfig_MCPBinding_DefaultAccount asserts account "" → base env, no
// overlay, registered under the bare server name (ApplyClientSuffix survived the
// merge into agentcore).
func TestRunConfig_MCPBinding_DefaultAccount(t *testing.T) {
	base := MCPServerBase{
		BaseEnv: map[string]string{"XANDR_API_KEY": "default-key", "XANDR_SEAT": "elcano"},
		Command: "python",
		Args:    []string{"mcp/xandr_mcp.py"},
	}

	name, env, err := resolveMCPVariant("xandr", base, "")
	if err != nil {
		t.Fatalf("default account should not error: %v", err)
	}
	if name != "xandr" {
		t.Errorf("default account name = %q, want xandr", name)
	}
	if !reflect.DeepEqual(env, base.BaseEnv) {
		t.Errorf("default account env = %v, want base env %v", env, base.BaseEnv)
	}
}

// TestRunConfig_MCPBinding_NamedAccount asserts a named account with
// <VAR>_<ACCOUNT> overrides set → ApplyClientSuffix overlay, registered under
// server_account; and that a named account with ZERO overrides is REFUSED.
func TestRunConfig_MCPBinding_NamedAccount(t *testing.T) {
	base := MCPServerBase{
		BaseEnv: map[string]string{"XANDR_API_KEY": "default-key", "XANDR_SEAT": "elcano"},
		Command: "python",
		Args:    []string{"mcp/xandr_mcp.py"},
	}

	t.Run("overlay applied", func(t *testing.T) {
		// Provision client_a's suffixed credential in the process env.
		t.Setenv("XANDR_API_KEY_CLIENT_A", "client-a-key")

		name, env, err := resolveMCPVariant("xandr", base, "client_a")
		if err != nil {
			t.Fatalf("named account with overrides should not error: %v", err)
		}
		if name != "xandr_client_a" {
			t.Errorf("variant name = %q, want xandr_client_a", name)
		}
		if env["XANDR_API_KEY"] != "client-a-key" {
			t.Errorf("XANDR_API_KEY = %q, want the client_a override", env["XANDR_API_KEY"])
		}
		// Non-overridden vars retain the default seat's value.
		if env["XANDR_SEAT"] != "elcano" {
			t.Errorf("XANDR_SEAT = %q, want elcano (no override → base value)", env["XANDR_SEAT"])
		}
	})

	t.Run("zero-override named account refused", func(t *testing.T) {
		// No XANDR_*_CLIENT_B set anywhere → binding must REFUSE rather than
		// silently inject the default seat under the client_b label.
		name, env, err := resolveMCPVariant("xandr", base, "client_b")
		if err == nil {
			t.Fatalf("named account with zero overrides must be refused, got name=%q env=%v", name, env)
		}
		if !contains(err.Error(), "refusing to spawn") {
			t.Errorf("refusal error should explain the refusal, got: %v", err)
		}
	})

	t.Run("hyphen account folds to the underscore seat (#146)", func(t *testing.T) {
		// The seat is provisioned with the underscore spelling; selecting it with
		// the hyphen spelling must resolve to the SAME seat and register under the
		// folded name, not fork a second.
		t.Setenv("XANDR_API_KEY_CLIENT_A", "client-a-key")

		name, env, err := resolveMCPVariant("xandr", base, "client-a")
		if err != nil {
			t.Fatalf("hyphen account with an underscore-suffixed override should not error: %v", err)
		}
		if name != "xandr_client_a" {
			t.Errorf("variant name = %q, want xandr_client_a (separator folded)", name)
		}
		if env["XANDR_API_KEY"] != "client-a-key" {
			t.Errorf("XANDR_API_KEY = %q, want the client_a override (hyphen resolved to the _CLIENT_A seat)", env["XANDR_API_KEY"])
		}
	})
}

// TestMCPBinding_HTTPRejectsAccountVariant asserts HTTP (fast_io) servers reject
// account variants.
func TestMCPBinding_HTTPRejectsAccountVariant(t *testing.T) {
	base := MCPServerBase{HTTPURL: "https://fast.io/mcp"}

	if _, _, err := resolveMCPVariant("fast_io", base, ""); err != nil {
		t.Fatalf("HTTP server with default account should not error: %v", err)
	}
	if _, _, err := resolveMCPVariant("fast_io", base, "client_a"); err == nil {
		t.Fatal("HTTP server with named account should be rejected")
	}
}

// TestMCPSelection_OptInSet asserts the selection reduces to a server-name set
// (the Gate-1 opt-in both modes feed) regardless of account.
func TestMCPSelection_OptInSet(t *testing.T) {
	sel := MCPSelection{
		{Server: "xandr", Account: "client_a"},
		{Server: "magnite"},
		{Server: "", Account: "ignored"},
	}
	got := sel.OptInSet()
	want := map[string]bool{"xandr": true, "magnite": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OptInSet = %v, want %v", got, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
