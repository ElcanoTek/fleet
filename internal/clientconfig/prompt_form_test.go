package clientconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// campaignFormPrompt is the shape a client bundle ships: a select with a
// default, required text and textarea fields, and optional ("advanced") fields
// the template mentions on lines of their own.
const campaignFormPrompt = `---
name: "New campaign page from a template"
description: "A campaign dashboard built from the partner's Pages template and filled with real data."
mode: interactive
fields:
  - key: partner
    label: Partner
    type: select
    required: true
    default: TWC
    options: [TWC, RainBarrel, Reklaim, Raptive, Outcomes CA, Other]
  - key: campaign
    label: Campaign name
    type: text
    required: true
    placeholder: Go Raw CTV
  - key: kpis
    label: Channel(s) and KPI target
    type: textarea
    required: true
    placeholder: "CTV, CPM $27"
  - key: deals
    label: Deals
    type: textarea
    advanced: true
  - key: shareable
    label: Client-shareable (adds a password)
    type: toggle
    advanced: true
    default: false
promptTemplate: |-
  Create a new Pages dashboard by following protocols/page-creation.md, route A (from a template).
  Partner: {partner}
  Campaign: {campaign}
  Channels and KPI targets: {kpis}
  Deals: {deals}
  Client-shareable: {shareable}
`

func writePromptFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadPromptsParsesFormPrompt(t *testing.T) {
	dir := t.TempDir()
	writePromptFile(t, dir, "new-campaign.yaml", campaignFormPrompt)

	got, problems := ReadPrompts(dir)
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if len(got) != 1 {
		t.Fatalf("prompts = %+v", got)
	}
	p := got[0]
	if p.Name != "New campaign page from a template" {
		t.Errorf("Name = %q", p.Name)
	}
	// Content stays the raw file, form definition included: exports and
	// backups carry the prompt exactly as Git tracks it.
	if p.Content != campaignFormPrompt {
		t.Error("Content is not the raw file")
	}
	wantTemplate := "Create a new Pages dashboard by following protocols/page-creation.md, route A (from a template).\n" +
		"Partner: {partner}\nCampaign: {campaign}\nChannels and KPI targets: {kpis}\nDeals: {deals}\nClient-shareable: {shareable}"
	if p.PromptTemplate != wantTemplate {
		t.Errorf("PromptTemplate = %q, want %q", p.PromptTemplate, wantTemplate)
	}
	wantFields := []PromptField{
		{Key: "partner", Label: "Partner", Type: "select", Required: true, Default: "TWC",
			Options: []string{"TWC", "RainBarrel", "Reklaim", "Raptive", "Outcomes CA", "Other"}},
		{Key: "campaign", Label: "Campaign name", Type: "text", Required: true, Placeholder: "Go Raw CTV"},
		{Key: "kpis", Label: "Channel(s) and KPI target", Type: "textarea", Required: true, Placeholder: "CTV, CPM $27"},
		{Key: "deals", Label: "Deals", Type: "textarea", Advanced: true},
		{Key: "shareable", Label: "Client-shareable (adds a password)", Type: "toggle", Advanced: true, Default: false},
	}
	if !reflect.DeepEqual(p.Fields, wantFields) {
		t.Errorf("Fields =\n%#v\nwant\n%#v", p.Fields, wantFields)
	}
}

// TestPromptFormJSONShape pins the wire shape the web renders: the field
// properties use the empty-state card names, a false toggle default is still
// sent (it is a value, not an absence), and a plain prompt carries neither key.
func TestPromptFormJSONShape(t *testing.T) {
	dir := t.TempDir()
	writePromptFile(t, dir, "new-campaign.yaml", campaignFormPrompt)
	writePromptFile(t, dir, "plain.yaml", "name: Plain\ngoal: Just text.\n")
	got, problems := ReadPrompts(dir)
	if len(problems) != 0 || len(got) != 2 {
		t.Fatalf("got %+v problems %v", got, problems)
	}

	raw, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	var form map[string]any
	if err := json.Unmarshal(raw, &form); err != nil {
		t.Fatal(err)
	}
	if _, ok := form["prompt_template"].(string); !ok {
		t.Errorf("form prompt JSON has no prompt_template: %s", raw)
	}
	fields, _ := form["fields"].([]any)
	if len(fields) != 5 {
		t.Fatalf("form prompt JSON fields = %v", form["fields"])
	}
	toggle, _ := fields[4].(map[string]any)
	if def, present := toggle["default"]; !present || def != false {
		t.Errorf("toggle default = %v (present %v), want false", def, present)
	}
	if toggle["advanced"] != true || toggle["type"] != "toggle" {
		t.Errorf("toggle field JSON = %v", toggle)
	}

	raw, err = json.Marshal(got[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"fields"`) || strings.Contains(string(raw), `"prompt_template"`) {
		t.Errorf("plain prompt JSON carries form keys: %s", raw)
	}
}

func TestPromptFormNormalizesDefaults(t *testing.T) {
	body := `name: Defaults
fields:
  - {key: days, label: Days, type: number, min: 1, default: 7}
  - {key: lookback, label: Lookback, type: select, options: [7, 14, 30], default: 14}
  - {key: flight, label: Flight, type: daterange, default: {from: 2026-01-01}}
  - {key: note, label: Note, type: text, default: 2026}
promptTemplate: "{days} {lookback} {flight} {note}"
`
	fields, template, issues := parsePromptForm([]byte(body))
	if len(issues) != 0 {
		t.Fatalf("issues = %v", issues)
	}
	if template != "{days} {lookback} {flight} {note}" {
		t.Errorf("template = %q", template)
	}
	if fields[0].Default != float64(7) || fields[0].Min == nil || *fields[0].Min != 1 {
		t.Errorf("number field = %#v", fields[0])
	}
	if !reflect.DeepEqual(fields[1].Options, []string{"7", "14", "30"}) || fields[1].Default != "14" {
		t.Errorf("numeric select = %#v", fields[1])
	}
	if !reflect.DeepEqual(fields[2].Default, map[string]string{"from": "2026-01-01", "to": ""}) {
		t.Errorf("daterange default = %#v", fields[2].Default)
	}
	if fields[3].Default != "2026" {
		t.Errorf("text default = %#v", fields[3].Default)
	}
}

// TestReadPromptsInvalidFormDegradesToPlainPrompt covers each validation rule:
// the file is still served (with its raw content and no form), and the problem
// names the file and what is wrong with it.
func TestReadPromptsInvalidFormDegradesToPlainPrompt(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"unknown type", `fields: [{key: a, label: A, type: radio}]
promptTemplate: "{a}"`, `type "radio" is not one of`},
		{"missing type", `fields: [{key: a, label: A}]
promptTemplate: "{a}"`, "type is required"},
		{"duplicate key", `fields: [{key: a, label: A, type: text}, {key: a, label: B, type: text}]
promptTemplate: "{a}"`, `key "a" is used by more than one field`},
		{"missing key", `fields: [{label: A, type: text}]
promptTemplate: "x"`, "key is required"},
		{"key a token cannot name", `fields: [{key: deal-id, label: A, type: text}]
promptTemplate: "x"`, "must be letters, digits or underscores"},
		{"missing label", `fields: [{key: a, type: text}]
promptTemplate: "{a}"`, "label is required"},
		{"select without options", `fields: [{key: a, label: A, type: select}]
promptTemplate: "{a}"`, "needs at least one option"},
		{"select default outside options", `fields: [{key: a, label: A, type: select, options: [x, y], default: z}]
promptTemplate: "{a}"`, `default "z" is not one of the options`},
		{"duplicate option", `fields: [{key: a, label: A, type: select, options: [x, x]}]
promptTemplate: "{a}"`, `option "x" is listed twice`},
		{"options on a text field", `fields: [{key: a, label: A, type: text, options: [x]}]
promptTemplate: "{a}"`, "options only apply to a select field"},
		{"min on a text field", `fields: [{key: a, label: A, type: text, min: 1}]
promptTemplate: "{a}"`, "min only applies to a number field"},
		{"number default below min", `fields: [{key: a, label: A, type: number, min: 5, default: 2}]
promptTemplate: "{a}"`, "default 2 is below min 5"},
		{"required toggle", `fields: [{key: a, label: A, type: toggle, required: true}]
promptTemplate: "{a}"`, "cannot be required"},
		{"required advanced field", `fields: [{key: a, label: A, type: text, required: true, advanced: true}]
promptTemplate: "{a}"`, "a required field cannot be advanced"},
		{"toggle default not a bool", `fields: [{key: a, label: A, type: toggle, default: "yes"}]
promptTemplate: "{a}"`, "default must be true or false"},
		{"bad daterange default", `fields: [{key: a, label: A, type: daterange, default: {from: tomorrow}}]
promptTemplate: "{a}"`, `default from "tomorrow" is not a YYYY-MM-DD date`},
		{"typo in a property", `fields: [{key: a, label: A, type: text, require: true}]
promptTemplate: "{a}"`, `unknown property "require"`},
		{"required not a bool", `fields: [{key: a, label: A, type: text, required: "yes"}]
promptTemplate: "{a}"`, "required must be true or false"},
		{"undeclared token", `fields: [{key: a, label: A, type: text}]
promptTemplate: "{a} {b}"`, "promptTemplate uses {b}, which is not a declared field key"},
		{"unused field", `fields: [{key: a, label: A, type: text}, {key: b, label: B, type: text}]
promptTemplate: "{a}"`, `field "b" is never used in promptTemplate`},
		{"fields without a template", `fields: [{key: a, label: A, type: text}]`, "without a promptTemplate"},
		{"snake_case template key", `fields: [{key: a, label: A, type: text}]
prompt_template: "{a}"`, "the YAML key is promptTemplate"},
		{"template without fields", `promptTemplate: "Do it."`, "promptTemplate is declared without fields"},
		{"empty template", `fields: [{key: a, label: A, type: text}]
promptTemplate: "  "`, "promptTemplate is empty"},
		{"template not a string", `fields: [{key: a, label: A, type: text}]
promptTemplate: [a]`, "promptTemplate must be a string"},
		{"fields not a list", `fields: everything
promptTemplate: "x"`, "fields must be a list"},
		{"empty fields", `fields: []
promptTemplate: "x"`, "fields is empty"},
		{"field not a mapping", `fields: [a]
promptTemplate: "x"`, "fields[0] must be a mapping"},
		{"yaml that does not parse", "fields:\n  - key: a\n    label: [unterminated\npromptTemplate: \"{a}\"\n", "the YAML does not parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body := "name: Broken form\n" + tc.body + "\n"
			if strings.HasPrefix(tc.body, "fields:\n") {
				body = tc.body // the parse-error case must keep its own first line
			}
			writePromptFile(t, dir, "broken.yaml", body)
			got, problems := ReadPrompts(dir)
			if len(got) != 1 {
				t.Fatalf("an invalid form must not drop the entry: got %+v", got)
			}
			if got[0].Fields != nil || got[0].PromptTemplate != "" {
				t.Errorf("invalid form was served as a form: %+v", got[0])
			}
			if got[0].Content != body {
				t.Error("the plain fallback must serve the raw content")
			}
			if len(problems) != 1 {
				t.Fatalf("problems = %v, want exactly one", problems)
			}
			if !strings.HasPrefix(problems[0], "prompt broken.yaml: invalid form, served as a plain prompt: ") {
				t.Errorf("problem does not name the file and the fallback: %q", problems[0])
			}
			if !strings.Contains(problems[0], tc.want) {
				t.Errorf("problem = %q, want it to mention %q", problems[0], tc.want)
			}
		})
	}
}

func TestPromptFormReportsEveryIssueAtOnce(t *testing.T) {
	_, _, issues := parsePromptForm([]byte(`fields:
  - {key: a, type: radio}
  - {key: b, label: B, type: toggle, required: true}
promptTemplate: "{a} {c}"
`))
	joined := strings.Join(issues, "\n")
	for _, want := range []string{"fields[0] (a): label is required", `type "radio"`, "fields[1] (b): a toggle always has a value", "{c}", `field "b" is never used`} {
		if !strings.Contains(joined, want) {
			t.Errorf("issues missing %q:\n%s", want, joined)
		}
	}
}

func TestPromptFormOnlyForYAML(t *testing.T) {
	dir := t.TempDir()
	// Markdown and text prompts never have a form, even when their prose
	// happens to look like one.
	writePromptFile(t, dir, "notes.md", "# Notes\n\nfields:\n  - key: a\npromptTemplate: \"{a}\"\n")
	writePromptFile(t, dir, "notes.txt", "fields: [{key: a, label: A, type: text}]\npromptTemplate: \"{a}\"\n")
	// A YAML prompt that does not parse and shows no sign of a form is served
	// as plain text, exactly as before forms existed — no new problem.
	writePromptFile(t, dir, "loose.yaml", "name: Loose\nsteps: [unterminated\n")
	got, problems := ReadPrompts(dir)
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if len(got) != 3 {
		t.Fatalf("prompts = %+v", got)
	}
	for _, p := range got {
		if p.Fields != nil || p.PromptTemplate != "" {
			t.Errorf("%s got a form: %+v", p.Path, p)
		}
	}
}

// TestDefaultBundlePromptsValidate keeps the shipped generic bundle's prompt
// library clean and makes CI exercise a real form prompt end to end.
func TestDefaultBundlePromptsValidate(t *testing.T) {
	got, problems := ReadPrompts(filepath.Join(repoRoot(t), "config", "default", "prompts"))
	if len(problems) != 0 {
		t.Fatalf("config/default prompts have problems: %v", problems)
	}
	var form *Prompt
	for i := range got {
		if got[i].Path == "prompts/meeting-follow-up.yaml" {
			form = &got[i]
		}
	}
	if form == nil {
		t.Fatalf("config/default ships no meeting-follow-up form prompt: %+v", got)
	}
	types := map[string]bool{}
	for _, f := range form.Fields {
		types[f.Type] = true
	}
	for _, want := range promptFieldTypes {
		if !types[want] {
			t.Errorf("the default bundle's example form does not exercise field type %q", want)
		}
	}
}
