package agentcore_test

// End-to-end: a REAL bundle manifest carrying agent_policy.email_last_tools /
// settleable_create_tools, loaded by clientconfig and installed the way the
// boot paths (cmd/fleet, taskrun) install it, activates the email-last gate in
// a scheduled run. Codex P1 on #1710: Bundle.AgentPolicy() never copied the
// two lists, so the gate was inert for every real bundle while the unit tests
// (which call ConfigureAgentPolicy directly) stayed green.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

const (
	bundleCreate    = "mcp_ix_execute_deal_from_prompt_inputs"
	bundleEmail     = "mcp_sendgrid_send_email"
	bundleEmailArgs = `{"to":["trader@example.com"],"subject":"Batch results","body":"sheet"}`
)

// installBundlePolicy loads manifest from a temp bundle dir and installs its
// agent policy with the same field mapping as cmd/fleet and taskrun.
func installBundlePolicy(t *testing.T, manifest string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	bp := b.AgentPolicy()
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		ParallelSafeTools:            bp.ParallelSafeTools,
		CriticalToolSuffixes:         bp.CriticalToolSuffixes,
		CriticalToolSubstitutes:      bp.CriticalToolSubstitutes,
		CriticalToolAliases:          bp.CriticalToolAliases,
		EmailLastToolSuffixes:        bp.EmailLastTools,
		SettleableCreateToolSuffixes: bp.SettleableCreateTools,
	})
	t.Cleanup(agentcore.RestoreTestFixturePolicy)
}

// auditedBatch runs a scheduled policy through the real confirm_audit tool
// declaring two creates and the summary email.
func auditedBatch(t *testing.T) *agentcore.ScheduledPolicy {
	t.Helper()
	p := agentcore.NewScheduledPolicy(&agentcore.LogSession{}, 100, 0, 0)
	raw := `{"success":true,"reasoning":"checked","artifacts_checked":["brief.csv"],` +
		`"workflow_sections_checked":["build"],"send_contract_checked":true,"attachments_checked":[],` +
		`"remaining_risks":[],"critical_actions":[` +
		`{"tool":"` + bundleCreate + `"},{"tool":"` + bundleCreate + `"},{"tool":"` + bundleEmail + `"}]}`
	resp, err := agentcore.ConfirmAuditToolForTest(p).Run(context.Background(),
		fantasy.ToolCall{ID: "audit", Name: "confirm_audit", Input: raw})
	if err != nil || resp.IsError {
		t.Fatalf("confirm_audit refused: err=%v resp=%s", err, resp.Content)
	}
	return p
}

func run(t *testing.T, p *agentcore.ScheduledPolicy, tool, args, result string) (bool, string) {
	t.Helper()
	blocked, msg := p.BeforeToolCall(tool, "call", args)
	if !blocked {
		p.RecordToolResult(tool, args, result, true)
	}
	return blocked, msg
}

func TestEmailLast_BundleManifestActivatesGate(t *testing.T) {
	installBundlePolicy(t, `
agent_policy:
  critical_tools: [execute_deal_from_prompt_inputs, update_deal]
  email_last_tools: [update_deal]
  settleable_create_tools: [execute_deal_from_prompt_inputs]
`)
	p := auditedBatch(t)
	if blocked, msg := run(t, p, bundleCreate, `{"name":"A"}`, `{"success":true,"deal_id":"1"}`); blocked {
		t.Fatalf("create A must run: %s", msg)
	}
	// B has not run: the summary email waits for it.
	if blocked, msg := run(t, p, bundleEmail, bundleEmailArgs, `{"status_code":202}`); !blocked || !strings.Contains(msg, "still unsettled") {
		t.Fatalf("email must wait for the unsettled create, got blocked=%v msg=%s", blocked, msg)
	}
	// B fails definitively: settled, so the email goes and finish is allowed.
	if blocked, msg := run(t, p, bundleCreate, `{"name":"B"}`, `{"success":false,"error":"HTTP 403 not authorized"}`); blocked {
		t.Fatalf("create B must run: %s", msg)
	}
	if blocked, msg := run(t, p, bundleEmail, bundleEmailArgs, `{"status_code":202}`); blocked {
		t.Fatalf("email must go once B settled: %s", msg)
	}
	// No create may follow the summary email.
	if blocked, msg := run(t, p, bundleCreate, `{"name":"B"}`, `{"success":true}`); !blocked || !strings.Contains(msg, "summary email has already been sent") {
		t.Fatalf("a create after the summary email must be refused, got blocked=%v msg=%s", blocked, msg)
	}
}

// Control: the same run under a manifest without the two keys keeps the old
// behavior (the email is not ordered), so the test above proves the keys.
func TestEmailLast_BundleManifestWithoutKeysIsInert(t *testing.T) {
	installBundlePolicy(t, `
agent_policy:
  critical_tools: [execute_deal_from_prompt_inputs, update_deal]
`)
	p := auditedBatch(t)
	if blocked, msg := run(t, p, bundleEmail, bundleEmailArgs, `{"status_code":202}`); blocked {
		t.Fatalf("without email_last_tools the email is not ordered, got blocked: %s", msg)
	}
}
