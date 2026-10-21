package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// refCard is a card a tool built, as the deal_plan server's intake_card
// returns it: under "show_ui", next to other fields.
const refCard = `{"title":"Intake","components":[{"type":"text_input","id":"advertiser","label":"Advertiser","required":true}],"actions":[{"id":"go","label":"Go"}]}`

type shownCards map[string]string

func refSeam(results []PriorToolResult, shown shownCards) ShowUISeam {
	return ShowUISeam{
		Results:  func(string) []PriorToolResult { return results },
		ShowCard: func(id, _, spec string) { shown[id] = spec },
	}
}

func runShowUIWith(t *testing.T, seam *ShowUISeam, args string) fantasy.ToolResponse {
	t.Helper()
	ctx := context.Background()
	if seam != nil {
		ctx = WithShowUISeam(ctx, *seam)
	}
	resp, err := NewShowUITool().Run(ctx, fantasy.ToolCall{ID: "ui_1", Name: ShowUIToolName, Input: args})
	if err != nil {
		t.Fatalf("Run returned a Go error (fatal to the loop): %v", err)
	}
	return resp
}

// A reference by call id or by tool name shows the tool's card: the model
// gets the ordinary small acknowledgement and the validated spec goes to the
// transcript, not to the model.
func TestShowUIRef_ShowsTheToolsCard(t *testing.T) {
	results := []PriorToolResult{
		{CallID: "c3", Tool: "mcp_cards_show_ui", Text: `{"show_ui":` + refCard + `}`},
		{CallID: "c2", Tool: "mcp_deal_plan_intake_card", Text: `{"ok":true,"show_ui":` + refCard + `}`},
		{CallID: "c1", Tool: "mcp_deal_plan_intake_card", Text: `{"show_ui":{"title":"Old","components":[]}}`},
	}
	for name, args := range map[string]string{
		"call id":                   `{"from_result":{"call_id":"c2"}}`,
		"an MCP tool named show_ui": `{"from_result":{"tool":"mcp_cards_show_ui"}}`,
		"tool":                      `{"from_result":{"tool":"mcp_deal_plan_intake_card"}}`,
		"tool suffix":               `{"from_result":{"tool":"intake_card","path":"show_ui"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			shown := shownCards{}
			seam := refSeam(results, shown)
			resp := runShowUIWith(t, &seam, args)
			if resp.IsError || !strings.HasPrefix(resp.Content, "UI_DISPLAYED card_id=ui_1") || strings.Contains(resp.Content, "advertiser") {
				t.Fatalf("resp %+v", resp)
			}
			if mustCompact(t, shown["ui_1"]) != mustCompact(t, refCard) {
				t.Fatalf("shown card = %s, want %s", shown["ui_1"], refCard)
			}
		})
	}
}

func mustCompact(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// replaces and field_errors ride beside the reference and are laid over the
// tool's card, then validated with it (a field_errors entry must name a real
// field).
func TestShowUIRef_Overrides(t *testing.T) {
	results := []PriorToolResult{{CallID: "c1", Tool: "mcp_p_intake_card", Text: `{"show_ui":` + refCard + `}`}}
	shown := shownCards{}
	seam := refSeam(results, shown)
	resp := runShowUIWith(t, &seam, `{"from_result":{"call_id":"c1"},"replaces":"old_card","field_errors":[{"field":"advertiser","message":"Unknown advertiser"}]}`)
	if resp.IsError || !strings.Contains(resp.Content, "replaces card old_card") {
		t.Fatalf("resp %+v", resp)
	}
	var spec map[string]any
	if err := json.Unmarshal([]byte(shown["ui_1"]), &spec); err != nil || spec["replaces"] != "old_card" || spec["field_errors"] == nil {
		t.Fatalf("shown spec %s (%v)", shown["ui_1"], err)
	}
	bad := runShowUIWith(t, &seam, `{"from_result":{"call_id":"c1"},"field_errors":[{"field":"nope","message":"x"}]}`)
	if !bad.IsError || !strings.HasPrefix(bad.Content, "UI_INVALID") {
		t.Fatalf("a field_errors entry naming no field was accepted: %+v", bad)
	}
}

// A nested path, and a card a script printed (a JSON string at the path).
func TestShowUIRef_PathAndStringEncodedCard(t *testing.T) {
	printed, _ := json.Marshal(refCard + "\n")
	wrapped, _ := json.Marshal(`{"show_ui":` + refCard + "}\n")
	results := []PriorToolResult{
		{CallID: "nested", Tool: "mcp_s_t", Text: `{"data":{"card":` + refCard + `}}`},
		{CallID: "script", Tool: "bash", Text: `{"exit_code":0,"stdout":` + string(printed) + `}`},
		{CallID: "wrapped", Tool: "bash", Text: `{"exit_code":0,"stdout":` + string(wrapped) + `}`},
	}
	for args, id := range map[string]string{
		`{"from_result":{"call_id":"nested","path":"data.card"}}`:       "nested",
		`{"from_result":{"call_id":"script","path":"stdout"}}`:          "script",
		`{"from_result":{"call_id":"wrapped","path":"stdout.show_ui"}}`: "wrapped",
	} {
		shown := shownCards{}
		seam := refSeam(results, shown)
		if resp := runShowUIWith(t, &seam, args); resp.IsError || shown["ui_1"] == "" {
			t.Fatalf("%s: %+v", id, resp)
		}
	}
}

// Every refusal is a UI_INVALID tool error the model can act on, and shows
// nothing.
func TestShowUIRef_Refusals(t *testing.T) {
	results := []PriorToolResult{
		{CallID: "err", Tool: "mcp_p_intake_card", Text: "vendor said no", IsErr: true},
		{CallID: "ok", Tool: "mcp_p_other", Text: `{"show_ui":` + refCard + `}`},
		{CallID: "prose", Tool: "mcp_p_notes", Text: "not json"},
		{CallID: "bad", Tool: "mcp_p_bad", Text: `{"show_ui":{"title":"T","components":[{"type":"nope"}]}}`},
		{CallID: "secret", Tool: "mcp_p_secret", Text: `{"show_ui":{"title":"[REDACTED]","components":[]}}`},
		{CallID: "escaped", Tool: "mcp_p_escaped", Text: `{"show_ui":{"title":"T","components":[{"type":"select","id":"s","options":["a","\u005bREDACTED\u005d"]}]}}`},
		{CallID: "ui_0", Tool: ShowUIToolName, Text: "UI_DISPLAYED card_id=ui_0"},
		{CallID: "old", Tool: "mcp_p_intake_card", Text: `{"show_ui":` + refCard + `}`},
	}
	cases := map[string]struct{ args, want string }{
		"newest result is an error":     {`{"from_result":{"tool":"intake_card"}}`, "is an error"},
		"unknown call":                  {`{"from_result":{"call_id":"zzz"}}`, "no result of call"},
		"call id of another tool":       {`{"from_result":{"call_id":"ok","tool":"intake_card"}}`, "no result of call"},
		"not JSON":                      {`{"from_result":{"call_id":"prose"}}`, "not a JSON object"},
		"no such key":                   {`{"from_result":{"call_id":"ok","path":"card"}}`, `no key "card"`},
		"referenced card invalid":       {`{"from_result":{"call_id":"bad"}}`, "the card in the referenced result has"},
		"redacted value":                {`{"from_result":{"call_id":"secret"}}`, "redacted value"},
		"JSON-escaped redacted value":   {`{"from_result":{"call_id":"escaped"}}`, "redacted value"},
		"an earlier show_ui call":       {`{"from_result":{"call_id":"ui_0"}}`, "no result of call"},
		"show_ui named as the tool":     {`{"from_result":{"tool":"show_ui"}}`, "never an earlier show_ui call"},
		"neither call id nor tool":      {`{"from_result":{"path":"show_ui"}}`, "needs call_id"},
		"inline fields beside a ref":    {`{"from_result":{"call_id":"ok"},"title":"Mine"}`, `not "title"`},
		"no ref field at all is inline": {`{"title":"","components":[]}`, "UI_INVALID"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			shown := shownCards{}
			seam := refSeam(results, shown)
			resp := runShowUIWith(t, &seam, tc.args)
			if !resp.IsError || !strings.HasPrefix(resp.Content, "UI_INVALID") || !strings.Contains(resp.Content, tc.want) {
				t.Fatalf("resp %+v, want an error containing %q", resp, tc.want)
			}
			if len(shown) != 0 {
				t.Fatalf("a refused card was shown: %v", shown)
			}
		})
	}
	// Without the run's seam (a caller outside the loop) a reference is
	// refused and an inline card is unaffected.
	if resp := runShowUIWith(t, nil, `{"from_result":{"call_id":"ok"}}`); !resp.IsError || !strings.Contains(resp.Content, "cannot resolve from_result") {
		t.Fatalf("no seam: %+v", resp)
	}
	if resp := runShowUIWith(t, nil, refCard); resp.IsError {
		t.Fatalf("an inline card needs no seam: %+v", resp)
	}
}

// The description steers the model to the reference when a tool built the
// card, and away from rebuilding it.
func TestShowUIDescriptionPrefersTheReference(t *testing.T) {
	desc := NewShowUITool().Info().Description
	for _, want := range []string{"from_result", "do NOT copy it out", "do not rebuild it"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q", want)
		}
	}
	params := NewShowUITool().Info().Parameters
	if _, ok := params["from_result"]; !ok {
		t.Fatalf("schema lacks from_result: %v", params)
	}
	for _, r := range NewShowUITool().Info().Required {
		if r == "components" || r == "title" {
			t.Fatalf("%s is required by the schema, so a reference alone would be refused before show_ui runs", r)
		}
	}
}
