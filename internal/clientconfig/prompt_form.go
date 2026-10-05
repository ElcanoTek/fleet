package clientconfig

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Form prompts.
//
// A Git-backed YAML prompt may declare a FORM: a top-level `fields:` list and a
// `promptTemplate:` string whose `{key}` tokens name those fields. Picking such
// an entry in the shared Prompt Library picker opens the form instead of
// pasting the file; submitting it renders the template with the user's values
// and inserts the result into the chat or task draft, where it can still be
// reviewed and edited before anything runs. The motivating failure is the one
// every template-by-convention hits: a prompt full of `[PLACEHOLDERS]` that
// people have to find and overwrite by hand inside a large text box, and
// usually miss one of.
//
// The field schema is deliberately NOT new. It is the one the chat empty-state
// cards already render (`empty_state.cards[].fields`, the web's PillField):
// key, label, type, required, placeholder, hint, default, options, advanced,
// min, over the types text / textarea / select / number / daterange / toggle.
// One schema means one set of field components in the web, and a bundle author
// who has written a card has already written a form prompt.
//
// What is different from the cards is WHERE the schema is checked. Cards are
// passed to the browser as opaque JSON and a malformed one simply renders
// oddly. A library form is validated here, at catalog read, because the cost of
// a bad one is higher and quieter: a required toggle can never be satisfied,
// a template token with no field is pasted through literally, and a field the
// template never mentions collects an answer that silently goes nowhere. Each
// of those is a contract the author got wrong, so each is reported through the
// catalog's problems list (logged on every read; see ReadPrompts) and the entry
// is served as an ordinary plain prompt with its raw content. Degrading to the
// pre-form behaviour is the safe direction: the user still gets the prompt, and
// the author gets told why the form did not appear.
//
// The checks, all of which must pass for the form to be served:
//
//   - `fields` is a non-empty list of mappings, and `promptTemplate` is a
//     non-empty string. Either one without the other is reported: a template
//     with no fields is almost always a misspelled `fields:`, and fields with
//     no template have nothing to render into.
//   - Each field has only the known properties (a slip such as `require` for
//     `required` is reported, not ignored), a key of letters, digits and
//     underscores (the only keys a `{token}` can name), unique across the
//     form, a non-empty label, and a known type.
//   - Type-specific properties are where they mean something: `options` only on
//     a select, which must have at least one option, unique and non-empty;
//     `min` only on a number; `required` never on a toggle, which always has a
//     value (true or false) and so would block the form forever in the web,
//     whose readiness check treats a boolean as blank; and `required` never
//     together with `advanced`, which starts collapsed, so a required advanced
//     field would disable the form with nothing visible to fill in.
//   - A `default`, when given, has the type's shape: a string (or number) for
//     text, textarea and select — and for a select one of its options — a
//     number not below `min` for a number, true/false for a toggle, and a
//     `{from, to}` mapping of YYYY-MM-DD dates for a daterange.
//   - Every `{token}` in the template names a declared field, and every field
//     is used by at least one token.
//
// Only the top-level `fields` and `promptTemplate` keys are reserved. Every
// other key in the file (`name`, `description`, `mode`, anything the bundle
// uses) is untouched, and Content remains the raw file.

// promptTemplateToken matches one `{key}` token. It is the same pattern the web
// renderer interpolates with (pillToPrompt's /\{(\w+)\}/g — `\w` is ASCII
// letters, digits and underscore in both RE2 and a non-unicode JS regex), so a
// template that validates here renders the same tokens there.
var promptTemplateToken = regexp.MustCompile(`\{(\w+)\}`)

// promptFieldKeyPattern is the shape a field key must have to be reachable from
// a template token at all; a key with a hyphen or a space could never be named.
var promptFieldKeyPattern = regexp.MustCompile(`^\w+$`)

// promptFormKeyLine spots a top-level `fields:` in a YAML file that does not
// parse, so a form hidden by a syntax error elsewhere in the file is reported
// instead of silently disappearing.
var promptFormKeyLine = regexp.MustCompile(`(?m)^(fields|promptTemplate)\s*:`)

// promptFieldTypes is the closed set of field types a form prompt may use —
// the types the web's shared field renderer draws.
var promptFieldTypes = []string{"text", "textarea", "select", "number", "daterange", "toggle"}

var promptFieldProperties = map[string]bool{
	"key": true, "label": true, "type": true, "required": true,
	"placeholder": true, "hint": true, "default": true, "options": true,
	"advanced": true, "min": true,
}

// PromptField is one input of a form prompt. The JSON shape is the web's
// PillField, so the browser renders it with the same components as an
// empty-state card field. Default carries the type's value shape: a string for
// text/textarea/select, a number for number, a bool for toggle, and a
// {"from","to"} object for daterange.
type PromptField struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Type        string   `json:"type"`
	Required    bool     `json:"required,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Hint        string   `json:"hint,omitempty"`
	Default     any      `json:"default,omitempty"`
	Options     []string `json:"options,omitempty"`
	Advanced    bool     `json:"advanced,omitempty"`
	Min         *float64 `json:"min,omitempty"`
}

// parsePromptForm reads the optional form definition out of a YAML prompt.
// It returns (nil, "", nil) for a file that declares no form — including one
// whose YAML does not parse and shows no sign of a form, which is served as
// plain text exactly as before forms existed. Any issue means the form is not
// served; the caller reports the issues and keeps the entry as a plain prompt.
func parsePromptForm(raw []byte) (fields []PromptField, template string, issues []string) {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		if promptFormKeyLine.Match(raw) {
			msg, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
			return nil, "", []string{"the YAML does not parse, so its form cannot be read: " + msg}
		}
		return nil, "", nil
	}
	rawFields, hasFields := doc["fields"]
	rawTemplate, hasTemplate := doc["promptTemplate"]
	if !hasFields && !hasTemplate {
		return nil, "", nil
	}

	if !hasFields {
		issues = append(issues, "promptTemplate is declared without fields; a form needs at least one field")
	} else {
		fields, issues = parsePromptFields(rawFields)
	}

	switch t := rawTemplate.(type) {
	case nil:
		// Absent or `promptTemplate:` with no value. Without fields this is
		// already reported above; with fields it is the missing half.
		if hasFields {
			hint := ""
			if _, snake := doc["prompt_template"]; snake {
				hint = " (found prompt_template; the YAML key is promptTemplate)"
			}
			issues = append(issues, "fields are declared without a promptTemplate to render them into"+hint)
		}
	case string:
		if strings.TrimSpace(t) == "" {
			issues = append(issues, "promptTemplate is empty")
		}
		template = t
	default:
		issues = append(issues, "promptTemplate must be a string")
	}

	if template != "" && len(fields) > 0 {
		issues = append(issues, promptTemplateTokenIssues(template, fields)...)
	}
	if len(issues) > 0 {
		return nil, "", issues
	}
	return fields, template, nil
}

func parsePromptFields(raw any) ([]PromptField, []string) {
	list, ok := raw.([]any)
	if !ok {
		return nil, []string{"fields must be a list of field definitions"}
	}
	if len(list) == 0 {
		return nil, []string{"fields is empty; a form needs at least one field"}
	}
	var (
		fields []PromptField
		issues []string
		seen   = map[string]bool{}
	)
	for i, item := range list {
		where := fmt.Sprintf("fields[%d]", i)
		m, ok := item.(map[string]any)
		if !ok {
			issues = append(issues, where+" must be a mapping with key, label and type")
			continue
		}
		if key, ok := m["key"].(string); ok && strings.TrimSpace(key) != "" {
			where = fmt.Sprintf("%s (%s)", where, key)
		}
		field, fieldIssues := parsePromptField(m)
		for _, issue := range fieldIssues {
			issues = append(issues, where+": "+issue)
		}
		if field.Key != "" {
			if seen[field.Key] {
				issues = append(issues, fmt.Sprintf("%s: key %q is used by more than one field", where, field.Key))
			}
			seen[field.Key] = true
		}
		fields = append(fields, field)
	}
	return fields, issues
}

// promptFieldReader reads typed properties out of one field mapping and
// collects every issue rather than stopping at the first, so an author fixes a
// form in one pass instead of one problem per reload.
type promptFieldReader struct {
	m      map[string]any
	issues []string
}

func (r *promptFieldReader) reportf(format string, args ...any) {
	r.issues = append(r.issues, fmt.Sprintf(format, args...))
}

// value returns a property's raw value; a property written with no value
// (`hint:`) counts as absent, as it would for any other YAML consumer.
func (r *promptFieldReader) value(prop string) (any, bool) {
	v, present := r.m[prop]
	return v, present && v != nil
}

func (r *promptFieldReader) str(prop string) string {
	v, ok := r.value(prop)
	if !ok {
		return ""
	}
	s, isString := v.(string)
	if !isString {
		r.reportf("%s must be a string", prop)
	}
	return s
}

func (r *promptFieldReader) flag(prop string) bool {
	v, ok := r.value(prop)
	if !ok {
		return false
	}
	b, isBool := v.(bool)
	if !isBool {
		r.reportf("%s must be true or false", prop)
	}
	return b
}

func parsePromptField(m map[string]any) (PromptField, []string) {
	r := &promptFieldReader{m: m}
	unknown := make([]string, 0)
	for prop := range m {
		if !promptFieldProperties[prop] {
			unknown = append(unknown, prop)
		}
	}
	sort.Strings(unknown)
	for _, prop := range unknown {
		r.reportf("unknown property %q", prop)
	}

	f := PromptField{
		Key:         strings.TrimSpace(r.str("key")),
		Label:       strings.TrimSpace(r.str("label")),
		Type:        strings.TrimSpace(r.str("type")),
		Required:    r.flag("required"),
		Advanced:    r.flag("advanced"),
		Placeholder: r.str("placeholder"),
		Hint:        r.str("hint"),
	}
	switch {
	case f.Key == "":
		r.reportf("key is required")
	case !promptFieldKeyPattern.MatchString(f.Key):
		r.reportf("key %q must be letters, digits or underscores so a {token} can name it", f.Key)
		f.Key = ""
	}
	if f.Label == "" {
		r.reportf("label is required")
	}
	knownType := slices.Contains(promptFieldTypes, f.Type)
	switch {
	case f.Type == "":
		r.reportf("type is required (one of %s)", strings.Join(promptFieldTypes, ", "))
	case !knownType:
		r.reportf("type %q is not one of %s", f.Type, strings.Join(promptFieldTypes, ", "))
	}

	f.Options = r.options(f.Type)
	f.Min = r.minimum(f.Type)

	if f.Type == "toggle" && f.Required {
		r.reportf("a toggle always has a value, so it cannot be required")
	}
	if f.Required && f.Advanced {
		// advanced fields start collapsed under "More options"; a required one
		// would disable Use prompt with nothing visible to fill in.
		r.reportf("a required field cannot be advanced (it would be hidden under More options while blocking the form)")
	}

	if rawDefault, ok := r.value("default"); ok && knownType {
		def, issue := promptFieldDefault(f, rawDefault)
		if issue != "" {
			r.reportf("%s", issue)
		} else {
			f.Default = def
		}
	}
	return f, r.issues
}

// options reads a select's choices: strings or numbers, each non-blank and
// listed once. They are an error on any other type, and required on a select.
func (r *promptFieldReader) options(fieldType string) []string {
	raw, ok := r.value("options")
	if !ok {
		if fieldType == "select" {
			r.reportf("a select field needs at least one option")
		}
		return nil
	}
	if fieldType != "select" {
		r.reportf("options only apply to a select field")
	}
	list, isList := raw.([]any)
	if !isList {
		r.reportf("options must be a list")
	}
	var out []string
	seen := map[string]bool{}
	for _, o := range list {
		s, ok := yamlScalarText(o)
		switch {
		case !ok:
			r.reportf("options must be strings or numbers")
		case strings.TrimSpace(s) == "":
			r.reportf("options must not be blank")
		case seen[s]:
			r.reportf("option %q is listed twice", s)
		default:
			seen[s] = true
			out = append(out, s)
		}
	}
	if fieldType == "select" && isList && len(out) == 0 && len(list) == 0 {
		r.reportf("a select field needs at least one option")
	}
	return out
}

func (r *promptFieldReader) minimum(fieldType string) *float64 {
	raw, ok := r.value("min")
	if !ok {
		return nil
	}
	if fieldType != "number" {
		r.reportf("min only applies to a number field")
	}
	n, isNumber := yamlNumber(raw)
	if !isNumber {
		r.reportf("min must be a number")
		return nil
	}
	return &n
}

// promptFieldDefault checks a default against its field's type and returns it
// in the shape the web's form state expects (see PromptField).
func promptFieldDefault(f PromptField, v any) (any, string) {
	switch f.Type {
	case "text", "textarea":
		s, ok := yamlScalarText(v)
		if !ok {
			return nil, "default must be a string"
		}
		return s, ""
	case "select":
		s, ok := yamlScalarText(v)
		if !ok {
			return nil, "default must be one of the options"
		}
		for _, o := range f.Options {
			if o == s {
				return s, ""
			}
		}
		return nil, fmt.Sprintf("default %q is not one of the options", s)
	case "number":
		n, ok := yamlNumber(v)
		if !ok {
			return nil, "default must be a number"
		}
		if f.Min != nil && n < *f.Min {
			return nil, fmt.Sprintf("default %s is below min %s", formatNumber(n), formatNumber(*f.Min))
		}
		return n, ""
	case "toggle":
		b, ok := v.(bool)
		if !ok {
			return nil, "default must be true or false"
		}
		return b, ""
	case "daterange":
		m, ok := v.(map[string]any)
		if !ok {
			return nil, "default must be a mapping with from and to dates"
		}
		out := map[string]string{"from": "", "to": ""}
		for k, raw := range m {
			if k != "from" && k != "to" {
				return nil, fmt.Sprintf("default has unknown property %q (want from and to)", k)
			}
			s, ok := raw.(string)
			if raw == nil {
				s, ok = "", true
			}
			if !ok {
				return nil, "default " + k + " must be a YYYY-MM-DD date"
			}
			if s != "" {
				if _, err := time.Parse(time.DateOnly, s); err != nil {
					return nil, fmt.Sprintf("default %s %q is not a YYYY-MM-DD date", k, s)
				}
			}
			out[k] = s
		}
		return out, ""
	}
	return nil, ""
}

// promptTemplateTokenIssues enforces the two-way contract between a template
// and its fields: no token without a field (it would be pasted through
// literally) and no field without a token (its answer would go nowhere).
func promptTemplateTokenIssues(template string, fields []PromptField) []string {
	declared := map[string]bool{}
	for _, f := range fields {
		if f.Key != "" {
			declared[f.Key] = true
		}
	}
	var issues []string
	used := map[string]bool{}
	for _, m := range promptTemplateToken.FindAllStringSubmatch(template, -1) {
		key := m[1]
		if !declared[key] && !used[key] {
			issues = append(issues, fmt.Sprintf("promptTemplate uses {%s}, which is not a declared field key", key))
		}
		used[key] = true
	}
	for _, f := range fields {
		if f.Key != "" && !used[f.Key] {
			issues = append(issues, fmt.Sprintf("field %q is never used in promptTemplate (add {%s} where its answer belongs)", f.Key, f.Key))
		}
	}
	return issues
}

// yamlScalarText accepts a YAML string or number as text. Numbers are allowed
// because an author writing `options: [7, 14, 30]` means the labels "7", "14"
// and "30"; booleans are not, because an unquoted yes/no/true in an option list
// is far more often a quoting mistake than an intended option.
func yamlScalarText(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return "", false
	}
	if n, ok := yamlNumber(v); ok {
		return formatNumber(n), true
	}
	return "", false
}

func yamlNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func formatNumber(n float64) string {
	return strconv.FormatFloat(n, 'f', -1, 64)
}
