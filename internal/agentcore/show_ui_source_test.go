package agentcore

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/tools"
)

const sourceCard = `{"title":"Intake","components":[{"type":"text_input","id":"advertiser","label":"Advertiser"}],"actions":[{"id":"go","label":"Go"}]}`

// toolResultEvents records every tool.result payload.
type toolResultEvents struct {
	mu     sync.Mutex
	events []map[string]any
}

func (o *toolResultEvents) Observe(event string, payload map[string]any) {
	if event != "tool.result" {
		return
	}
	o.mu.Lock()
	o.events = append(o.events, payload)
	o.mu.Unlock()
}

func (o *toolResultEvents) forID(id string) map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, e := range o.events {
		if e["id"] == id {
			return e
		}
	}
	return nil
}

// cardBroker answers every MCP call with a result that carries a card.
type cardBroker struct{}

func (cardBroker) CallMCP(context.Context, string, string, map[string]any) (string, bool, error) {
	return `{"plan_id":"p1","show_ui":` + sourceCard + `}`, false, nil
}

func entryFor(res Result, id string) *RunEntry {
	for i := range res.Entries {
		if res.Entries[i].ToolCallID == id && res.Entries[i].Type == "tool_result" {
			return &res.Entries[i]
		}
	}
	return nil
}

func runShowUISource(t *testing.T, cfg RunConfig, deps Deps) Result {
	t.Helper()
	cfg.EnvPrefix = CanonicalEnvPrefix
	cfg.NativeTools = append(cfg.NativeTools, tools.NewShowUITool())
	if deps.Policy == nil {
		deps.Policy = NewInteractivePolicy(0, 0, nil, nil)
	}
	if deps.Input == nil {
		deps.Input = historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("go")}, label: "ui"}
	}
	res, err := Run(context.Background(), ModeInteractive, cfg, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// In one turn: a deferred MCP tool (reached through tool_call) returns a
// card, and show_ui names that tool. The card is resolved from the result,
// rides the show_ui result's entry and tool.result event as display data,
// and the model is sent only the acknowledgement.
func TestShowUISource_ThisRunsResultThroughTheBridge(t *testing.T) {
	t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", "5")
	obs := &toolResultEvents{}
	model := newOORModel("ui-bridge",
		[]oorCall{{"c1", toolNameToolCall, `{"name":"mcp_srv_jira_create_issue_1","arguments":{}}`}},
		[]oorCall{{"ui1", tools.ShowUIToolName, `{"from_result":{"tool":"jira_create_issue_1"}}`}},
		[]oorCall{{"c3", "native_0", `{}`}},
	)
	res := runShowUISource(t, RunConfig{NativeTools: discNative(3)},
		Deps{Model: model, Observer: obs, MCPCatalog: discMCPTools(20), MCPBroker: cardBroker{}})

	e := entryFor(res, "ui1")
	if e == nil || e.IsErr || !strings.HasPrefix(e.Text, "UI_DISPLAYED card_id=ui1") {
		t.Fatalf("show_ui result = %+v", e)
	}
	if !jsonEqual(t, e.Card, sourceCard) {
		t.Fatalf("entry card = %q", e.Card)
	}
	if ev := obs.forID("ui1"); ev == nil || !jsonEqual(t, ev["card"].(string), sourceCard) {
		t.Fatalf("tool.result event = %v", ev)
	}
	if seen := model.resultSeen(3, "ui1"); !strings.HasPrefix(seen, "UI_DISPLAYED") || strings.Contains(seen, "advertiser") {
		t.Fatalf("the model was sent %q", seen)
	}
	// Every other result carries no card.
	if c1 := entryFor(res, "c1"); c1 == nil || c1.Card != "" {
		t.Fatalf("the source result carries a card: %+v", c1)
	}
	if ev := obs.forID("c1"); ev == nil || ev["card"] != nil {
		t.Fatalf("a non-card tool.result has a card field: %v", ev)
	}
}

// A result from an earlier turn (the replayed history) can be referenced by
// its call id; a refused reference carries no card.
func TestShowUISource_EarlierTurnAndRefusal(t *testing.T) {
	seed := []fantasy.Message{
		fantasy.NewUserMessage("start"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "old1", ToolName: "mcp_deal_plan_intake_card", Input: `{}`}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "old1", Output: fantasy.ToolResultOutputContentText{Text: `{"show_ui":` + sourceCard + `}`}}}},
		fantasy.NewUserMessage("show it"),
	}
	obs := &toolResultEvents{}
	model := newOORModel("ui-seed",
		[]oorCall{{"ui1", tools.ShowUIToolName, `{"from_result":{"call_id":"old1"}}`}, {"ui2", tools.ShowUIToolName, `{"from_result":{"call_id":"nope"}}`}},
		[]oorCall{{"c3", "native_0", `{}`}},
	)
	res := runShowUISource(t, RunConfig{NativeTools: discNative(1)},
		Deps{Model: model, Observer: obs, Input: historyInput{system: "s", msgs: seed, label: "ui"}})
	if e := entryFor(res, "ui1"); e == nil || !jsonEqual(t, e.Card, sourceCard) {
		t.Fatalf("seed reference: %+v", e)
	}
	if e := entryFor(res, "ui2"); e == nil || !e.IsErr || e.Card != "" || !strings.HasPrefix(e.Text, "UI_INVALID") {
		t.Fatalf("refused reference: %+v", e)
	}
}

func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}

// The sink attaches a held card only to the show_ui success it was resolved
// for, matched by its acknowledgement; a failure or a rewritten result gets
// none.
func TestShowUISource_CardOnlyOnTheSuccess(t *testing.T) {
	s := newStreamSink(nil)
	s.noteShownCard("a", "UI_DISPLAYED card_id=a", sourceCard)
	s.onToolResult("a", tools.ShowUIToolName, "UI_INVALID: governance said no", true)
	s.noteShownCard("b", "UI_DISPLAYED card_id=b", sourceCard)
	s.onToolResult("b", "bash", "UI_DISPLAYED card_id=b", false)
	s.noteShownCard("c", "UI_DISPLAYED card_id=c", sourceCard)
	s.onToolResult("c", tools.ShowUIToolName, "UI_DISPLAYED card_id=c", false)
	for _, e := range s.entries {
		if (e.ToolCallID == "c") != (e.Card != "") {
			t.Fatalf("entry %s card = %q", e.ToolCallID, e.Card)
		}
	}
	if len(s.shownCards["c"]) != 0 {
		t.Fatalf("the consumed card is still held: %v", s.shownCards)
	}
}

// Two parallel by-reference show_ui calls that share a reused id each get
// their own card, whatever order they finish in (Codex P2 on #1819).
func TestShowUISource_ReusedIDKeepsEachCard(t *testing.T) {
	s := newStreamSink(nil)
	one := `UI_DISPLAYED card_id=dup: the user can now see the card "One".`
	two := `UI_DISPLAYED card_id=dup: the user can now see the card "Two".`
	s.noteShownCard("dup", one, `{"title":"One"}`)
	s.noteShownCard("dup", two, `{"title":"Two"}`)
	s.onToolResult("dup", tools.ShowUIToolName, two, false)
	s.onToolResult("dup", tools.ShowUIToolName, one, false)
	got := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		got = append(got, e.Card)
	}
	if len(got) != 2 || got[0] != `{"title":"Two"}` || got[1] != `{"title":"One"}` {
		t.Fatalf("cards = %q", got)
	}
}

// "Newest" is the later CALL, not the later finish: two parallel calls to the
// same card tool that complete out of order resolve to the same result in
// this run as after replay, where results follow call order (Codex P2 on
// #1819).
func TestShowUISource_NewestMeansTheLaterCall(t *testing.T) {
	s := newStreamSink(nil)
	s.onToolCall("early", "mcp_p_intake_card", `{}`)
	s.onToolCall("late", "mcp_p_intake_card", `{}`)
	s.onToolResult("late", "mcp_p_intake_card", `{"n":2}`, false)  // finishes first
	s.onToolResult("early", "mcp_p_intake_card", `{"n":1}`, false) // finishes last
	run := s.priorToolResults("")
	if len(run) != 2 || run[1].CallID != "late" {
		t.Fatalf("this run's results oldest call first = %+v", run)
	}
	seed := priorToolResultsFromMessages([]fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "early", ToolName: "mcp_p_intake_card", Input: `{}`},
			fantasy.ToolCallPart{ToolCallID: "late", ToolName: "mcp_p_intake_card", Input: `{}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "late", Output: fantasy.ToolResultOutputContentText{Text: `{"n":2}`}},
			fantasy.ToolResultPart{ToolCallID: "early", Output: fantasy.ToolResultOutputContentText{Text: `{"n":1}`}},
		}},
	})
	if len(seed) != 2 || seed[1].CallID != "late" || seed[1].Tool != "mcp_p_intake_card" {
		t.Fatalf("replayed results oldest call first = %+v", seed)
	}
}

// A reused call id keeps each occurrence's position and tool: results answer
// the oldest open call with their id, as the web binds them, so neither
// result takes the later call's name or position (Codex P2 on #1819).
func TestShowUISource_ReusedIDKeepsEachCallsPlace(t *testing.T) {
	s := newStreamSink(nil)
	s.onToolCall("dup", "mcp_p_intake_card", `{}`)
	s.onToolCall("other", "bash", `{}`)
	s.onToolCall("dup", "mcp_p_notes", `{}`)
	s.onToolResult("dup", "mcp_p_intake_card", `{"n":1}`, false)
	s.onToolResult("other", "bash", `{}`, false)
	s.onToolResult("dup", "mcp_p_notes", `{"n":2}`, false)
	got := s.priorToolResults("")
	want := []string{"dup mcp_p_intake_card", "other bash", "dup mcp_p_notes"}
	if len(got) != len(want) {
		t.Fatalf("results = %+v", got)
	}
	for i, r := range got {
		if r.CallID+" "+r.Tool != want[i] {
			t.Fatalf("result %d = %s %s, want %s (all: %+v)", i, r.CallID, r.Tool, want[i], got)
		}
	}
}

// A post_tool_use hook may append its context to show_ui's acknowledgement;
// the card still rides that result (Codex P2 on #1819). A text that merely
// starts the same way without the hook separator does not match.
func TestShowUISource_HookFragmentKeepsTheCard(t *testing.T) {
	s := newStreamSink(nil)
	ack := `UI_DISPLAYED card_id=u1: the user can now see the card "One".`
	s.noteShownCard("u1", ack, `{"title":"One"}`)
	s.onToolResult("u1", tools.ShowUIToolName, appendHookContext(ack, "hook says hi"), false)
	s.noteShownCard("u2", ack, `{"title":"Two"}`)
	s.onToolResult("u2", tools.ShowUIToolName, ack+" and more", false)
	if len(s.entries) != 2 || s.entries[0].Card != `{"title":"One"}` || s.entries[1].Card != "" {
		t.Fatalf("entries = %+v", s.entries)
	}
}

// A sibling call of the show_ui call's own step is never a candidate, even
// when it finished first: the model has not seen its result (Codex P2 on
// #1819). Results recorded before the show_ui call are.
func TestShowUISource_SameStepSiblingIsNotSeen(t *testing.T) {
	s := newStreamSink(nil)
	s.onToolCall("old", "mcp_p_intake_card", `{}`)
	s.onToolResult("old", "mcp_p_intake_card", `{"n":1}`, false)
	// The next step: the card tool and show_ui together.
	s.onToolCall("sib", "mcp_p_intake_card", `{}`)
	s.onToolCall("ui", tools.ShowUIToolName, `{"from_result":{"tool":"intake_card"}}`)
	s.onToolResult("sib", "mcp_p_intake_card", `{"n":2}`, false) // finished before show_ui ran
	got := s.priorToolResults("ui")
	if len(got) != 1 || got[0].CallID != "old" {
		t.Fatalf("candidates for ui = %+v, want only the earlier result", got)
	}
	if all := s.priorToolResults(""); len(all) != 2 {
		t.Fatalf("all results = %+v", all)
	}
}
