package clientconfig

import (
	"strings"
	"testing"
)

// TestBatchSecondsPerDeal pins the per-server deal_ids batch pace: a server's
// mcp_servers[].batch_seconds_per_deal reaches Bundle.AgentPolicy() keyed by
// server name, an absent key contributes nothing (the engine default applies),
// and the policy map is a fresh copy per call.
func TestBatchSecondsPerDeal(t *testing.T) {
	dir := writeManifest(t, `
mcp_servers:
  - name: paced_mcp
    command: paced
    always: true
    batch_seconds_per_deal: 45
  - name: plain_mcp
    command: plain
    always: true
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := b.AgentPolicy()
	if len(p.BatchSecondsPerDeal) != 1 || p.BatchSecondsPerDeal["paced_mcp"] != 45 {
		t.Fatalf("BatchSecondsPerDeal = %v, want only paced_mcp: 45", p.BatchSecondsPerDeal)
	}
	p.BatchSecondsPerDeal["paced_mcp"] = 1
	if got := b.AgentPolicy().BatchSecondsPerDeal["paced_mcp"]; got != 45 {
		t.Fatalf("AgentPolicy() must return a fresh map; the bundle now says %d", got)
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
	if got := b.AgentPolicy().BatchSecondsPerDeal; got != nil {
		t.Fatalf("BatchSecondsPerDeal = %v, want nil when no server declares a pace", got)
	}
}

// TestBatchSecondsPerDealRejectsOutOfRange: a negative pace, or one above the
// 30-minute whole-call cap (most likely milliseconds typed as seconds), fails
// the load; and the key is a server field, not an agent_policy one.
func TestBatchSecondsPerDealRejectsOutOfRange(t *testing.T) {
	for _, v := range []string{"-1", "1801", "45000"} {
		dir := writeManifest(t, `
mcp_servers:
  - name: paced_mcp
    command: paced
    always: true
    batch_seconds_per_deal: `+v+`
`)
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "batch_seconds_per_deal") {
			t.Fatalf("batch_seconds_per_deal: %s loaded (err=%v), want a load error naming the key", v, err)
		}
	}
	dir := writeManifest(t, `
mcp_servers:
  - name: paced_mcp
    command: paced
    always: true
    batch_seconds_per_deal: 1800
`)
	if _, err := Load(dir); err != nil {
		t.Fatalf("batch_seconds_per_deal: 1800 (the cap) must load: %v", err)
	}

	misplaced := writeManifest(t, `
agent_policy:
  batch_seconds_per_deal:
    paced_mcp: 45
`)
	if _, err := Load(misplaced); err == nil {
		t.Fatal("agent_policy.batch_seconds_per_deal loaded; the pace is declared on the server, and the strict decoder must refuse it here")
	}
}
