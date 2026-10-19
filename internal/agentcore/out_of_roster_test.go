package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
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
		{"arguments keep their own bytes", "mcp_srv_slack_send_message_0", `{ "text": "<b>hi</b> & bye" }`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bridged := run(t, oorCall{"c1", toolNameToolCall, `{"name":"` + tc.tool + `","arguments":` + tc.args + `}`})
			direct := run(t, oorCall{"c1", tc.tool, tc.args})
			if !reflect.DeepEqual(direct, bridged) {
				t.Fatalf("a direct call to a deferred tool differs from a tool_call:\ndirect:  %+v\nbridged: %+v", direct, bridged)
			}
			wantBroker := "srv/" + strings.TrimPrefix(tc.tool, "mcp_srv_") + " " + compactJSON(t, tc.args)
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
	deferred, unknown := oorCount("oor-metric-model", "deferred"), oorCount("oor-metric-model", "unknown")
	runOOR(t, RunConfig{NativeTools: discNative(3)}, Deps{Model: model, MCPCatalog: discMCPTools(20), Observer: obs, LogSession: session})

	if got := oorCount("oor-metric-model", "deferred"); got != deferred+1 {
		t.Errorf("deferred count went from %v to %v, want one more", deferred, got)
	}
	if got := oorCount("oor-metric-model", "unknown"); got != unknown+1 {
		t.Errorf("unknown count went from %v to %v, want one more", unknown, got)
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
	want := `mcp_srv_slack_send_message_0 was not called: it is reachable only through tool_call. ` +
		`Call tool_call with {"name": "mcp_srv_slack_send_message_0", "arguments": {...}}, with the arguments as a JSON object.`
	if got := model.resultSeen(2, "c1"); got != want || broker.lastTool != "" {
		t.Fatalf("result %q (broker %q), want %q", got, broker.lastTool, want)
	}
}

// explainResults rewrites copies: the run's own history keeps fantasy's
// result, and results without a note for their call ID, tool and refusal
// text, or that are not errors, are untouched.
func TestOutOfRoster_ExplainResultsCopies(t *testing.T) {
	notFound := fantasy.ToolResultOutputContentError{Error: errors.New("tool not found: x")}
	msgs := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "a", ToolName: "x"},
			fantasy.ToolCallPart{ToolCallID: "b", ToolName: "x"},
			fantasy.ToolCallPart{ToolCallID: "c", ToolName: "x"},
			fantasy.ToolCallPart{ToolCallID: "d", ToolName: "x"},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "a", Output: notFound},
			fantasy.ToolResultPart{ToolCallID: "b", Output: notFound},
			fantasy.ToolResultPart{ToolCallID: "c", Output: fantasy.ToolResultOutputContentText{Text: "ok"}},
			fantasy.ToolResultPart{ToolCallID: "d", Output: fantasy.ToolResultOutputContentError{Error: errors.New("it ran and failed")}},
		}},
	}
	out, changed := explainResults(msgs, map[noteKey]string{
		{"a", "x", "tool not found: x"}: "explained", {"c", "x", "ok"}: "never", {"b", "other", "tool not found: x"}: "wrong tool",
		{"d", "x", "tool not found: x"}: "not this refusal",
	})
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
	if text(out, 0) != "explained" || text(out, 1) != "tool not found: x" || text(out, 2) != "ok" || text(out, 3) != "it ran and failed" {
		t.Errorf("rewritten = %q, %q, %q, %q", text(out, 0), text(out, 1), text(out, 2), text(out, 3))
	}
	if text(msgs, 0) != "tool not found: x" {
		t.Errorf("the original history was changed: %q", text(msgs, 0))
	}
	if _, changed := explainResults(msgs, map[noteKey]string{{"z", "x", "tool not found: x"}: "n"}); changed {
		t.Error("no note matched, yet a change was reported")
	}
	if again, changed := explainResults(out, map[noteKey]string{{"a", "x", "tool not found: x"}: "explained"}); changed || len(again) != 0 {
		t.Error("an already explained result was copied again")
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
	o.notes[noteKey{"c1", "mcp_y", "tool not found: mcp_y"}] = "explained"
	in := finalizeInputFor(ModeInteractive, "", nil, 0, nil, nil, "", nil, panicAttribution{}, nil, nil, 0, o)

	msgs := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "mcp_y"}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentError{Error: errors.New("tool not found: mcp_y")}},
		}},
	}
	_, res, err := in.GuardStep(nil)(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil || len(res.Messages) != 2 {
		t.Fatalf("GuardStep: %v, %d messages", err, len(res.Messages))
	}
	tr, _ := fantasy.AsMessagePart[fantasy.ToolResultPart](res.Messages[1].Content[0])
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
// arrives, so the roster never sees what it dropped. The run's own Gate-1 and
// Gate-2 rules, applied to the called name, still find the gate; but a name
// alone cannot tell a dropped tool from a made-up one on the same connector,
// so fantasy's result, with its list of tools, is kept and only a sentence
// true of both is added, and the metric counts the call as unknown.
func TestOutOfRoster_GateOfAToolTheBrokerAlreadyDropped(t *testing.T) {
	tool := func(server, name string) mcp.ServerTool {
		return mcp.ServerTool{ServerName: server, Tool: mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}}
	}
	// What the broker hands over: each server's allowed tools only.
	catalog := []mcp.ServerTool{
		tool("srv", "search"), tool("google", "search"), tool("google_drive", "list"), tool("jira_prod", "search"),
	}
	const narrowed = "This run's tools are narrowed to the ones it requires, and that name is not one of them."
	for _, tc := range []struct {
		name, tool, gate, sentence string
		cfg                        RunConfig
	}{
		{"allowlist", "mcp_srv_refund", "allowlist", "The srv connector's tool allowlist does not include that name.",
			RunConfig{Allowlist: mcpAllowlist{"srv": {"search"}}}},
		{"the longest server name", "mcp_google_drive_delete", "allowlist", "The google_drive connector's tool allowlist does not include that name.",
			RunConfig{Allowlist: mcpAllowlist{"google_drive": {"list"}}}},
		{"a variant seat under its base's allowlist", "mcp_jira_prod_delete", "allowlist", "The jira_prod connector's tool allowlist does not include that name.",
			RunConfig{Allowlist: mcpAllowlist{"jira": {"search"}}}},
		{"narrowed", "mcp_srv_refund", "narrowed", narrowed,
			RunConfig{Allowlist: mcpAllowlist{"srv": {"search"}}, MCPRosterNarrowing: "required_tools_only"}},
		// The scheduled driver gives a server with no required tool this
		// deny-all entry (scheduledrun's rosterNarrowingDeniesAll).
		{"narrowed, a server with no required tool", "mcp_other_sync", "narrowed", narrowed,
			RunConfig{Allowlist: mcpAllowlist{"srv": {"search"}, "other": {"__roster_narrowing_denies_all_tools__"}}, MCPRosterNarrowing: "required_tools_only"}},
		{"optional connector", "mcp_opt_lookup", "opt_in", "That name is on the optional opt connector, which this run did not turn on.",
			RunConfig{OptionalServers: mcpOptionalSet{"opt": true}}},
		{"no gate: an allowlist key for a server not in this run", "mcp_stripe_refund", "", "",
			RunConfig{Allowlist: mcpAllowlist{"stripe": {"charge"}}}},
		{"no gate: a made-up tool with no allowlist", "mcp_srv_made_up", "", "",
			RunConfig{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker := &fakeBroker{}
			obs := &oorEvents{}
			model := newOORModel("oor-filtered", []oorCall{{"c1", tc.tool, `{}`}})
			runOOR(t, tc.cfg, Deps{Model: model, MCPCatalog: catalog, MCPBroker: broker, Observer: obs})
			got := model.resultSeen(2, "c1")
			if !strings.HasPrefix(got, "tool not found: "+tc.tool+". Available tools: ") {
				t.Fatalf("result %q, want fantasy's own text first", got)
			}
			if tc.sentence != "" && !strings.HasSuffix(got, ". "+tc.sentence) {
				t.Fatalf("result %q, want it to end %q", got, tc.sentence)
			}
			if tc.sentence == "" && strings.Count(got, ". ") > 1 {
				t.Fatalf("result %q, want fantasy's text alone", got)
			}
			if broker.lastTool != "" {
				t.Fatalf("a gated tool reached the broker: %s", broker.lastTool)
			}
			obs.mu.Lock()
			defer obs.mu.Unlock()
			if len(obs.events) != 1 || obs.events[0]["reason"] != "unknown" || (obs.events[0]["inferred_gate"] != nil) != (tc.gate != "") ||
				(tc.gate != "" && obs.events[0]["inferred_gate"] != tc.gate) {
				t.Fatalf("events = %v, want reason unknown and inferred gate %q", obs.events, tc.gate)
			}
		})
	}
}

// compactJSON is s re-encoded as the broker's json.Marshal writes it.
func compactJSON(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// failTool is a native tool whose call fails, after doing its work.
type failTool struct{ name, msg string }

func (f failTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: f.name, Description: "fails", Parameters: map[string]any{"type": "object"}}
}
func (f failTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (f failTool) SetProviderOptions(_ fantasy.ProviderOptions) {}
func (f failTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextErrorResponse(f.msg), nil
}

// A provider may reuse a call ID. An explanation belongs to the refused call
// alone: a later call with the same ID to a registered tool that ran and
// failed keeps its own error, never "was not called".
func TestOutOfRoster_AReusedCallIDKeepsEachResult(t *testing.T) {
	const failed = "upstream 500: write may have partially applied"
	session := NewLogSession()
	model := newOORModel("oor-reused-id",
		[]oorCall{{"call_0", "secret_tool", `{}`}},
		[]oorCall{{"call_0", "write_thing", `{}`}},
	)
	runOOR(t, RunConfig{
		NativeTools:   []fantasy.AgentTool{echoTool{name: "secret_tool", out: "ran"}, failTool{name: "write_thing", msg: failed}},
		PersonaPolicy: &PersonaToolPermissions{Deny: []string{"secret_tool"}},
	}, Deps{Model: model, LogSession: session})
	tr := transcriptOf(session)
	if !slices.Contains(tr, "result write_thing "+failed) {
		t.Fatalf("the failed write must keep its own error, transcript %v", tr)
	}
	if !slices.Contains(tr, "result secret_tool secret_tool is not available in this run and was not called: this persona's tool policy does not allow it.") {
		t.Fatalf("the refused call must keep its explanation, transcript %v", tr)
	}
	if got := model.resultSeen(3, "call_0"); got != failed {
		t.Fatalf("the model was sent %q for the failed write, want its own error", got)
	}
}

// promptLog records the prompt of every model call made through a wrapped
// oorModel.
type promptLog struct {
	mu      sync.Mutex
	prompts []fantasy.Prompt
}

func (l *promptLog) wrap(m *oorModel) {
	inner := m.streamFunc
	m.streamFunc = func(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		l.mu.Lock()
		l.prompts = append(l.prompts, call.Prompt)
		l.mu.Unlock()
		return inner(ctx, call)
	}
}

// parts is every result text and call input for call id in the prompt of
// model call n (1-based), in order.
func (l *promptLog) parts(n int, id string) (results, inputs []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > len(l.prompts) {
		return nil, nil
	}
	for _, m := range l.prompts[n-1] {
		for _, p := range m.Content {
			if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p); ok && tc.ToolCallID == id {
				inputs = append(inputs, tc.Input)
			}
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p); ok && tr.ToolCallID == id {
				if e, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Output); ok {
					results = append(results, e.Error.Error())
				}
			}
		}
	}
	return results, inputs
}

// The history the model is sent includes earlier turns, and a provider may
// reuse a call ID across them. An explanation replaces only fantasy's refusal
// of its own call: an earlier turn's call with the same ID and tool, which ran
// and failed, keeps its error, and the prompt-cache prefix holds.
func TestOutOfRoster_AnEarlierTurnsResultKeepsItsText(t *testing.T) {
	const failed = "upstream 500: write may have partially applied"
	history := []fantasy.Message{
		fantasy.NewUserMessage("send it"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "call_0", ToolName: "write_thing", Input: `{}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "call_0", Output: fantasy.ToolResultOutputContentError{Error: errors.New(failed)}},
		}},
		fantasy.NewUserMessage("try again"),
	}
	model := newOORModel("oor-earlier-turn", []oorCall{{"call_0", "write_thing", `{}`}})
	prompts := &promptLog{}
	prompts.wrap(model)
	runOOR(t, RunConfig{
		NativeTools:   []fantasy.AgentTool{failTool{name: "write_thing", msg: failed}},
		PersonaPolicy: &PersonaToolPermissions{Deny: []string{"write_thing"}},
	}, Deps{Model: model, Input: historyInput{system: "s", msgs: history, label: "oor"}})

	const explained = "write_thing is not available in this run and was not called: this persona's tool policy does not allow it."
	first, _ := prompts.parts(1, "call_0")
	second, _ := prompts.parts(2, "call_0")
	if want := []string{failed}; !reflect.DeepEqual(first, want) {
		t.Fatalf("first call was sent %q, want %q", first, want)
	}
	if want := []string{failed, explained}; !reflect.DeepEqual(second, want) {
		t.Fatalf("second call was sent %q, want the earlier failure kept and this turn's refusal explained %q", second, want)
	}
}

// A refused call whose input is not a JSON object is replayed both ways
// fixed: its input normalized (#1783) and its result explained.
func TestOutOfRoster_ExplainedWithItsInputSanitized(t *testing.T) {
	model := newOORModel("oor-sanitized", []oorCall{{"c1", "secret_tool", `["x"]`}})
	prompts := &promptLog{}
	prompts.wrap(model)
	runOOR(t, RunConfig{
		NativeTools:   []fantasy.AgentTool{echoTool{name: "secret_tool", out: "ran"}},
		PersonaPolicy: &PersonaToolPermissions{Deny: []string{"secret_tool"}},
	}, Deps{Model: model})
	results, inputs := prompts.parts(2, "c1")
	const explained = "secret_tool is not available in this run and was not called: this persona's tool policy does not allow it."
	if !reflect.DeepEqual(inputs, []string{"{}"}) || !reflect.DeepEqual(results, []string{explained}) {
		t.Fatalf("the model was sent input %q and result %q; want {} and %q", inputs, results, explained)
	}
}

// The terminal structured-output phase, which takes the round's transcript
// from the carry rather than a stream, is sent the explanation too.
func TestOutOfRoster_TerminalOutputSeesTheExplanation(t *testing.T) {
	script := newOORModel("unused", []oorCall{{"c1", "secret_tool", `{}`}})
	model := &terminalTestModel{provider: "anthropic", model: "oor-terminal", responses: []*fantasy.Response{terminalToolResponse(`{"answer":1}`)}}
	model.streamFunc = script.streamFunc
	runOOR(t, RunConfig{
		OutputSchema:  json.RawMessage(terminalTestSchema),
		NativeTools:   []fantasy.AgentTool{echoTool{name: "secret_tool", out: "ran"}},
		PersonaPolicy: &PersonaToolPermissions{Deny: []string{"secret_tool"}},
	}, Deps{Model: model})
	const want = "secret_tool is not available in this run and was not called: this persona's tool policy does not allow it."
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.generate) != 1 {
		t.Fatalf("terminal calls = %d, want 1", len(model.generate))
	}
	for _, m := range model.generate[0].Prompt {
		for _, p := range m.Content {
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p); ok && tr.ToolCallID == "c1" {
				if e, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Output); !ok || e.Error.Error() != want {
					t.Fatalf("the terminal phase was sent %+v, want the explanation", tr.Output)
				}
				return
			}
		}
	}
	t.Fatal("the terminal phase was not sent the refused call's result")
}

// The sink substitutes the explanation only for fantasy's refusal of the
// call: a call that succeeded, or ran and failed, keeps its own text whatever
// note shares its ID and tool.
func TestOutOfRoster_SinkExplainsOnlyErrors(t *testing.T) {
	o := newOutOfRoster("", nil, mcpGates{})
	o.notes[noteKey{"c1", "t", "tool not found: t"}] = "explained"
	sink := newStreamSink(nil)
	sink.outOfRoster = o
	sink.onToolResult("c1", "t", "tool not found: t", false)
	sink.onToolResult("c1", "t", "tool not found: t", true)
	sink.onToolResult("c1", "other", "tool not found: t", true)
	sink.onToolResult("c1", "t", "boom", true)
	got := make([]string, 0, len(sink.entries))
	for _, e := range sink.entries {
		got = append(got, e.Text)
	}
	if want := []string{"tool not found: t", "explained", "tool not found: t", "boom"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
}

// A deferred tool's arguments get tool_call's own leniency: malformed JSON is
// repaired, and null or a one-object array are accepted as tool_call accepts
// them.
func TestOutOfRoster_DeferredArgumentsAsToolCallTakesThem(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "5")
	for _, tc := range []struct{ input, want string }{
		{`{"text":"hi",}`, `{"text":"hi"}`},
		{`null`, `{}`},
		{`[{"text":"hi"}]`, `{"text":"hi"}`},
		{``, `{}`},
	} {
		t.Run(tc.input, func(t *testing.T) {
			broker := &fakeBroker{}
			model := newOORModel("oor-args", []oorCall{{"c1", "mcp_srv_slack_send_message_0", tc.input}})
			runOOR(t, RunConfig{NativeTools: discNative(3)}, Deps{Model: model, MCPCatalog: discMCPTools(20), MCPBroker: broker})
			if broker.lastTool != "slack_send_message_0" || broker.lastArgs != tc.want {
				t.Fatalf("broker got %q %s, want slack_send_message_0 %s (the model was sent %q)", broker.lastTool, broker.lastArgs, tc.want, model.resultSeen(2, "c1"))
			}
		})
	}
}

// tool_call's input carries the name between quotes as it is, so only a
// name of the MCP spec's tool-name characters is rerouted; the arguments keep
// their own bytes.
func TestOutOfRoster_DeferredCallInputTakesOnlyPlainNames(t *testing.T) {
	if got, ok := deferredCallInput("mcp_srv-1_get.page_2", `{ "a": "<b>" }`); !ok || got != `{"name":"mcp_srv-1_get.page_2","arguments":{ "a": "<b>" }}` {
		t.Fatalf("plain name: %q, %t", got, ok)
	}
	for _, name := range []string{"", `mcp_srv_a"b`, `mcp_srv_a\b`, "mcp_srv_a b", "mcp_srv_é", "mcp_srv_a\nb"} {
		if got, ok := deferredCallInput(name, `{}`); ok {
			t.Errorf("%q was rerouted as %q", name, got)
		}
	}
}

// A deferred tool whose name is not plain is not rerouted. Its result says
// to call tool_call, counted as deferred and not rerouted, and a tool_call
// written as it says runs the tool.
func TestOutOfRoster_DeferredToolWithAnOddNameIsExplained(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "5")
	const odd = "mcp_srv_files/read"
	catalog := append(discMCPTools(20), mcp.ServerTool{ServerName: "srv", Tool: mcp.Tool{
		Name: "files/read", Description: "Read a file", InputSchema: map[string]interface{}{"type": "object"},
	}})
	broker := &fakeBroker{}
	obs := &oorEvents{}
	model := newOORModel("oor-odd-name",
		[]oorCall{{"c1", odd, `{}`}},
		[]oorCall{{"c2", toolNameToolCall, `{"name":"` + odd + `","arguments":{}}`}},
	)
	runOOR(t, RunConfig{NativeTools: discNative(3)}, Deps{Model: model, MCPCatalog: catalog, MCPBroker: broker, Observer: obs})
	want := odd + ` was not called: it is reachable only through tool_call. ` +
		`Call tool_call with {"name": "` + odd + `", "arguments": {...}}, with the arguments as a JSON object.`
	if got := model.resultSeen(2, "c1"); got != want {
		t.Fatalf("result %q, want %q", got, want)
	}
	if broker.lastTool != "files/read" {
		t.Fatalf("the tool_call the result asks for did not run the tool: broker got %q", broker.lastTool)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.events) != 1 || obs.events[0]["reason"] != "deferred" || obs.events[0]["rerouted_to"] != nil {
		t.Fatalf("events = %v, want one deferred, not rerouted", obs.events)
	}
}

// When the persona denies tool_call itself, nothing reroutes: the prompt does
// not promise it, and a direct call to a deferred tool says why it failed.
func TestOutOfRoster_PersonaDeniesTheBridge(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "5")
	broker := &fakeBroker{}
	obs := &oorEvents{}
	var prompt string
	model := newOORModel("oor-no-bridge", []oorCall{{"c1", "mcp_srv_slack_send_message_0", `{}`}})
	inner := model.streamFunc
	model.streamFunc = func(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		for _, m := range call.Prompt {
			for _, p := range m.Content {
				if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok && m.Role == fantasy.MessageRoleSystem {
					prompt = tp.Text
				}
			}
		}
		return inner(ctx, call)
	}
	runOOR(t, RunConfig{NativeTools: discNative(3), PersonaPolicy: &PersonaToolPermissions{Deny: []string{"tool_call"}}},
		Deps{Model: model, MCPCatalog: discMCPTools(20), MCPBroker: broker, Observer: obs})
	const want = "mcp_srv_slack_send_message_0 is not available in this run and was not called: it is reachable only through tool_call, which is not in your tool list."
	if got := model.resultSeen(2, "c1"); got != want || broker.lastTool != "" {
		t.Fatalf("result %q (broker %q), want %q", got, broker.lastTool, want)
	}
	if strings.Contains(prompt, "rerouted") {
		t.Fatalf("the prompt promises rerouting with the bridge denied:\n%s", prompt)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.events) != 1 || obs.events[0]["reason"] != "deferred" || obs.events[0]["rerouted_to"] != nil {
		t.Fatalf("events = %v, want one deferred, not rerouted", obs.events)
	}
}

// After a fallback swap, the count is labelled with the model that made the
// call.
func TestOutOfRoster_CountsTheFallbackModel(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "5")
	t.Setenv("FLEET_RETRY_MAX_ATTEMPTS", "0")
	// Failover is cross-provider, so the primary needs a provider of its own.
	primary := &otherProviderModel{namedMockModel: namedMockModel{name: "oor-primary"}}
	primary.streamFunc = func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		return nil, &fantasy.ProviderError{StatusCode: http.StatusServiceUnavailable, Message: "capacity"}
	}
	fallback := newOORModel("oor-fallback-model", []oorCall{{"c1", "mcp_srv_slack_send_message_0", `{}`}})
	before := oorCount("oor-fallback-model", "deferred")
	runOOR(t, RunConfig{NativeTools: discNative(3)}, Deps{Model: primary, FallbackModel: fallback, MCPCatalog: discMCPTools(20)})
	counts := grepLines(metrics.Render(), "fleet_tool_call_out_of_roster_total{")
	if oorCount("oor-fallback-model", "deferred") != before+1 || strings.Contains(counts, `model="oor-primary"`) {
		t.Fatalf("want the call counted under the fallback model:\n%s", counts)
	}
}

// oorCount is the out-of-roster count for model and reason so far. The
// metrics registry is process-global, so tests compare counts before and
// after a run rather than expect an absolute value.
func oorCount(model, reason string) float64 {
	prefix := `fleet_tool_call_out_of_roster_total{model="` + model + `",reason="` + reason + `"} `
	for _, l := range strings.Split(metrics.Render(), "\n") {
		if v, ok := strings.CutPrefix(l, prefix); ok {
			n, _ := strconv.ParseFloat(v, 64)
			return n
		}
	}
	return 0
}

type otherProviderModel struct{ namedMockModel }

func (m *otherProviderModel) Provider() string { return "other-provider" }

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// resultTextIn is the text of call id's result in msgs (the last one, if
// several), and whether there was one.
func resultTextIn(msgs []fantasy.Message, id string) (string, bool) {
	text, found := "", false
	for _, m := range msgs {
		for _, p := range m.Content {
			tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p)
			if !ok || tr.ToolCallID != id {
				continue
			}
			found = true
			if e, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Output); ok {
				text = e.Error.Error()
			} else if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output); ok {
				text = txt.Text
			}
		}
	}
	return text, found
}

// The finalize hook is handed the round's transcript from the carry, already
// explained, for the recovery call it may make.
func TestOutOfRoster_FinalizeHookIsSentTheExplanation(t *testing.T) {
	var got []fantasy.Message
	runOOR(t, RunConfig{
		NativeTools:   []fantasy.AgentTool{echoTool{name: "secret_tool", out: "ran"}},
		PersonaPolicy: &PersonaToolPermissions{Deny: []string{"secret_tool"}},
	}, Deps{
		Model: newOORModel("oor-finalize", []oorCall{{"c1", "secret_tool", `{}`}}),
		Finalize: func(_ context.Context, in FinalizeInput) (string, error) {
			got = in.Messages
			return in.FinalText, nil
		},
	})
	const want = "secret_tool is not available in this run and was not called: this persona's tool policy does not allow it."
	if text, ok := resultTextIn(got, "c1"); !ok || text != want {
		t.Fatalf("the finalize hook was handed %q (found %t), want the explanation", text, ok)
	}
}

// The explanation step runs before the recovery checkpoint (and the reducers
// and cache markers after it), so a resumed stream replays the explained
// history, not fantasy's text.
func TestOutOfRoster_RecoveryCheckpointHoldsTheExplanation(t *testing.T) {
	model := newOORModel("oor-checkpoint", []oorCall{{"c1", "secret_tool", `{}`}})
	e := newMockEngine(t, model)
	e.outOfRoster = newOutOfRoster("", nil, mcpGates{})
	e.outOfRoster.setRoster(toolRoster{removed: map[string]string{"secret_tool": gatePersona}})
	round := newRoundState(e, newOrchestrationState(e.logSession, 50), 1000)
	round.sink = newStreamSink(nil)
	round.sink.outOfRoster = e.outOfRoster
	if _, err := round.stream(context.Background(), fantasy.NewAgent(model), model, []fantasy.Message{fantasy.NewUserMessage("go")}); err != nil {
		t.Fatal(err)
	}
	const want = "secret_tool is not available in this run and was not called: this persona's tool policy does not allow it."
	if text, ok := resultTextIn(round.recoveryMessages, "c1"); !ok || text != want {
		t.Fatalf("the recovery checkpoint holds %q (found %t), want the explanation", text, ok)
	}
}
