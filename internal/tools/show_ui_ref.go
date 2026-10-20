package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/redact"
)

// show_ui by reference (docs/GENERATIVE-UI.md "A card a tool built",
// ADR-0086).
//
// Bundles build cards in code: a tool returns {"show_ui": {...}} (3-13 KB).
// Copying that spec into show_ui costs output tokens on every use and is
// fragile: a model that copied it correctly then decided it had "hand-built"
// the card, redrew it, and narrated its internals to the user. With
// from_result the model names the tool result instead, and fleet loads the
// spec from it, validates it with genui.Validate exactly as an inline spec,
// and hands the validated spec to the transcript as display data on the
// show_ui result (ShowUISeam.ShowCard), so a reload and a second tab draw the
// same card. The card is still declarative data; nothing executes.

// PriorToolResult is one tool result the model can see in this conversation:
// the replayed history the turn was given plus this run's results so far.
type PriorToolResult struct {
	CallID string
	// Tool is the model-facing tool name. A call made through the deferred
	// tool_call bridge is named by the tool it ran, not "tool_call".
	Tool  string
	Text  string
	IsErr bool
}

// ShowUISeam is what the run gives show_ui to resolve a reference. The agent
// loop installs it on the tool's context (WithShowUISeam); without it a
// reference is refused, and an inline card works exactly as before.
type ShowUISeam struct {
	// Results returns the tool results the model can see in this
	// conversation, newest first.
	Results func() []PriorToolResult
	// ShowCard records the validated spec for callID's show_ui result. The
	// loop attaches it to that result's transcript entry and stream event
	// (only when the result is the UI_DISPLAYED success), never to what the
	// model is sent.
	ShowCard func(callID, spec string)
}

type showUISeamKey struct{}

// WithShowUISeam installs the run's seam on ctx.
func WithShowUISeam(ctx context.Context, s ShowUISeam) context.Context {
	return context.WithValue(ctx, showUISeamKey{}, s)
}

func showUISeamFrom(ctx context.Context) (ShowUISeam, bool) {
	s, ok := ctx.Value(showUISeamKey{}).(ShowUISeam)
	return s, ok && s.Results != nil && s.ShowCard != nil
}

// ShowUIRef is the from_result argument.
type ShowUIRef struct {
	CallID string `json:"call_id,omitempty" description:"The id of the tool call whose result holds the card. Give this or tool."`
	Tool   string `json:"tool,omitempty" description:"The tool whose NEWEST result holds the card (its name as you call it, e.g. mcp_deal_plan_intake_card; a name suffix such as intake_card also matches). Use when you do not have the call id."`
	Path   string `json:"path,omitempty" description:"Dot-separated keys to the card inside the result's JSON. Default \"show_ui\". A string value on the way that holds JSON is parsed (a printed card: \"stdout.show_ui\")."`
}

// DefaultShowUIRefPath is where a tool puts the card it built when the
// reference names no path.
const DefaultShowUIRefPath = "show_ui"

// showUIRefOverrides are the only keys a reference may carry beside
// from_result: what a refinement needs on top of the tool's card.
var showUIRefOverrides = map[string]bool{"replaces": true, "field_errors": true}

// uiInvalid formats a refusal the model can act on. It starts with
// UI_INVALID, like every refusal, so no client draws a card for it.
func uiInvalid(format string, args ...any) fantasy.ToolResponse {
	return fantasy.NewTextErrorResponse("UI_INVALID: the card was NOT shown — " + fmt.Sprintf(format, args...))
}

// resolveShowUIRef builds the card a reference names: the object at path in
// the referenced result, with replaces / field_errors from the call laid
// over it. It returns the spec bytes to validate, or a refusal.
func resolveShowUIRef(ctx context.Context, input map[string]json.RawMessage) ([]byte, *fantasy.ToolResponse) {
	fail := func(format string, args ...any) ([]byte, *fantasy.ToolResponse) {
		r := uiInvalid(format, args...)
		return nil, &r
	}
	for k := range input {
		if k != "from_result" && !showUIRefOverrides[k] {
			return fail("with from_result, the card comes from the tool's result; only replaces and field_errors may be given beside it, not %q. Drop %q and call %s again.", k, k, ShowUIToolName)
		}
	}
	var ref ShowUIRef
	if err := json.Unmarshal(input["from_result"], &ref); err != nil {
		return fail("from_result must be an object {call_id or tool, path?}: %v", err)
	}
	ref.CallID, ref.Tool, ref.Path = strings.TrimSpace(ref.CallID), strings.TrimSpace(ref.Tool), strings.TrimSpace(ref.Path)
	if ref.CallID == "" && ref.Tool == "" {
		return fail("from_result needs call_id (the id of the tool call whose result holds the card) or tool (the tool whose newest result holds it).")
	}
	if ref.Tool == ShowUIToolName || strings.HasSuffix(ref.Tool, "_"+ShowUIToolName) {
		return fail("from_result names a tool's result that holds a card, never an earlier %s call. To change a card you showed, call %s again with replaces.", ShowUIToolName, ShowUIToolName)
	}
	seam, ok := showUISeamFrom(ctx)
	if !ok {
		return fail("this run cannot resolve from_result. Pass the card inline (title, components, actions).")
	}
	res, found := findPriorResult(seam.Results(), ref)
	if !found {
		what := fmt.Sprintf("call %q", ref.CallID)
		if ref.CallID == "" {
			what = fmt.Sprintf("tool %q", ref.Tool)
		}
		return fail("no result of %s is in this conversation. Reference a tool result you have already received (a call in the same step as %s has not returned yet), or pass the card inline.", what, ShowUIToolName)
	}
	if res.IsErr {
		return fail("the result of %s (call %s) is an error, not a card. Fix that call first.", res.Tool, res.CallID)
	}
	path := ref.Path
	if path == "" {
		path = DefaultShowUIRefPath
	}
	spec, err := cardAtPath(res.Text, path)
	if err != nil {
		return fail("the result of %s (call %s) has no card at %q: %v.", res.Tool, res.CallID, path, err)
	}
	for k := range showUIRefOverrides {
		if raw, ok := input[k]; ok {
			spec[k] = raw
		}
	}
	out, err := json.Marshal(spec)
	if err != nil {
		return fail("the card could not be encoded: %v", err)
	}
	// A value the redaction already scrubbed from the result would show as
	// "[REDACTED]" and, in an option, collapse two choices into one.
	if hasRedactionMarker(out) {
		return fail("the card in the result of %s contains a redacted value, so it cannot be shown as built. Ask the tool for a card without secrets.", res.Tool)
	}
	return out, nil
}

// findPriorResult picks the result a reference names. A call id selects the
// newest result with that id (and, when tool is also given, that tool); a
// tool alone selects that tool's newest result, matched by exact name or by
// the critical_tools suffix rule. The newest one is final: an older success
// never stands in for a newer failure.
func findPriorResult(results []PriorToolResult, ref ShowUIRef) (PriorToolResult, bool) {
	toolMatches := func(name string) bool {
		return ref.Tool == "" || name == ref.Tool || strings.HasSuffix(name, "_"+ref.Tool)
	}
	for _, r := range results {
		if ref.CallID != "" && r.CallID != ref.CallID {
			continue
		}
		if ref.CallID == "" && r.Tool == ShowUIToolName {
			continue
		}
		if r.Tool != ShowUIToolName && toolMatches(r.Tool) {
			return r, true
		}
		if ref.CallID != "" {
			// The newest call with that id is another tool's (or a show_ui
			// call's): the reference names nothing usable.
			return PriorToolResult{}, false
		}
	}
	return PriorToolResult{}, false
}

// cardAtPath decodes text as JSON and walks the dot-separated keys of path to
// a JSON object, which it returns as a mutable key → raw-value map. A JSON
// string met on the way is decoded as JSON and walked into, for a card a
// script printed (bash's {"stdout": "..."}: path "stdout.show_ui").
func cardAtPath(text, path string) (map[string]json.RawMessage, error) {
	cur := json.RawMessage(bytes.TrimSpace([]byte(text)))
	if len(cur) == 0 || cur[0] != '{' {
		return nil, fmt.Errorf("the result is not a JSON object")
	}
	for _, key := range strings.Split(path, ".") {
		cur = unwrapJSONString(cur)
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(cur, &obj); err != nil {
			return nil, fmt.Errorf("it is not a JSON object above %q", key)
		}
		next, ok := obj[key]
		if !ok {
			return nil, fmt.Errorf("there is no key %q", key)
		}
		cur = next
	}
	cur = unwrapJSONString(cur)
	var card map[string]json.RawMessage
	if err := json.Unmarshal(cur, &card); err != nil || card == nil {
		return nil, fmt.Errorf("the value there is not a JSON object")
	}
	return card, nil
}

// unwrapJSONString returns the text of a JSON string value as JSON, and any
// other value unchanged.
func unwrapJSONString(v json.RawMessage) json.RawMessage {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return json.RawMessage(strings.TrimSpace(s))
	}
	return v
}

// hasRedactionMarker reports whether a card carries the redactor's marker in
// its wire text or in any string it decodes to, key or value, at any depth:
// a JSON escape ("\u005bREDACTED\u005d") hides it from a scan of the raw
// bytes, but the browser shows the decoded text (Codex P2 on #1819).
func hasRedactionMarker(card []byte) bool {
	if bytes.Contains(card, []byte(redact.MarkerPrefix)) {
		return true
	}
	// decodedRedacted asks "would this function change any decoded string";
	// a probe that changes exactly the strings holding the marker answers
	// "does any decoded string hold it".
	return decodedRedacted(card, func(s string) string {
		if strings.Contains(s, redact.MarkerPrefix) {
			return ""
		}
		return s
	})
}
