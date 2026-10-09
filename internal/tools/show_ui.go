package tools

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/genui"
)

// ShowUIToolName is the canonical tool name. Exported so the web contract
// test and docs can reference it without re-typing the string.
const ShowUIToolName = "show_ui"

// UISubmissionPrefix opens the user message a card submission sends. The web
// client writes it (web/src/app/chat/ui/genui/submission.ts) and renders a
// message that starts with it as a compact "submitted" bubble; the model is
// told to expect it. Keep the three in step.
const UISubmissionPrefix = "[UI submission]"

// UIReplyPrefix opens the user message a card's quick-reply ("message")
// button sends: "[UI reply] card=<id> action=<id>" on the first line, then the
// button's fixed text. The marker ties the reply to the card that offered it
// (the web locks that card and only that one) instead of guessing from text.
const UIReplyPrefix = "[UI reply]"

// ShowUIParams is the agent-facing argument shape. Components, actions and
// field errors are loosely typed on purpose: the real schema is the recursive
// component catalog that genui.Validate enforces, which a portable tool
// schema cannot express. The description below is the model's catalog.
type ShowUIParams struct {
	Title       string `json:"title" description:"Short card title (a few words)."`
	Description string `json:"description,omitempty" description:"Optional one-line subtitle. May contain {{ expr }} templates."`
	Components  []any  `json:"components" description:"The card body: an array of component objects, each with a \"type\" from the catalog in the tool description."`
	Actions     []any  `json:"actions,omitempty" description:"Footer buttons: [{id, label, kind: submit|message, style: primary|secondary|danger, message, validate, confirm, visible_if, disabled_if}]. Required (with at least one kind=submit) when the card has inputs."`
	Replaces    string `json:"replaces,omitempty" description:"card_id of an earlier card this one updates. The old card collapses to a 'replaced' note so only the newest version is interactive."`
	FieldErrors []any  `json:"field_errors,omitempty" description:"Inline errors to show next to inputs: [{field: \"id\" or \"repeater_id[i].field_id\", message}], where i is the item's 0-based position in the repeater's array (the first item is [0], unlike the 1-based index in expressions). Use when re-showing a card after checking the user's answers; the field must be editable (not disabled)."`
}

const showUIDescription = `Show the user an interactive card inline in the chat — a form, a picker, a multi-item editor, a comparison, a checklist, a small calculator or a chart — built on the fly from a fixed component catalog. Use it when a visual or interactive answer serves the user better than prose: collecting several related values at once (instead of asking one question per message), letting them choose among options you fetched, editing a list of items, reviewing proposed changes, or showing numbers they will want to compare or tweak. Plain questions still get plain text.

HOW IT FLOWS
- The card renders immediately; the call returns a card_id. If the card has inputs, END YOUR TURN right after (at most one short sentence; do not restate the card in prose).
- When the user presses a submit button, their next message starts with "` + UISubmissionPrefix + ` card=<card_id> action=<action_id>" followed by the values as JSON, keyed by input id (a repeater's value is an array of objects keyed by its field ids). Treat those values as the user's answers.
- The card itself never performs actions. YOU act on the submission with your normal tools (MCP tools, files, email, schedule_task…) — their usual approvals still apply.
- To refine (e.g. after validating the answers against a live system), call show_ui again with replaces=<old card_id>, the user's values prefilled via "value", and field_errors for what needs fixing.
- NEVER invent option values that come from an external system (ids, accounts, catalog names). Fetch them with your tools first, then offer them as options; if you cannot fetch them, use a text_input and say so.

COMPONENTS — every component is {"type": ..., ...props}; any component may also take "visible_if": "<expr>" (a condition must read an editable input and be able to hold; a constant or self-contradictory one is refused).
Layout: section {title?, description?, children, collapsible?, collapsed?} · columns {children (2-4, one per column)} · tabs {tabs:[{label, children}], variant?: tabs|steps} · divider {}
Display: heading {text} · text {text, markdown?, tone?: default|muted} · callout {text, title?, tone?: neutral|info|success|warning|danger} · badges {items:[{text, tone?}]} · stat {label, value, caption?, tone?} · facts {items:[{label, value}]} · table {columns:[{key, label?, align?}], rows:[{<key>: string|number|bool|null}], select?: none|single|multi, row_key?, id?, value?, required?, disabled? (the last four only when select is single|multi)} · status_list {items:[{status: pass|fail|warn|info|pending, label, detail?, field? (an input path, same form as field_errors)}]} · progress {value (expr), max?, label?} · chart {kind: bar|line, labels:[...], series:[{name, values:[numbers]}], title?, unit?} · diff {rows:[{label, before?, after?}], title?} · code {text, language?} · link {text, url (https)}
Inputs (all take id (required, unique), label?, help?, required?, disabled?, value? = default):
  text_input {placeholder?, multiline?, min_length?, max_length?, format?: text|email|url} · number {min?, max?, step?, prefix?, suffix?} · slider {min, max, step?, prefix?, suffix?} · select {options} · choice {options, variant?: segmented|radio} · multi_select {options?, allow_custom?, max_items?} · toggle {} · date {min?, max?} (YYYY-MM-DD) · list_input {placeholder?, max_items?, dedupe?} (paste one item per line → string array) · include_exclude {options?, allow_custom?, include_label?, exclude_label?} (value {include:[], exclude:[]}) · repeater {fields:[components], item_label?, min_items?, max_items?, add_label?} (a list of items the user can add/remove/duplicate; value is [{field_id: value}]; no nested repeaters)
Options are strings or {value, label?, description?}. A selectable table submits the row_key value(s) of the chosen rows under its id.

EXPRESSIONS — used in {{ }} templates (text, heading, callout, stat value/caption, facts value, status_list label/detail, badges text, description, help, item_label) and in visible_if / disabled_if / progress value. Read input ids by name; inside a repeater's fields also that item's fields and index (1-based); from outside, repeater_id.field is the array of that field across items. Operators: + - * / % == != < <= > >= && || ! ?: ( ) and literals ('text', 12.5, true, false, null). Functions: ` + "len count sum avg min max abs floor ceil round(x, digits?) fixed(x, digits) number string upper lower join(list, sep?) contains(list|text, x) empty unique" + `. Example: "{{ count(lines) * len(exchanges) }} deals", visible_if "channel == 'Video'".

ACTIONS — {id, label, kind: "submit" (default; sends the values) | "message" (sends the fixed "message" text as the user's reply, after a "` + UIReplyPrefix + ` card=<card_id> action=<action_id>" line — use for quick-reply buttons), style?, validate? (default true: required fields must be filled), confirm? (a confirmation question), visible_if?, disabled_if?}; a button hidden or disabled in every state is refused. At most 6.

The spec is validated: a mistake comes back as a tool error naming the path — fix it and call again. Keep cards focused (one task per card); limits: 128 KiB per card, 800 components, 500 table rows (and 500 entries per badges / facts / status_list / diff list; 2000 rows, entries and repeater item fields across the card, and a repeater's max_items (200 when unset) times its fields within 2000; 3200 chart points × series), 2000 options per input.`

// NewShowUITool returns the show_ui tool. Its Run validates the spec and
// reports the outcome to the model; the card itself is drawn by the web
// client from the tool call's own arguments (which every client already
// receives as the tool.call event and which the transcript persists), so
// there is no staging row, no new SSE event and nothing to replay on reload.
func NewShowUITool() fantasy.AgentTool {
	return fantasy.NewAgentTool(ShowUIToolName, showUIDescription,
		func(_ context.Context, _ ShowUIParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return ShowUIResult(call.ID, []byte(call.Input)), nil
		})
}

// ShowUIRedactor is the secret scrubber the agent loop applies to every tool
// call's input before it reaches the stream, the transcript and the model
// replay (set by internal/agentcore; nil in tests that do not need it).
var ShowUIRedactor func(string) string

// ShowUIResult builds the tool response for one call. Exported for tests.
func ShowUIResult(callID string, input []byte) fantasy.ToolResponse {
	// The browser draws the card from the REDACTED input: a value that
	// matches a secret pattern becomes "[REDACTED]" there, so two such
	// options would collapse into one and the answer could not say which was
	// picked. Refuse such a card rather than validate what nobody sees.
	if ShowUIRedactor != nil {
		if s := string(input); ShowUIRedactor(s) != s {
			return fantasy.NewTextErrorResponse("UI_INVALID: the card was NOT shown — it contains a value that looks like a secret (an API key or token), which is redacted before the user sees the card. Do not put secrets in a card; use a label or an id that is not a credential, and call " + ShowUIToolName + " again.")
		}
	}
	card, issues := genui.Validate(input)
	if len(issues) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "UI_INVALID: the card was NOT shown — the spec has %d problem(s). Fix them and call %s again:\n", len(issues), ShowUIToolName)
		for _, is := range issues {
			b.WriteString("- ")
			b.WriteString(is.String())
			b.WriteByte('\n')
		}
		return fantasy.NewTextErrorResponse(strings.TrimRight(b.String(), "\n"))
	}
	interactive := hasSubmitAction(card.Actions)
	var b strings.Builder
	// The acknowledgement stays small whatever the card holds: a result over
	// the tool-output ceiling is replaced by an envelope, and the browser
	// draws a card only when its result starts with UI_DISPLAYED.
	fmt.Fprintf(&b, "UI_DISPLAYED card_id=%s: the user can now see the card %q.", callID, genui.TruncateRunes(card.Title, 120))
	if card.Replaces != "" {
		fmt.Fprintf(&b, " It replaces card %s, which is now collapsed.", card.Replaces)
	}
	switch {
	case interactive:
		fmt.Fprintf(&b, " Do not restate its contents. End your turn now with at most one short sentence. When the user submits, their next message will start with %q followed by the values as JSON; act on those values with your normal tools.", fmt.Sprintf("%s card=%s action=<action_id>", UISubmissionPrefix, callID))
	case len(card.Actions) > 0:
		fmt.Fprintf(&b, " Its buttons send their fixed message as the user's next reply (after a %q line); end your turn now with at most one short sentence.", fmt.Sprintf("%s card=%s action=<action_id>", UIReplyPrefix, callID))
	default:
		b.WriteString(" It is display-only; continue your answer without repeating what the card shows.")
	}
	return fantasy.NewTextResponse(b.String())
}

func hasSubmitAction(actions []any) bool {
	for _, a := range actions {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		if k, _ := m["kind"].(string); k == "" || k == "submit" {
			return true
		}
	}
	return false
}
