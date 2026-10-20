package agentcore

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// The list is normalized at install: trimmed, de-duplicated, and a member
// that could never match a tool is dropped and reported (the same problems
// validate-config prints).
func TestInteractiveHidden_ListIsNormalizedAndBadMembersReported(t *testing.T) {
	got, problems := buildInteractiveHiddenList([]string{" ox_create_deal ", "", "ox_create_deal", "bad name", "mcp_ix_ix_merge_deal_domains"})
	if want := []string{"ox_create_deal", "mcp_ix_ix_merge_deal_domains"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
	if len(problems) != 2 || !strings.Contains(problems[0], "blank") || !strings.Contains(problems[1], `"bad name"`) {
		t.Fatalf("problems = %q", problems)
	}
	if p := InteractiveHiddenToolProblems(AgentPolicy{InteractiveHiddenTools: []string{"ok_tool"}}); len(p) != 0 {
		t.Fatalf("a valid list reported %q", p)
	}
}

// ConfigureAgentPolicy installs the list; the snapshot the interactive driver
// reads is a copy, and an absent key leaves it nil (so a turn's RunConfig is
// byte-identical to before).
func TestInteractiveHidden_InstalledListAndSnapshot(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{})
	if got := InteractiveHiddenTools(); got != nil {
		t.Fatalf("no key: snapshot = %v, want nil", got)
	}
	if InteractiveHiddenTool("mcp_ox_ox_create_deal") {
		t.Fatal("no key: a tool is hidden")
	}
	ConfigureAgentPolicy(AgentPolicy{InteractiveHiddenTools: []string{"ox_create_deal"}})
	snap := InteractiveHiddenTools()
	if !reflect.DeepEqual(snap, []string{"ox_create_deal"}) {
		t.Fatalf("snapshot = %v", snap)
	}
	snap[0] = "mutated"
	if !InteractiveHiddenTool("mcp_ox_ox_create_deal") || InteractiveHiddenTool("mcp_ox_ox_create_deal_preview") {
		t.Fatal("suffix matching differs from critical_tools, or the snapshot aliases the installed list")
	}
}

// Above the disclosure threshold a hidden tool is in neither the direct list
// nor the deferred registry: tool_search does not find it, tool_describe
// refuses it, and tool_call refuses it by name — saying why — without ever
// reaching the broker. Its siblings are unaffected.
func TestInteractiveHidden_DeferredToolIsUnreachable(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "5")
	const hidden = "mcp_srv_stripe_refund_charge_2"
	broker := &fakeBroker{}
	got, roster, err := buildFantasyToolsWithRoster(discNative(3), discMCPTools(20), broker, nil, nil, nil, nil,
		toolBuildConfig{hiddenSuffixes: []string{"stripe_refund_charge_2"}})
	if err != nil {
		t.Fatal(err)
	}
	names := toolNamesOf(got)
	if !names["tool_search"] || !names["tool_call"] || names[hidden] {
		t.Fatalf("want bridges and no direct %s, got %v", hidden, names)
	}
	if roster.removed[hidden] != gateInteractiveHidden || slices.Contains(roster.mcp, hidden) || roster.deferredMCP != 19 {
		t.Fatalf("roster: removed=%q, in mcp=%t, deferred=%d", roster.removed[hidden], slices.Contains(roster.mcp, hidden), roster.deferredMCP)
	}
	sresp, _ := findTool(got, "tool_search").Run(context.Background(), fantasy.ToolCall{Input: `{"query":"refund a stripe payment","limit":50}`})
	if strings.Contains(sresp.Content, hidden+":") || !strings.Contains(sresp.Content, "mcp_srv_stripe_refund_charge_5") {
		t.Fatalf("tool_search: %s", sresp.Content)
	}
	if dresp, _ := findTool(got, "tool_describe").Run(context.Background(), fantasy.ToolCall{Input: `{"name":"` + hidden + `"}`}); !dresp.IsError {
		t.Fatalf("tool_describe revealed a hidden tool: %+v", dresp)
	}
	cresp, _ := findTool(got, "tool_call").Run(context.Background(), fantasy.ToolCall{ID: "tc-1", Input: `{"name":"` + hidden + `","arguments":{}}`})
	if !cresp.IsError || cresp.Content != interactiveHiddenRefusal(hidden) {
		t.Fatalf("tool_call: %+v", cresp)
	}
	if broker.lastTool != "" {
		t.Fatalf("the broker was reached for a hidden tool: %s/%s", broker.lastServer, broker.lastTool)
	}
	// A name that never existed keeps the ordinary answer.
	if r, _ := findTool(got, "tool_call").Run(context.Background(), fantasy.ToolCall{Input: `{"name":"nope"}`}); !strings.Contains(r.Content, "no tool named") {
		t.Fatalf("unknown name: %q", r.Content)
	}
}

// Below the threshold the hidden tool is simply not registered; an absent
// list registers exactly what it did before.
func TestInteractiveHidden_DirectRosterAndAbsentList(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "128")
	without, _, err := buildFantasyToolsWithRoster(discNative(2), discMCPTools(5), &fakeBroker{}, nil, nil, nil, nil, toolBuildConfig{})
	if err != nil {
		t.Fatal(err)
	}
	with, roster, err := buildFantasyToolsWithRoster(discNative(2), discMCPTools(5), &fakeBroker{}, nil, nil, nil, nil,
		toolBuildConfig{hiddenSuffixes: []string{"jira_create_issue_1", "not_a_tool"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(without) != 7 || len(with) != 6 || toolNamesOf(with)["mcp_srv_jira_create_issue_1"] {
		t.Fatalf("without %d tools, with %d: %v", len(without), len(with), toolNamesOf(with))
	}
	if roster.removed["mcp_srv_jira_create_issue_1"] != gateInteractiveHidden {
		t.Fatalf("removed = %v", roster.removed)
	}
}

// The call gate: an mcpTool whose run hides it refuses before the policy
// gate, so a hidden CRITICAL tool stages no approval card, and the broker is
// never called.
func TestInteractiveHidden_CallGateRefusesBeforeThePolicy(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{CriticalToolSuffixes: []string{"create_deal"}})
	stager := &stagerFake{}
	pol := &policyCallLog{Policy: NewInteractivePolicy(0, 0, stager, nil)}
	broker := &fakeBroker{}
	tool := &mcpTool{serverName: "ox", tool: discMCPTools(1)[0].Tool, broker: broker, policy: pol, hiddenSuffixes: []string{"slack_send_message_0"}}
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: `{}`})
	if err != nil || !resp.IsError || resp.Content != interactiveHiddenRefusal("mcp_ox_slack_send_message_0") {
		t.Fatalf("resp %+v, err %v", resp, err)
	}
	if len(pol.calls) != 0 || len(stager.staged) != 0 || broker.lastTool != "" {
		t.Fatalf("a hidden tool reached the policy %v, the stager %v or the broker %q", pol.calls, stager.staged, broker.lastTool)
	}
}

// End to end through Run: the model is never offered the hidden tool, a
// direct call to it is explained (not "tool not found") and never runs, and
// a run without the list (a scheduled run) offers and runs it as before.
func TestInteractiveHidden_RunHidesAndExplains(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "128")
	const hidden = "mcp_srv_jira_create_issue_1"

	var offered []string
	model := newOORModel("hidden", []oorCall{{"c1", hidden, `{}`}}, []oorCall{{"c2", "native_0", `{}`}})
	inner := model.streamFunc
	model.streamFunc = func(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		if offered == nil {
			for _, tl := range call.Tools {
				offered = append(offered, tl.GetName())
			}
		}
		return inner(ctx, call)
	}
	broker := &fakeBroker{}
	runOOR(t, RunConfig{NativeTools: discNative(2), HiddenToolSuffixes: []string{"jira_create_issue_1"}},
		Deps{Model: model, MCPCatalog: discMCPTools(3), MCPBroker: broker})
	if slices.Contains(offered, hidden) || !slices.Contains(offered, "mcp_srv_slack_send_message_0") {
		t.Fatalf("offered %v", offered)
	}
	if broker.lastTool != "" {
		t.Fatalf("a hidden tool ran: %s", broker.lastTool)
	}
	if got := model.resultSeen(2, "c1"); got != interactiveHiddenRefusal(hidden) {
		t.Fatalf("the model was sent %q", got)
	}

	// The same run without the list: the tool is offered and runs.
	broker = &fakeBroker{}
	runOOR(t, RunConfig{NativeTools: discNative(2)},
		Deps{Model: newOORModel("shown", []oorCall{{"c1", hidden, `{}`}}, []oorCall{{"c2", "native_0", `{}`}}), MCPCatalog: discMCPTools(3), MCPBroker: broker})
	if broker.lastTool != "jira_create_issue_1" {
		t.Fatalf("without the list the tool did not run: %q", broker.lastTool)
	}
}
