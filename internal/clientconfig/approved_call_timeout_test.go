package clientconfig

import (
	"strings"
	"testing"
)

// TestApprovedCallTimeoutSeconds pins the per-server approved-card call
// budget: mcp_servers[].approved_call_timeout_seconds reaches
// Bundle.AgentPolicy() keyed by server name, an absent key contributes
// nothing (the engine's 60 s default applies), and the policy map is a fresh
// copy per call.
func TestApprovedCallTimeoutSeconds(t *testing.T) {
	dir := writeManifest(t, `
mcp_servers:
  - name: deals_mcp
    command: deals
    always: true
    approved_call_timeout_seconds: 300
  - name: plain_mcp
    command: plain
    always: true
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := b.AgentPolicy()
	if len(p.ApprovedCallTimeoutSeconds) != 1 || p.ApprovedCallTimeoutSeconds["deals_mcp"] != 300 {
		t.Fatalf("ApprovedCallTimeoutSeconds = %v, want only deals_mcp: 300", p.ApprovedCallTimeoutSeconds)
	}
	p.ApprovedCallTimeoutSeconds["deals_mcp"] = 1
	if got := b.AgentPolicy().ApprovedCallTimeoutSeconds["deals_mcp"]; got != 300 {
		t.Fatalf("AgentPolicy() must return a fresh map; the bundle now says %d", got)
	}
	// The two server budgets are independent declarations.
	if got := p.BatchSecondsPerDeal; got != nil {
		t.Fatalf("BatchSecondsPerDeal = %v, want nil: declaring an approved budget declares no batch pace", got)
	}

	plain := writeManifest(t, `
mcp_servers:
  - name: plain_mcp
    command: plain
    always: true
`)
	b, err = Load(plain)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := b.AgentPolicy().ApprovedCallTimeoutSeconds; got != nil {
		t.Fatalf("ApprovedCallTimeoutSeconds = %v, want nil when no server declares a budget", got)
	}
}

// TestApprovedCallTimeoutSecondsRejectsOutOfRange: a negative budget, or one
// above the 30-minute ceiling (most likely milliseconds typed as seconds),
// fails the load; 1800 itself loads; and the key is a server field, not an
// agent_policy one. A misspelled key fails the strict decoder, which is also
// exactly what a Fleet release that predates the key does with it, so a
// bundle must adopt the key only after this release is deployed.
func TestApprovedCallTimeoutSecondsRejectsOutOfRange(t *testing.T) {
	for _, v := range []string{"-1", "1801", "300000"} {
		dir := writeManifest(t, `
mcp_servers:
  - name: deals_mcp
    command: deals
    always: true
    approved_call_timeout_seconds: `+v+`
`)
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "approved_call_timeout_seconds") {
			t.Fatalf("approved_call_timeout_seconds: %s loaded (err=%v), want a load error naming the key", v, err)
		}
	}
	for _, v := range []string{"1", "1800"} {
		dir := writeManifest(t, `
mcp_servers:
  - name: deals_mcp
    command: deals
    always: true
    approved_call_timeout_seconds: `+v+`
`)
		if _, err := Load(dir); err != nil {
			t.Fatalf("approved_call_timeout_seconds: %s (inside the range) must load: %v", v, err)
		}
	}

	misplaced := writeManifest(t, `
agent_policy:
  approved_call_timeout_seconds:
    deals_mcp: 300
`)
	if _, err := Load(misplaced); err == nil {
		t.Fatal("agent_policy.approved_call_timeout_seconds loaded; the budget is declared on the server, and the strict decoder must refuse it here")
	}

	misspelled := writeManifest(t, `
mcp_servers:
  - name: deals_mcp
    command: deals
    always: true
    approved_call_timeout_secs: 300
`)
	if _, err := Load(misspelled); err == nil || !strings.Contains(err.Error(), "approved_call_timeout_secs") {
		t.Fatalf("an unknown server key loaded (err=%v); the strict decoder must name it", err)
	}
}

// TestPluginServerCannotDeclareApprovedCallTimeout: an Agent Plugin server's
// fleet extension is a closed allow-list, and the approved-call budget is not
// on it. The key is reported and ignored; the plugin still loads, and its
// server contributes no budget.
func TestPluginServerCannotDeclareApprovedCallTimeout(t *testing.T) {
	manifest := `{` + testPluginSchema + `, "name": "gov", "extensions": {"com.elcanotek.fleet": {
  "mcp_servers": {"srv": {"approved_call_timeout_seconds": 900}}
}}}`
	mcp := `{` + testMCPSchema + `, "mcpServers": {"srv": {"type": "stdio", "command": "python3"}}}`
	dir := writePluginBundle(t, "", pluginFixture{dir: "g", manifest: manifest, mcp: mcp})
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(b.Plugins) != 1 {
		t.Fatalf("the extension must never reject the plugin: %+v %v", b.Plugins, b.PluginProblems())
	}
	if !containsProblem(b.PluginProblems(), `unknown key "approved_call_timeout_seconds" ignored`) {
		t.Fatalf("the key must be reported as unknown for a plugin server: %v", b.PluginProblems())
	}
	if got := b.AgentPolicy().ApprovedCallTimeoutSeconds; got != nil {
		t.Fatalf("ApprovedCallTimeoutSeconds = %v, want nil: a plugin server cannot raise its own approved-call budget", got)
	}
}
