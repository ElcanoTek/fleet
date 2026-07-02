package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

type discInput struct {
	X string `json:"x"`
}

func discNative(n int) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("native_%d", i)
		out[i] = fantasy.NewAgentTool(name, "core tool "+name,
			func(_ context.Context, _ discInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.NewTextResponse("ok"), nil
			})
	}
	return out
}

// fakeBroker records the last dispatched MCP call and returns a canned result,
// so the disclosure tool_call path can be verified end-to-end.
type fakeBroker struct {
	lastServer, lastTool, lastArgs string
}

func (f *fakeBroker) CallMCP(_ context.Context, server, tool string, args map[string]any) (string, bool, error) {
	raw, _ := json.Marshal(args)
	f.lastServer, f.lastTool, f.lastArgs = server, tool, string(raw)
	return "called " + server + "/" + tool, false, nil
}

func discMCPTools(n int) []mcp.ServerTool {
	out := make([]mcp.ServerTool, n)
	verbs := []string{"slack_send_message Send a message to a Slack channel",
		"jira_create_issue Create a Jira issue ticket",
		"stripe_refund_charge Refund a Stripe payment"}
	for i := 0; i < n; i++ {
		text := verbs[i%len(verbs)]
		name := strings.Fields(text)[0] + "_" + itoaDisc(i)
		out[i] = mcp.ServerTool{ServerName: "srv", Tool: mcp.Tool{
			Name:        name,
			Description: strings.SplitN(text, " ", 2)[1],
			InputSchema: map[string]interface{}{"type": "object"},
		}}
	}
	return out
}

func itoaDisc(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func toolNamesOf(ts []fantasy.AgentTool) map[string]bool {
	m := map[string]bool{}
	for _, t := range ts {
		m[t.Info().Name] = true
	}
	return m
}

// TestDisclosureBelowThresholdUnchanged: a small roster registers every MCP
// tool directly, no bridges (#506 "small catalogs unchanged").
func TestDisclosureBelowThresholdUnchanged(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "128")
	got, err := buildFantasyTools(discNative(3), discMCPTools(5), &fakeBroker{}, nil, nil, nil, nil, toolBuildConfig{})
	if err != nil {
		t.Fatal(err)
	}
	names := toolNamesOf(got)
	if names["tool_search"] || names["tool_call"] {
		t.Fatal("bridges must NOT register below threshold")
	}
	if len(got) != 8 { // 3 native + 5 mcp
		t.Fatalf("want 8 tools, got %d", len(got))
	}
}

// TestDisclosureAboveThresholdDefers: a big roster stays under the ceiling by
// deferring MCP tools behind the 3 bridges; core stays direct.
func TestDisclosureAboveThresholdDefers(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "20")
	got, err := buildFantasyTools(discNative(3), discMCPTools(200), &fakeBroker{}, nil, nil, nil, nil, toolBuildConfig{})
	if err != nil {
		t.Fatalf("a 200-tool catalog must not error: %v", err)
	}
	if len(got) > maxToolsPerRequest {
		t.Fatalf("deferred roster still exceeds ceiling: %d", len(got))
	}
	names := toolNamesOf(got)
	for _, b := range []string{"tool_search", "tool_describe", "tool_call"} {
		if !names[b] {
			t.Fatalf("bridge %s missing", b)
		}
	}
	for i := 0; i < 3; i++ {
		if !names[fmt.Sprintf("native_%d", i)] {
			t.Fatalf("core native_%d must stay direct", i)
		}
	}
	// No raw MCP tool should be directly registered (they are mcp_<srv>_<tool>).
	for n := range names {
		if strings.HasPrefix(n, "mcp_srv_") {
			t.Fatalf("MCP tool %s should be deferred, not direct", n)
		}
	}
}

// TestDisclosureSearchDescribeCall exercises the full bridge flow end-to-end,
// verifying a deferred call still routes through the broker.
func TestDisclosureSearchDescribeCall(t *testing.T) {
	broker := &fakeBroker{}
	reg := newDeferredToolRegistry(mcpToolsFrom(discMCPTools(60), broker))

	// search finds the stripe refund tool.
	search := reg.searchTool()
	sresp, _ := search.Run(context.Background(), fantasy.ToolCall{Input: `{"query":"refund a stripe payment"}`})
	if sresp.IsError || !strings.Contains(sresp.Content, "mcp_srv_stripe_refund_charge") {
		t.Fatalf("search: %+v", sresp)
	}
	// pick the exact name from the results.
	name := firstStripe(sresp.Content)
	if name == "" {
		t.Fatalf("no stripe tool in results:\n%s", sresp.Content)
	}
	// describe returns its schema.
	dresp, _ := reg.describeTool().Run(context.Background(), fantasy.ToolCall{Input: `{"name":"` + name + `"}`})
	if dresp.IsError || !strings.Contains(dresp.Content, "JSON Schema") {
		t.Fatalf("describe: %+v", dresp)
	}
	// call dispatches through the broker with the real tool name.
	cresp, _ := reg.callTool().Run(context.Background(), fantasy.ToolCall{ID: "tc-9", Input: `{"name":"` + name + `","arguments":{"amount":10}}`})
	if cresp.IsError {
		t.Fatalf("call: %+v", cresp)
	}
	// The advertised name is mcp_srv_<tool>; the broker receives the underlying
	// tool name + server, so a deferred call routes exactly like a direct one.
	if broker.lastServer != "srv" || !strings.HasPrefix(broker.lastTool, "stripe_refund_charge") {
		t.Fatalf("deferred call must route to the real tool via broker: got %s/%s", broker.lastServer, broker.lastTool)
	}
	if !strings.HasSuffix(name, broker.lastTool) {
		t.Fatalf("advertised name %q should end with the underlying tool %q", name, broker.lastTool)
	}
	if !strings.Contains(broker.lastArgs, "amount") {
		t.Fatalf("args not forwarded: %q", broker.lastArgs)
	}
	// unknown name → clear error.
	if r, _ := reg.callTool().Run(context.Background(), fantasy.ToolCall{Input: `{"name":"nope"}`}); !r.IsError {
		t.Fatal("unknown tool_call must error")
	}
}

func findTool(ts []fantasy.AgentTool, name string) fantasy.AgentTool {
	for _, t := range ts {
		if t.Info().Name == name {
			return t
		}
	}
	return nil
}

// TestDisclosure_PersonaDenyGovernsDeferredTools is the #570 regression: with
// tool disclosure active (roster > threshold), a persona deny must keep the
// tool out of the deferred registry entirely — not directly registered, absent
// from tool_search/tool_describe, and rejected by tool_call BEFORE the broker
// is reached. Gate-4 must govern the deferred set exactly as it governs the
// registered roster: disclosure changes visibility, not governance.
func TestDisclosure_PersonaDenyGovernsDeferredTools(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "5")
	const denied = "mcp_srv_stripe_refund_charge_2"
	broker := &fakeBroker{}
	obs := &recordingObserver{}
	policy := PersonaToolPermissions{Deny: []string{"mcp:srv/stripe_refund_charge_2"}}
	got, err := buildFantasyTools(discNative(3), discMCPTools(20), broker, nil, nil, nil, nil,
		toolBuildConfig{personaName: "p", personaPolicy: &policy, observer: obs})
	if err != nil {
		t.Fatal(err)
	}
	names := toolNamesOf(got)
	if !names["tool_search"] || !names["tool_call"] {
		t.Fatalf("disclosure should be active (bridges registered), got %v", names)
	}
	if names[denied] {
		t.Fatalf("persona-denied tool %s was directly registered", denied)
	}
	// tool_search must not disclose the denied tool.
	sresp, _ := findTool(got, "tool_search").Run(context.Background(), fantasy.ToolCall{Input: `{"query":"refund a stripe payment","limit":50}`})
	if sresp.IsError {
		t.Fatalf("search: %+v", sresp)
	}
	if strings.Contains(sresp.Content, denied+":") {
		t.Fatalf("persona-denied tool leaked into tool_search results:\n%s", sresp.Content)
	}
	// A sibling stripe tool (not denied) proves the search itself works.
	if !strings.Contains(sresp.Content, "mcp_srv_stripe_refund_charge_5") {
		t.Fatalf("permitted sibling tool missing from search results:\n%s", sresp.Content)
	}
	// tool_describe must not reveal its schema.
	dresp, _ := findTool(got, "tool_describe").Run(context.Background(), fantasy.ToolCall{Input: `{"name":"` + denied + `"}`})
	if !dresp.IsError {
		t.Fatalf("tool_describe revealed a persona-denied tool: %+v", dresp)
	}
	// tool_call must reject it without ever reaching the broker.
	cresp, _ := findTool(got, "tool_call").Run(context.Background(), fantasy.ToolCall{ID: "tc-1", Input: `{"name":"` + denied + `","arguments":{}}`})
	if !cresp.IsError {
		t.Fatalf("tool_call dispatched a persona-denied tool: %+v", cresp)
	}
	if broker.lastTool != "" {
		t.Fatalf("broker was reached for a persona-denied tool: %s/%s", broker.lastServer, broker.lastTool)
	}
	// The suppression is audited like any Gate-4 block.
	blocked := obs.blocked()
	if len(blocked) != 1 || blocked[0]["tool"] != denied || blocked[0]["reason"] != "deny" {
		t.Fatalf("expected one persona_tool_blocked audit event for %s, got %v", denied, blocked)
	}
}

// TestDisclosure_PersonaDenyAllMCPSkipsBridges: when the persona denies the
// entire deferrable set, no registry remains and therefore no bridges register —
// the roster degrades to the direct path with zero MCP surface, rather than
// advertising bridges over an empty index.
func TestDisclosure_PersonaDenyAllMCPSkipsBridges(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "5")
	policy := PersonaToolPermissions{Deny: []string{"mcp:srv/*"}}
	got, err := buildFantasyTools(discNative(4), discMCPTools(20), &fakeBroker{}, nil, nil, nil, nil,
		toolBuildConfig{personaName: "p", personaPolicy: &policy, observer: &recordingObserver{}})
	if err != nil {
		t.Fatal(err)
	}
	names := toolNamesOf(got)
	if names["tool_search"] || names["tool_describe"] || names["tool_call"] {
		t.Fatalf("bridges must not register when the persona denied every deferrable tool: %v", names)
	}
	if len(got) != 4 { // just the native core
		t.Fatalf("want 4 native tools, got %d: %v", len(got), names)
	}
}

// TestDisclosure_PersonaFilterBelowThresholdUnchanged: below the threshold the
// persona filter applies to the directly-registered MCP tools exactly as
// before #570 — denied tool absent, siblings present, no bridges.
func TestDisclosure_PersonaFilterBelowThresholdUnchanged(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "128")
	policy := PersonaToolPermissions{Deny: []string{"mcp:srv/stripe_refund_charge_2"}}
	got, err := buildFantasyTools(discNative(2), discMCPTools(5), &fakeBroker{}, nil, nil, nil, nil,
		toolBuildConfig{personaName: "p", personaPolicy: &policy, observer: &recordingObserver{}})
	if err != nil {
		t.Fatal(err)
	}
	names := toolNamesOf(got)
	if names["tool_search"] || names["tool_call"] {
		t.Fatal("bridges must NOT register below threshold")
	}
	if names["mcp_srv_stripe_refund_charge_2"] {
		t.Fatal("persona-denied tool registered directly below threshold")
	}
	if len(got) != 6 { // 2 native + 5 mcp - 1 denied
		t.Fatalf("want 6 tools, got %d: %v", len(got), names)
	}
}

// mcpToolsFrom builds the *mcpTool wrappers a real buildFantasyTools would
// defer, so the registry test dispatches through the genuine broker path.
func mcpToolsFrom(sts []mcp.ServerTool, broker MCPBroker) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(sts))
	for i, st := range sts {
		out[i] = &mcpTool{serverName: st.ServerName, tool: st.Tool, broker: broker, policy: nil}
	}
	return out
}

func firstStripe(listing string) string {
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
		if strings.HasPrefix(line, "mcp_srv_stripe_refund_charge") {
			return strings.SplitN(line, ":", 2)[0]
		}
	}
	return ""
}
