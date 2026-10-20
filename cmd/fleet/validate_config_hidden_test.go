package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// validate-config fails on an interactive_hidden_tools member the install
// drops (blank, or a character no tool name has) and, when every server lists
// its tools, warns on one that matches none of them. A server without a
// tools: allowlist makes "matches nothing" unknowable, so it is not reported
// then.
func TestCheckAgentPolicy_InteractiveHiddenTools(t *testing.T) {
	load := func(body string) *clientconfig.Bundle {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		b, err := clientconfig.Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return b
	}
	listed := `
mcp_servers:
  - name: openx
    command: ox
    always: true
    tools: [ox_create_deal, ox_get_deal]
agent_policy:
  interactive_hidden_tools: [%s]
`
	res := checkAgentPolicy(load(fmt.Sprintf(listed, `ox_create_deal, "bad name"`)), nil)
	if res.Status != statusFail || !strings.Contains(res.Detail, `"bad name"`) || strings.Contains(res.Detail, `"ox_create_deal"`) {
		t.Fatalf("want a fail naming the bad name only, got %q: %s", res.Status, res.Detail)
	}
	// A member that matches no bundle tool may be meant for a per-user hosted
	// connector (Codex P2 on #1816): it warns, and never fails the check.
	res = checkAgentPolicy(load(fmt.Sprintf(listed, `ox_create_deal, ox_creat_deal, mcp_github_create_issue`)), nil)
	if res.Status != statusWarn || !strings.Contains(res.Detail, `"ox_creat_deal" matches no tool`) || !strings.Contains(res.Detail, `"mcp_github_create_issue"`) || strings.Contains(res.Detail, `"ox_create_deal"`) {
		t.Fatalf("want a warning naming the unmatched members only, got %q: %s", res.Status, res.Detail)
	}
	if res := checkAgentPolicy(load(fmt.Sprintf(listed, "create_deal, mcp_openx_ox_get_deal")), nil); res.Status != statusOK {
		t.Fatalf("matching members must pass, got %q: %s", res.Status, res.Detail)
	}
	unlisted := `
mcp_servers:
  - name: openx
    command: ox
    always: true
agent_policy:
  interactive_hidden_tools: [anything_at_all]
`
	if res := checkAgentPolicy(load(unlisted), nil); res.Status != statusOK {
		t.Fatalf("a server without a tools: list cannot prove a miss, got %s", res.Detail)
	}
}
