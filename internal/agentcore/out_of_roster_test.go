package agentcore

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/metrics"
)

// Out-of-roster tool calls (#1744): out_of_roster.go.

// oorCall is one tool call a scripted model emits.
type oorCall struct{ id, name, input string }

// oorModel emits each step's tool calls in turn, then "done", and records the
// tool results it is sent on each call, by tool call ID.
type oorModel struct {
	namedMockModel
	mu   sync.Mutex
	seen []map[string]string
}

func newOORModel(name string, steps ...[]oorCall) *oorModel {
	m := &oorModel{namedMockModel: namedMockModel{name: name}}
	m.streamFunc = func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		results := map[string]string{}
		for _, msg := range call.Prompt {
			for _, part := range msg.Content {
				tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
				if !ok {
					continue
				}
				if e, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Output); ok {
					results[tr.ToolCallID] = e.Error.Error()
				} else if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output); ok {
					results[tr.ToolCallID] = txt.Text
				}
			}
		}
		m.mu.Lock()
		m.seen = append(m.seen, results)
		n := len(m.seen)
		m.mu.Unlock()
		if n > len(steps) {
			return streamTextThenFinish("done"), nil
		}
		calls := steps[n-1]
		return func(yield func(fantasy.StreamPart) bool) {
			for _, c := range calls {
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: c.id, ToolCallName: c.name, ToolCallInput: c.input}) {
					return
				}
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
		}, nil
	}
	return m
}

// resultSeen is the result of call id the model was sent on its call n
// (1-based).
func (m *oorModel) resultSeen(n int, id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n > len(m.seen) {
		return ""
	}
	return m.seen[n-1][id]
}

// policyCallLog records every policy call of the wrapped Policy, in order.
type policyCallLog struct {
	Policy
	mu    sync.Mutex
	calls []string
}

func (p *policyCallLog) BeforeToolCall(name, id, input string) (bool, string) {
	blocked, msg := p.Policy.BeforeToolCall(name, id, input)
	p.mu.Lock()
	p.calls = append(p.calls, fmt.Sprintf("before %s %s %s -> %t %s", name, id, input, blocked, msg))
	p.mu.Unlock()
	return blocked, msg
}

func (p *policyCallLog) RecordToolResult(name, input, result string, ok bool) {
	p.mu.Lock()
	p.calls = append(p.calls, fmt.Sprintf("record %s %s %q %t", name, input, result, ok))
	p.mu.Unlock()
	p.Policy.RecordToolResult(name, input, result, ok)
}

func (p *policyCallLog) Unwrap() Policy { return p.Policy }

// transcriptOf is a run's tool calls and results as the log session recorded
// them.
func transcriptOf(s *LogSession) []string {
	var out []string
	for _, m := range s.Messages {
		for _, tc := range m.ToolCalls {
			out = append(out, fmt.Sprintf("call %s %s", tc.Name, tc.Arguments))
		}
		if m.Role == "tool" {
			out = append(out, fmt.Sprintf("result %s %s", m.ToolName, m.Content))
		}
	}
	return out
}

// oorEvents records the out-of-roster Observer events.
type oorEvents struct {
	mu     sync.Mutex
	events []map[string]any
}

func (o *oorEvents) Observe(eventType string, payload map[string]any) {
	if eventType != eventToolCallOutOfRoster {
		return
	}
	o.mu.Lock()
	o.events = append(o.events, payload)
	o.mu.Unlock()
}

func runOOR(t *testing.T, cfg RunConfig, deps Deps) {
	t.Helper()
	cfg.EnvPrefix = CanonicalEnvPrefix
	if deps.Input == nil {
		deps.Input = historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("go")}, label: "oor"}
	}
	if deps.Policy == nil {
		deps.Policy = NewInteractivePolicy(0, 0, nil, nil)
	}
	if deps.MCPBroker == nil {
		deps.MCPBroker = &fakeBroker{}
	}
	if _, err := Run(context.Background(), ModeInteractive, cfg, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// A direct call to a deferred tool runs through tool_call, and everything the
// run records about it, the approval it is staged for included, matches a
// tool_call the model wrote itself.
func TestOutOfRoster_DeferredCallRunsThroughToolCall(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "5")
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{CriticalToolSuffixes: []string{"stripe_refund_charge_2"}})

	type outcome struct {
		Policy, Staged, Transcript []string
		Broker                     string
	}
	run := func(t *testing.T, call oorCall) outcome {
		stager := &stagerFake{}
		pol := &policyCallLog{Policy: NewInteractivePolicy(0, 0, stager, nil)}
		broker := &fakeBroker{}
		session := NewLogSession()
		runOOR(t, RunConfig{NativeTools: discNative(3)}, Deps{
			Policy: pol, Model: newOORModel("oor-parity", []oorCall{call}),
			MCPCatalog: discMCPTools(20), MCPBroker: broker, LogSession: session,
		})
		return outcome{pol.calls, stager.staged, transcriptOf(session), broker.lastServer + "/" + broker.lastTool + " " + broker.lastArgs}
	}
	for _, tc := range []struct {
		name, tool, args string
		staged           bool
	}{
		{"a critical tool is staged for approval", "mcp_srv_stripe_refund_charge_2", `{"amount":5}`, true},
		{"another tool runs", "mcp_srv_slack_send_message_0", `{"text":"hi"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bridged := run(t, oorCall{"c1", toolNameToolCall, `{"name":"` + tc.tool + `","arguments":` + tc.args + `}`})
			direct := run(t, oorCall{"c1", tc.tool, tc.args})
			if !reflect.DeepEqual(direct, bridged) {
				t.Fatalf("a direct call to a deferred tool differs from a tool_call:\ndirect:  %+v\nbridged: %+v", direct, bridged)
			}
			wantBroker := "srv/" + strings.TrimPrefix(tc.tool, "mcp_srv_") + " " + tc.args
			if tc.staged {
				wantBroker = "/ " // blocked pending approval: never dispatched
			}
			if (len(direct.Staged) == 1) != tc.staged || direct.Broker != wantBroker {
				t.Fatalf("staged %v, broker %q; want staged=%t, broker %q", direct.Staged, direct.Broker, tc.staged, wantBroker)
			}
		})
	}
}

// A call to a tool a gate removed is never run, and its result says which
// gate removed it: in the transcript, and in what the model is sent, the same
// on every later step.
func TestOutOfRoster_GatedCallIsExplainedAndNeverRuns(t *testing.T) {
	ran := false
	native := append(discNative(2), echoTool{name: "secret_tool", ran: &ran, out: "ran"})
	broker := &fakeBroker{}
	session := NewLogSession()
	model := newOORModel("oor-gated",
		[]oorCall{{"c1", "secret_tool", `{}`}, {"c2", "mcp_srv_jira_create_issue_1", `{}`}},
		[]oorCall{{"c3", "native_0", `{}`}},
	)
	runOOR(t, RunConfig{
		NativeTools: native, PersonaName: "analyst",
		PersonaPolicy: &PersonaToolPermissions{Deny: []string{"secret_tool", "mcp:srv/jira_create_issue_1"}},
	}, Deps{Model: model, MCPCatalog: discMCPTools(3), MCPBroker: broker, LogSession: session})

	if ran || broker.lastTool != "" {
		t.Fatalf("a persona-denied tool ran: native %t, broker %q", ran, broker.lastTool)
	}
	for id, tool := range map[string]string{"c1": "secret_tool", "c2": "mcp_srv_jira_create_issue_1"} {
		want := tool + ` is not available in this run and was not called: the "analyst" persona's tool policy does not allow it.`
		if got := model.resultSeen(2, id); got != want {
			t.Errorf("%s: the model was sent %q, want %q", tool, got, want)
		}
		if again := model.resultSeen(3, id); again != model.resultSeen(2, id) {
			t.Errorf("%s: the explanation changed between steps (prompt-cache prefix): %q then %q", tool, model.resultSeen(2, id), again)
		}
		if !slices.Contains(transcriptOf(session), "result "+tool+" "+want) {
			t.Errorf("%s: the transcript does not record the explanation: %v", tool, transcriptOf(session))
		}
	}
}

// Rerouting can never widen access: only a name in the run's deferred
// registry is rewritten. A tool the persona or an allowlist removed keeps
// its own name, gets the gate's explanation, and never reaches the broker.
func TestOutOfRoster_NeverReroutesAGatedTool(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "5")
	for _, tc := range []struct {
		name, tool, why string
		cfg             RunConfig
	}{
		{"persona", "mcp_srv_stripe_refund_charge_2", "this persona's tool policy does not allow it",
			RunConfig{PersonaPolicy: &PersonaToolPermissions{Deny: []string{"mcp:srv/stripe_refund_charge_2"}}}},
		{"allowlist", "mcp_srv_stripe_refund_charge_2", "its connector's tool allowlist does not include it",
			RunConfig{Allowlist: mcpAllowlist{"srv": discToolNames(20, "stripe_refund_charge_2")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker := &fakeBroker{}
			session := NewLogSession()
			model := newOORModel("oor-widen", []oorCall{{"c1", tc.tool, `{}`}})
			tc.cfg.NativeTools = discNative(3)
			runOOR(t, tc.cfg, Deps{Model: model, MCPCatalog: discMCPTools(20), MCPBroker: broker, LogSession: session})
			want := tc.tool + " is not available in this run and was not called: " + tc.why + "."
			tr := transcriptOf(session)
			if !slices.Contains(tr, "call "+tc.tool+" {}") || !slices.Contains(tr, "result "+tc.tool+" "+want) {
				t.Fatalf("want the call kept under its own name and explained, got %v", tr)
			}
			if broker.lastTool != "" {
				t.Fatalf("a gated tool reached the broker: %s", broker.lastTool)
			}
		})
	}
}

// discToolNames is discMCPTools(n)'s tool names, without the ones excluded.
func discToolNames(n int, exclude ...string) []string {
	var out []string
	for _, st := range discMCPTools(n) {
		if !slices.Contains(exclude, st.Tool.Name) {
			out = append(out, st.Tool.Name)
		}
	}
	return out
}

// Each gate that drops a catalog tool records itself in the roster.
func TestOutOfRoster_RosterRecordsTheGate(t *testing.T) {
	catalog := []mcp.ServerTool{
		{ServerName: "opt", Tool: mcp.Tool{Name: "a", InputSchema: map[string]any{"type": "object"}}},
		{ServerName: "srv", Tool: mcp.Tool{Name: "kept", InputSchema: map[string]any{"type": "object"}}},
		{ServerName: "srv", Tool: mcp.Tool{Name: "unlisted", InputSchema: map[string]any{"type": "object"}}},
		{ServerName: "srv", Tool: mcp.Tool{Name: "denied", InputSchema: map[string]any{"type": "object"}}},
		{ServerName: "srv", Tool: mcp.Tool{Name: "broken", InputSchema: jsonMap(t, `{"type":"object","properties":{"q":{"type":42}}}`)}},
	}
	allow := mcpAllowlist{"srv": {"kept", "denied", "broken"}}
	persona := &PersonaToolPermissions{Deny: []string{"mcp:srv/denied"}}
	for _, tc := range []struct {
		name      string
		exclusive bool
		want      map[string]string
	}{
		{"allowlist", false, map[string]string{"mcp_opt_a": gateOptIn, "mcp_srv_unlisted": gateAllowlist, "mcp_srv_denied": gatePersona, "mcp_srv_broken": gateSchema}},
		{"narrowed", true, map[string]string{"mcp_opt_a": gateOptIn, "mcp_srv_unlisted": gateNarrowed, "mcp_srv_denied": gatePersona, "mcp_srv_broken": gateSchema}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, roster, err := buildFantasyToolsWithRoster(nil, catalog, &fakeBroker{}, allow, nil, mcpOptionalSet{"opt": true}, nil,
				toolBuildConfig{exclusiveAllowlist: tc.exclusive, personaPolicy: persona, observer: &recordingObserver{}})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(roster.removed, tc.want) {
				t.Fatalf("removed = %v, want %v", roster.removed, tc.want)
			}
		})
	}
}

// The metric and the Observer event carry the model's slug and a bounded
// reason; a made-up name keeps fantasy's own "tool not found" result.
func TestOutOfRoster_CountedPerModelAndReason(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "5")
	obs := &oorEvents{}
	session := NewLogSession()
	model := newOORModel("oor-metric-model", []oorCall{
		{"c1", "mcp_srv_slack_send_message_0", `{"text":"hi"}`},
		{"c2", "str_replace_editor", `{}`},
	})
	runOOR(t, RunConfig{NativeTools: discNative(3)}, Deps{Model: model, MCPCatalog: discMCPTools(20), Observer: obs, LogSession: session})

	out := metrics.Render()
	for _, line := range []string{
		`fleet_tool_call_out_of_roster_total{model="oor-metric-model",reason="deferred"} 1`,
		`fleet_tool_call_out_of_roster_total{model="oor-metric-model",reason="unknown"} 1`,
	} {
		if !strings.Contains(out, line) {
			t.Errorf("metrics lack %s", line)
		}
	}
	obs.mu.Lock()
	events := obs.events
	obs.mu.Unlock()
	want := []map[string]any{
		{"id": "c1", "tool": "mcp_srv_slack_send_message_0", "reason": "deferred", "model": "oor-metric-model", "rerouted_to": "tool_call"},
		{"id": "c2", "tool": "str_replace_editor", "reason": "unknown", "model": "oor-metric-model"},
	}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events = %v, want %v", events, want)
	}
	if got := model.resultSeen(2, "c2"); !strings.HasPrefix(got, "tool not found: str_replace_editor. Available tools: ") {
		t.Errorf("a made-up name should keep fantasy's result, got %q", got)
	}
}

// Installing a repair function turns off fantasy's default jsonrepair; the
// run's keeps it for registered tools.
func TestOutOfRoster_KeepsFantasysJSONRepair(t *testing.T) {
	ran := false
	model := newOORModel("oor-json", []oorCall{{"c1", "echo_tool", `{"a":1,}`}})
	runOOR(t, RunConfig{NativeTools: []fantasy.AgentTool{echoTool{name: "echo_tool", ran: &ran, out: "ran"}}}, Deps{Model: model})
	if !ran {
		t.Fatalf("malformed JSON for a registered tool was not repaired; the model saw %q", model.resultSeen(2, "c1"))
	}
}

// A deferred tool called with arguments that are not a JSON object is not
// rerouted; its result says how to call it.
func TestOutOfRoster_DeferredCallWithBadArguments(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "5")
	broker := &fakeBroker{}
	model := newOORModel("oor-badargs", []oorCall{{"c1", "mcp_srv_slack_send_message_0", `["hi"]`}})
	runOOR(t, RunConfig{NativeTools: discNative(3)}, Deps{Model: model, MCPCatalog: discMCPTools(20), MCPBroker: broker})
	want := `mcp_srv_slack_send_message_0 was not called: it is reachable only through tool_call, and its arguments were not a JSON object. ` +
		`Call tool_call with {"name": "mcp_srv_slack_send_message_0", "arguments": {...}}.`
	if got := model.resultSeen(2, "c1"); got != want || broker.lastTool != "" {
		t.Fatalf("result %q (broker %q), want %q", got, broker.lastTool, want)
	}
}

// explainResults rewrites copies: the run's own history keeps fantasy's
// result, and results without a note, or that are not errors, are untouched.
func TestOutOfRoster_ExplainResultsCopies(t *testing.T) {
	notFound := fantasy.ToolResultOutputContentError{Error: errors.New("tool not found: x")}
	msgs := []fantasy.Message{
		fantasy.NewUserMessage("hi"),
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "a", Output: notFound},
			fantasy.ToolResultPart{ToolCallID: "b", Output: notFound},
			fantasy.ToolResultPart{ToolCallID: "c", Output: fantasy.ToolResultOutputContentText{Text: "ok"}},
		}},
	}
	out, changed := explainResults(msgs, map[string]string{"a": "explained", "c": "never"})
	if !changed {
		t.Fatal("want a change")
	}
	text := func(m []fantasy.Message, i int) string {
		tr, _ := fantasy.AsMessagePart[fantasy.ToolResultPart](m[1].Content[i])
		if e, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Output); ok {
			return e.Error.Error()
		}
		txt, _ := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output)
		return txt.Text
	}
	if text(out, 0) != "explained" || text(out, 1) != "tool not found: x" || text(out, 2) != "ok" {
		t.Errorf("rewritten = %q, %q, %q", text(out, 0), text(out, 1), text(out, 2))
	}
	if text(msgs, 0) != "tool not found: x" {
		t.Errorf("the original history was changed: %q", text(msgs, 0))
	}
	if _, changed := explainResults(msgs, map[string]string{"z": "n"}); changed {
		t.Error("no note matched, yet a change was reported")
	}
}

// In a narrowed run, a refused tool's explanation names the registered tool
// that stands in for it, when there is one.
func TestOutOfRoster_NarrowedNoteNamesTheStandIn(t *testing.T) {
	withPagesPolicy(t, pagesAliases)
	orch := newOrchStateForTest()
	orch.setNarrowedMCPRoster([]string{aliasUploadTool})
	o := newOutOfRoster("", orch, mcpGates{})
	want := aliasInlineTool + " is not available in this run and was not called: this run's tools are narrowed to the ones it requires, and " +
		aliasInlineTool + " is not one of them; the registered stand-in is " + aliasUploadTool + ", call it instead."
	if got := o.gateNote(aliasInlineTool, gateNarrowed); got != want {
		t.Fatalf("note = %q, want %q", got, want)
	}
	if got := newOutOfRoster("", nil, mcpGates{}).gateNote("mcp_x_y", gateNarrowed); strings.Contains(got, "stand-in") {
		t.Fatalf("no orchestration state, yet a stand-in was named: %q", got)
	}
}

// The finalize retry gets the same explanations and the same rerouting.
func TestOutOfRoster_FinalizeInputCarriesTheRecovery(t *testing.T) {
	o := newOutOfRoster("", nil, mcpGates{})
	o.setRoster(toolRoster{deferredMCP: 1, mcp: []string{"mcp_srv_x"}})
	o.notes["c1"] = "explained"
	in := finalizeInputFor(ModeInteractive, "", nil, 0, nil, nil, "", nil, panicAttribution{}, nil, nil, 0, o)

	msgs := []fantasy.Message{{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
		fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentError{Error: errors.New("tool not found: mcp_y")}},
	}}}
	_, res, err := in.GuardStep(nil)(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil || len(res.Messages) != 1 {
		t.Fatalf("GuardStep: %v, %d messages", err, len(res.Messages))
	}
	tr, _ := fantasy.AsMessagePart[fantasy.ToolResultPart](res.Messages[0].Content[0])
	if e, _ := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Output); e.Error == nil || e.Error.Error() != "explained" {
		t.Fatalf("the finalize retry's model is not sent the explanation: %+v", tr.Output)
	}

	bridge := fantasy.NewAgentTool(toolNameToolCall, "bridge", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse(""), nil
	})
	repaired, err := in.RepairToolCall("m")(context.Background(), fantasy.ToolCallRepairOptions{
		OriginalToolCall: fantasy.ToolCallContent{ToolCallID: "c2", ToolName: "mcp_srv_x", Input: `{"a":1}`},
		AvailableTools:   []fantasy.AgentTool{bridge},
	})
	if err != nil || repaired == nil || repaired.ToolName != toolNameToolCall || repaired.Input != `{"name":"mcp_srv_x","arguments":{"a":1}}` {
		t.Fatalf("repaired = %+v, %v; want the call rerouted through tool_call", repaired, err)
	}
}

// Out of process, the MCP broker applies the allowlist before the catalog
// arrives, so the roster never sees what it dropped. The gate is still named,
// by the run's own Gate-1 and Gate-2 rules applied to the called name.
func TestOutOfRoster_GateOfAToolTheBrokerAlreadyDropped(t *testing.T) {
	filtered := func(drop ...string) []mcp.ServerTool {
		var out []mcp.ServerTool
		for _, st := range discMCPTools(6) {
			if !slices.Contains(drop, st.Tool.Name) {
				out = append(out, st)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name, tool, why string
		cfg             RunConfig
	}{
		{"allowlist", "mcp_srv_stripe_refund_charge_2", "its connector's tool allowlist does not include it",
			RunConfig{Allowlist: mcpAllowlist{"srv": discToolNames(6, "stripe_refund_charge_2")}}},
		{"narrowed", "mcp_srv_stripe_refund_charge_2", "this run's tools are narrowed to the ones it requires, and mcp_srv_stripe_refund_charge_2 is not one of them",
			RunConfig{Allowlist: mcpAllowlist{"srv": discToolNames(6, "stripe_refund_charge_2")}, MCPRosterNarrowing: "required_tools_only"}},
		{"narrowed, a server with no entry", "mcp_other_sync", "this run's tools are narrowed to the ones it requires, and mcp_other_sync is not one of them",
			// The scheduled driver gives a server with no required tool this
			// deny-all entry (scheduledrun's rosterNarrowingDeniesAll).
			RunConfig{Allowlist: mcpAllowlist{"srv": discToolNames(6), "other": {"__roster_narrowing_denies_all_tools__"}}, MCPRosterNarrowing: "required_tools_only"}},
		{"optional connector", "mcp_opt_lookup", "it belongs to an optional connector this run did not turn on",
			RunConfig{OptionalServers: mcpOptionalSet{"opt": true}}},
		{"no gate: a made-up tool", "mcp_srv_made_up", "",
			RunConfig{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker := &fakeBroker{}
			model := newOORModel("oor-filtered", []oorCall{{"c1", tc.tool, `{}`}})
			runOOR(t, tc.cfg, Deps{Model: model, MCPCatalog: filtered("stripe_refund_charge_2"), MCPBroker: broker})
			got := model.resultSeen(2, "c1")
			if tc.why == "" {
				if !strings.HasPrefix(got, "tool not found: "+tc.tool+". Available tools: ") {
					t.Fatalf("result %q, want fantasy's own", got)
				}
			} else if want := tc.tool + " is not available in this run and was not called: " + tc.why + "."; got != want {
				t.Fatalf("result %q, want %q", got, want)
			}
			if broker.lastTool != "" {
				t.Fatalf("a gated tool reached the broker: %s", broker.lastTool)
			}
		})
	}
}
