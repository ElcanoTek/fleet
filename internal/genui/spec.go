// Package genui validates generative-UI card specs: the declarative component
// trees the interactive chat agent hands the show_ui tool so the web client
// can render a purpose-built card (a form, a comparison, a checklist, a small
// calculator, a multi-item editor) inline in the conversation.
//
// Why a declarative spec and not generated code: the model's output is
// untrusted, and a card is rendered inside the user's authenticated session.
// A fixed component catalog rendered by fleet's own React code means the
// model can choose WHAT to show and HOW it is laid out, but every pixel, every
// event handler and every network request is fleet's. There is no HTML, no
// script, no style, and no URL fetched on render. The one "logic" surface —
// templates, visibility conditions and computed values — is the side-effect
// free expression language in expr.go.
//
// Why validate on the server when the browser renders: the browser's only
// recourse with a bad spec is to draw an error box the model never hears
// about. Validating at tool-call time turns every mistake (an unknown
// component, a select with no options, an expression naming a field that does
// not exist) into a tool error the model reads and fixes in the same turn.
// The web renderer is still defensive — it renders only known components and
// treats every string as text — so this is a feedback loop, not the security
// boundary.
//
// See docs/GENERATIVE-UI.md and ADR-0080.
package genui

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Protocol limits. They bound what one card can make the browser do and what
// one tool call can cost in context; they are not operator knobs, because a
// card valid on one fleet must render on another.
const (
	MaxSpecBytes       = 256 << 10
	MaxNodes           = 800
	MaxDepth           = 12
	MaxOptions         = 2000
	MaxTableRows       = 500
	MaxTableColumns    = 20
	MaxChartSeries     = 8
	MaxChartPoints     = 200
	MaxActions         = 6
	MaxRepeaterItems   = 200
	MaxListItems       = 20000
	MaxStringLen       = 20000
	MaxColumns         = 4
	MaxTabs            = 12
	MaxFieldErrors     = 200
	maxIssuesReported  = 25
	maxNamesInHintText = 30
)

// Issue is one validation problem, addressed by a JSON-ish path into the
// tool call's arguments so the model can find it.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (i Issue) String() string {
	if i.Path == "" {
		return i.Message
	}
	return i.Path + ": " + i.Message
}

// Card is the show_ui tool's argument shape. Components and actions stay
// loosely typed (any) because the tool schema cannot express a recursive
// discriminated union portably across providers; Validate is the real schema.
type Card struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Components  []any  `json:"components"`
	Actions     []any  `json:"actions,omitempty"`
	Replaces    string `json:"replaces,omitempty"`
	FieldErrors []any  `json:"field_errors,omitempty"`
}

type kind int

const (
	kString   kind = iota // plain text
	kTemplate             // text with {{ expr }} holes
	kExpr                 // a bare expression
	kBool
	kNumber
	kInt
	kEnum
	kChildren   // []component
	kOptions    // []string | []{value,label?,description?}
	kStringList // []string
	kNumberList // []number
	kValue      // an input's default value — checked per component in checkValue
	kURL        // https only
	kDate       // YYYY-MM-DD
	kObjects    // []object validated against a sub-spec (items)
	kRows       // table rows: []object of primitives
	kFieldPath  // "id" or "repeater[i].field", resolved after the walk
	kNonBlank   // a string the user must be able to read: never empty or whitespace
)

type prop struct {
	kind     kind
	required bool
	enum     []string
	items    map[string]prop // for kObjects
}

func p(k kind) prop            { return prop{kind: k} }
func req(k kind) prop          { return prop{kind: k, required: true} }
func enum(vals ...string) prop { return prop{kind: kEnum, enum: vals} }
func reqObjects(items map[string]prop) prop {
	return prop{kind: kObjects, items: items, required: true}
}

var tones = []string{"neutral", "info", "success", "warning", "danger"}

type compSpec struct {
	props map[string]prop
	input bool // has an id + a submitted value
	// scope marks a component whose children see per-item names (repeater).
	scope bool
}

// inputCommon is merged into every input component.
var inputCommon = map[string]prop{
	"id":       req(kString),
	"label":    p(kString),
	"help":     p(kTemplate),
	"required": p(kBool),
	"disabled": p(kBool),
	"value":    p(kValue),
}

// components is the catalog. Keep in step with the renderer registry in
// web/src/app/chat/ui/genui/GenerativeCard.tsx and the catalog text in
// internal/tools/show_ui.go; TestCatalogMatchesFixture pins the names.
var components = map[string]compSpec{
	// ── layout ──
	"section": {props: map[string]prop{
		"title": p(kString), "description": p(kTemplate), "children": req(kChildren),
		"collapsible": p(kBool), "collapsed": p(kBool),
	}},
	"columns": {props: map[string]prop{"children": req(kChildren)}},
	"tabs": {props: map[string]prop{
		"variant": enum("tabs", "steps"),
		"tabs": reqObjects(map[string]prop{
			"label": req(kString), "children": req(kChildren),
		}),
	}},
	"divider": {props: map[string]prop{}},

	// ── display ──
	"heading": {props: map[string]prop{"text": req(kTemplate)}},
	"text": {props: map[string]prop{
		"text": req(kTemplate), "markdown": p(kBool), "tone": enum("default", "muted"),
	}},
	"callout": {props: map[string]prop{
		"tone": enum(tones...), "title": p(kString), "text": req(kTemplate),
	}},
	"badges": {props: map[string]prop{"items": reqObjects(map[string]prop{
		"text": req(kTemplate), "tone": enum(tones...),
	})}},
	"stat": {props: map[string]prop{
		"label": req(kString), "value": req(kTemplate), "caption": p(kTemplate), "tone": enum(tones...),
	}},
	"facts": {props: map[string]prop{"items": reqObjects(map[string]prop{
		"label": req(kString), "value": req(kTemplate),
	})}},
	"table": {props: map[string]prop{
		"id": p(kString), "label": p(kString), "help": p(kTemplate),
		"columns": reqObjects(map[string]prop{
			"key": req(kString), "label": p(kString), "align": enum("left", "right", "center"),
		}),
		"rows":     {kind: kRows, required: true},
		"select":   enum("none", "single", "multi"),
		"row_key":  p(kString),
		"value":    p(kValue),
		"required": p(kBool),
	}},
	"status_list": {props: map[string]prop{"items": reqObjects(map[string]prop{
		"status": {kind: kEnum, required: true, enum: []string{"pass", "fail", "warn", "info", "pending"}},
		"label":  req(kTemplate), "detail": p(kTemplate), "field": p(kFieldPath),
	})}},
	"progress": {props: map[string]prop{
		"label": p(kString), "value": req(kExpr), "max": p(kNumber),
	}},
	"chart": {props: map[string]prop{
		"kind":  {kind: kEnum, required: true, enum: []string{"bar", "line"}},
		"title": p(kString), "labels": req(kStringList), "unit": p(kString),
		"series": reqObjects(map[string]prop{
			"name": req(kString), "values": req(kNumberList),
		}),
	}},
	"code": {props: map[string]prop{"text": req(kString), "language": p(kString)}},
	"link": {props: map[string]prop{"text": req(kString), "url": req(kURL)}},
	"diff": {props: map[string]prop{
		"title": p(kString),
		"rows": reqObjects(map[string]prop{
			"label": req(kString), "before": p(kString), "after": p(kString),
		}),
	}},

	// ── inputs ──
	"text_input": {input: true, props: withInput(map[string]prop{
		"placeholder": p(kString), "multiline": p(kBool),
		"min_length": p(kInt), "max_length": p(kInt), "format": enum("text", "email", "url"),
	})},
	"number": {input: true, props: withInput(map[string]prop{
		"min": p(kNumber), "max": p(kNumber), "step": p(kNumber),
		"prefix": p(kString), "suffix": p(kString), "placeholder": p(kString),
	})},
	"slider": {input: true, props: withInput(map[string]prop{
		"min": req(kNumber), "max": req(kNumber), "step": p(kNumber),
		"prefix": p(kString), "suffix": p(kString),
	})},
	"select": {input: true, props: withInput(map[string]prop{
		"options": req(kOptions), "placeholder": p(kString),
	})},
	"choice": {input: true, props: withInput(map[string]prop{
		"options": req(kOptions), "variant": enum("segmented", "radio"),
	})},
	"multi_select": {input: true, props: withInput(map[string]prop{
		"options": p(kOptions), "allow_custom": p(kBool), "max_items": p(kInt), "placeholder": p(kString),
	})},
	"toggle": {input: true, props: withInput(map[string]prop{})},
	"date":   {input: true, props: withInput(map[string]prop{"min": p(kDate), "max": p(kDate)})},
	"list_input": {input: true, props: withInput(map[string]prop{
		"placeholder": p(kString), "max_items": p(kInt), "dedupe": p(kBool),
	})},
	"include_exclude": {input: true, props: withInput(map[string]prop{
		"options": p(kOptions), "allow_custom": p(kBool),
		"include_label": p(kString), "exclude_label": p(kString),
	})},
	"repeater": {input: true, scope: true, props: withInput(map[string]prop{
		"fields": req(kChildren), "item_label": p(kTemplate),
		"min_items": p(kInt), "max_items": p(kInt), "add_label": p(kString),
	})},
}

func withInput(m map[string]prop) map[string]prop {
	for k, v := range inputCommon {
		m[k] = v
	}
	return m
}

// commonProps are allowed on every component.
var commonProps = map[string]prop{
	"type":       req(kString),
	"id":         p(kString),
	"visible_if": p(kExpr),
}

var actionProps = map[string]prop{
	"id":          req(kString),
	"label":       req(kString),
	"kind":        enum("submit", "message"),
	"style":       enum("primary", "secondary", "danger"),
	"message":     p(kString),
	"validate":    p(kBool),
	"confirm":     p(kNonBlank),
	"visible_if":  p(kExpr),
	"disabled_if": p(kExpr),
}

var fieldErrorProps = map[string]prop{"field": req(kFieldPath), "message": req(kNonBlank)}

// ComponentTypes lists the catalog, sorted.
func ComponentTypes() []string {
	out := make([]string, 0, len(components))
	for k := range components {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

var idRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// reservedNames cannot be field ids: they are expression keywords, the
// repeater's per-item index, or function names (an id shadowing a function
// would make "len(len)" legal and baffling).
var reservedNames = func() map[string]bool {
	// __proto__ / constructor / prototype would hit JavaScript's legacy
	// prototype setters when the browser assigns values[id], silently
	// dropping the input from drafts and submissions.
	m := map[string]bool{"true": true, "false": true, "null": true, "index": true,
		"__proto__": true, "constructor": true, "prototype": true}
	for f := range exprFuncs {
		m[f] = true
	}
	return m
}()

// field is a declared input, recorded in the first pass.
type field struct {
	id       string
	typ      string
	path     string
	repeater string // enclosing repeater id, "" at card scope
	// items is a repeater's item count when the card opens (its value, else
	// max(1, min_items)) — the indexes a field_error or Fix link can name.
	items int
	// fixed: the input is `disabled: true`, so the user can never change it.
	fixed bool
}

type validator struct {
	issues []Issue
	nodes  int
	fields map[string]*field
	// pending expression references and field paths, resolved after all
	// ids are known.
	exprs      []exprRef
	fieldPaths []pathRef
}

type pathRef struct{ path, target string }

type exprRef struct {
	path     string
	src      string
	repeater string // scope the expression is written in
}

func (v *validator) addf(path, format string, args ...any) {
	v.issues = append(v.issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
}

// Validate checks a raw show_ui argument payload and returns every issue
// found (capped). An empty slice means the card is renderable.
func Validate(raw []byte) (Card, []Issue) {
	var c Card
	if len(raw) > MaxSpecBytes {
		return c, []Issue{{Message: fmt.Sprintf("card is %d bytes; the limit is %d. Split it, or put long data in a file and summarize it", len(raw), MaxSpecBytes)}}
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return c, []Issue{{Message: "arguments are not a JSON object: " + err.Error()}}
	}
	v := &validator{fields: map[string]*field{}}
	allowedTop := map[string]bool{"title": true, "description": true, "components": true, "actions": true, "replaces": true, "field_errors": true}
	for k := range top {
		if !allowedTop[k] {
			v.addf(k, "unknown top-level key (allowed: title, description, components, actions, replaces, field_errors)")
		}
	}
	title, _ := top["title"].(string)
	if strings.TrimSpace(title) == "" {
		v.addf("title", "required: a short card title")
	}
	v.checkString("title", title)
	if d, ok := top["description"]; ok {
		v.checkProp("description", p(kTemplate), d, "")
	}
	comps, ok := top["components"].([]any)
	if !ok || len(comps) == 0 {
		v.addf("components", "required: a non-empty array of components")
	}
	for i, comp := range comps {
		v.component(fmt.Sprintf("components[%d]", i), comp, 1, "")
	}

	v.actions(top["actions"], len(v.fields) > 0)
	if r, ok := top["replaces"]; ok {
		if s, isStr := r.(string); !isStr || strings.TrimSpace(s) == "" {
			v.addf("replaces", "must be the card_id string of an earlier card")
		}
	}
	v.fieldErrors(top["field_errors"])
	v.resolveExprs()

	if len(v.issues) == 0 {
		// Decode into the typed shape only once the payload is known-good.
		_ = json.Unmarshal(raw, &c)
	}
	if len(v.issues) > maxIssuesReported {
		extra := len(v.issues) - maxIssuesReported
		v.issues = append(v.issues[:maxIssuesReported], Issue{Message: fmt.Sprintf("…and %d more issue(s)", extra)})
	}
	return c, v.issues
}

func (v *validator) component(path string, raw any, depth int, repeater string) {
	v.nodes++
	if v.nodes == MaxNodes+1 {
		v.addf(path, "card has more than %d components; split it into smaller cards", MaxNodes)
	}
	if v.nodes > MaxNodes {
		return
	}
	if depth > MaxDepth {
		v.addf(path, "nested more than %d levels deep", MaxDepth)
		return
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		v.addf(path, "a component must be an object with a \"type\"")
		return
	}
	typ, _ := obj["type"].(string)
	spec, known := components[typ]
	if !known {
		v.addf(path, "unknown component type %q (allowed: %s)", typ, strings.Join(ComponentTypes(), ", "))
		return
	}
	path = path + "(" + typ + ")"
	v.componentProps(path, typ, spec, obj, depth, repeater)

	id, hasID := obj["id"].(string)
	if _, present := obj["id"]; present && !hasID {
		v.addf(path+".id", "must be a string")
	}
	if spec.input || (typ == "table" && hasID) {
		if !hasID {
			if spec.input {
				v.addf(path+".id", "required: every input needs an id (it becomes the key in the submitted values)")
			}
		} else {
			v.declare(path, id, typ, repeater)
			if f, ok := v.fields[id]; ok {
				if typ == "repeater" {
					f.items = initialItems(obj)
				}
				f.fixed, _ = obj["disabled"].(bool)
			}
		}
	} else if hasID && !idRe.MatchString(id) {
		v.addf(path+".id", "ids must match %s", idRe.String())
	}

	v.componentRules(path, typ, obj)
	if val, ok := obj["value"]; ok && spec.props["value"].kind == kValue {
		v.checkValue(path+".value", typ, obj, val)
	}
}

func (v *validator) declare(path, id, typ, repeater string) {
	if !idRe.MatchString(id) {
		v.addf(path+".id", "ids must match %s", idRe.String())
		return
	}
	if reservedNames[id] {
		v.addf(path+".id", "%q is reserved (expression keyword or function name); pick another id", id)
		return
	}
	if prev, dup := v.fields[id]; dup {
		v.addf(path+".id", "duplicate id %q (first used at %s); ids must be unique across the card", id, prev.path)
		return
	}
	v.fields[id] = &field{id: id, typ: typ, path: path, repeater: repeater}
}

func (v *validator) tabs(path string, val any, depth int, repeater string) {
	arr, ok := val.([]any)
	if !ok || len(arr) == 0 {
		v.addf(path+".tabs", "must be a non-empty array of {label, children}")
		return
	}
	if len(arr) > MaxTabs {
		v.addf(path+".tabs", "at most %d tabs", MaxTabs)
	}
	for i, t := range arr {
		tp := fmt.Sprintf("%s.tabs[%d]", path, i)
		obj, ok := t.(map[string]any)
		if !ok {
			v.addf(tp, "must be an object {label, children}")
			continue
		}
		for k := range obj {
			if k != "label" && k != "children" {
				v.addf(tp+"."+k, "unknown property (allowed: label, children)")
			}
		}
		lbl, _ := obj["label"].(string)
		if strings.TrimSpace(lbl) == "" {
			v.addf(tp+".label", "required")
		}
		kids, ok := obj["children"].([]any)
		if !ok || len(kids) == 0 {
			v.addf(tp+".children", "must be a non-empty array of components")
			continue
		}
		for j, kid := range kids {
			v.component(fmt.Sprintf("%s.children[%d]", tp, j), kid, depth+1, repeater)
		}
	}
}

func propNames(spec compSpec) []string {
	out := []string{}
	for k := range spec.props {
		out = append(out, k)
	}
	for k := range commonProps {
		if _, dup := spec.props[k]; !dup {
			out = append(out, k)
		}
	}
	sortStrings(out)
	return out
}

func (v *validator) checkString(path, s string) {
	if utf8.RuneCountInString(s) > MaxStringLen {
		v.addf(path, "string longer than %d characters", MaxStringLen)
	}
}

func (v *validator) checkProp(path string, pr prop, val any, repeater string) {
	switch pr.kind {
	case kBool:
		if _, ok := val.(bool); !ok {
			v.addf(path, "must be true or false")
		}
	case kNumber:
		if f, ok := val.(float64); !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			v.addf(path, "must be a number")
		}
	case kInt:
		f, ok := val.(float64)
		if !ok || f != math.Trunc(f) || f < 0 {
			v.addf(path, "must be a non-negative whole number")
		}
	case kEnum:
		s, _ := val.(string)
		if !containsStr(pr.enum, s) {
			v.addf(path, "must be one of: %s", strings.Join(pr.enum, ", "))
		}
	case kString, kURL, kDate, kFieldPath, kTemplate, kExpr, kNonBlank:
		v.checkTextProp(path, pr, val, repeater)
	case kOptions, kStringList, kNumberList, kObjects, kRows:
		v.checkListProp(path, pr, val, repeater)
	case kChildren, kValue:
		// Walked / type-checked by component(), which knows the component.
	}
}

// checkTextProp checks the string-valued kinds.
func (v *validator) checkTextProp(path string, pr prop, val any, repeater string) {
	switch pr.kind {
	case kString, kURL, kDate:
		s, ok := val.(string)
		if !ok {
			v.addf(path, "must be a string")
			return
		}
		v.checkString(path, s)
		if pr.kind == kURL {
			u, err := url.Parse(s)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				v.addf(path, "must be an absolute https:// URL")
			}
		}
		if pr.kind == kDate {
			if !isCalendarDate(s) {
				v.addf(path, "must be a YYYY-MM-DD date (year 0001 or later)")
			}
		}
	case kNonBlank:
		s, ok := val.(string)
		if !ok || strings.TrimSpace(s) == "" {
			v.addf(path, "must be a non-blank string")
			return
		}
		v.checkString(path, s)
	case kFieldPath:
		s, ok := val.(string)
		if !ok {
			v.addf(path, "must be a field path string")
			return
		}
		v.fieldPaths = append(v.fieldPaths, pathRef{path: path, target: s})
	case kTemplate:
		s, ok := val.(string)
		if !ok {
			v.addf(path, "must be a string")
			return
		}
		v.checkString(path, s)
		holes, err := TemplateExprs(s)
		if err != nil {
			v.addf(path, "%v", err)
			return
		}
		for _, h := range holes {
			v.exprs = append(v.exprs, exprRef{path: path, src: h, repeater: repeater})
		}
	case kExpr:
		s, ok := val.(string)
		if !ok {
			v.addf(path, "must be an expression string")
			return
		}
		v.exprs = append(v.exprs, exprRef{path: path, src: s, repeater: repeater})
	case kStringList:
		arr, ok := val.([]any)
		if !ok {
			v.addf(path, "must be an array of strings")
			return
		}
		for i, e := range arr {
			if _, ok := e.(string); !ok {
				v.addf(fmt.Sprintf("%s[%d]", path, i), "must be a string")
				return
			}
		}
	default:
	}
}

// checkListProp checks the array-valued kinds.
func (v *validator) checkListProp(path string, pr prop, val any, repeater string) {
	switch pr.kind {
	case kOptions:
		v.options(path, val)
	case kStringList:
		arr, ok := val.([]any)
		if !ok {
			v.addf(path, "must be an array of strings")
			return
		}
		for i, e := range arr {
			if _, ok := e.(string); !ok {
				v.addf(fmt.Sprintf("%s[%d]", path, i), "must be a string")
				return
			}
		}
	case kNumberList:
		arr, ok := val.([]any)
		if !ok {
			v.addf(path, "must be an array of numbers")
			return
		}
		for i, e := range arr {
			if _, ok := e.(float64); !ok && e != nil {
				v.addf(fmt.Sprintf("%s[%d]", path, i), "must be a number (or null for a gap)")
				return
			}
		}
	case kObjects:
		arr, ok := val.([]any)
		if !ok || len(arr) == 0 {
			v.addf(path, "must be a non-empty array of objects")
			return
		}
		for i, e := range arr {
			ep := fmt.Sprintf("%s[%d]", path, i)
			obj, ok := e.(map[string]any)
			if !ok {
				v.addf(ep, "must be an object")
				continue
			}
			for k, ev := range obj {
				ip, ok := pr.items[k]
				if !ok {
					v.addf(ep+"."+k, "unknown property (allowed: %s)", strings.Join(sortedKeys(pr.items), ", "))
					continue
				}
				v.checkProp(ep+"."+k, ip, ev, repeater)
			}
			for k, ip := range pr.items {
				if _, present := obj[k]; ip.required && !present {
					v.addf(ep+"."+k, "required")
				}
			}
		}
	case kRows:
		arr, ok := val.([]any)
		if !ok {
			v.addf(path, "must be an array of row objects")
			return
		}
		if len(arr) > MaxTableRows {
			v.addf(path, "at most %d rows; summarize or paginate across cards", MaxTableRows)
		}
		for i, e := range arr {
			obj, ok := e.(map[string]any)
			if !ok {
				v.addf(fmt.Sprintf("%s[%d]", path, i), "each row must be an object keyed by column key")
				return
			}
			for k, cell := range obj {
				switch cell.(type) {
				case string, float64, bool, nil:
				default:
					v.addf(fmt.Sprintf("%s[%d].%s", path, i, k), "cells must be strings, numbers, booleans or null")
				}
			}
		}

	default:
	}
}

// optionValues decodes an options list into its values; ok=false when the
// shape is wrong (the issue is reported by options()).
func optionValues(val any) ([]string, bool) {
	arr, ok := val.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		switch o := e.(type) {
		case string:
			out = append(out, o)
		case map[string]any:
			s, ok := o["value"].(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		default:
			return nil, false
		}
	}
	return out, true
}

func (v *validator) options(path string, val any) {
	arr, ok := val.([]any)
	if !ok || len(arr) == 0 {
		v.addf(path, "must be a non-empty array of strings or {value, label, description} objects")
		return
	}
	if len(arr) > MaxOptions {
		v.addf(path, "at most %d options; narrow the list with what you already know", MaxOptions)
		return
	}
	seen := map[string]bool{}
	for i, e := range arr {
		ep := fmt.Sprintf("%s[%d]", path, i)
		var value string
		switch o := e.(type) {
		case string:
			value = o
		case map[string]any:
			s, ok := o["value"].(string)
			if !ok {
				v.addf(ep+".value", "required string")
				continue
			}
			value = s
			for k, ov := range o {
				switch k {
				case "value":
				case "label", "description":
					if _, ok := ov.(string); !ok {
						v.addf(ep+"."+k, "must be a string")
					}
				default:
					v.addf(ep+"."+k, "unknown option property (allowed: value, label, description)")
				}
			}
		default:
			v.addf(ep, "must be a string or {value, label, description}")
			continue
		}
		if strings.TrimSpace(value) == "" {
			// "" is the "nothing chosen" value of select / choice and fails
			// required — an option with it could never be a real answer.
			v.addf(ep, "option values must be non-empty")
		}
		if seen[value] {
			v.addf(ep, "duplicate option value %q", value)
		}
		seen[value] = true
	}
}

// componentRules holds the cross-property checks a flat prop table cannot
// express.
// constantConditions refuses a visible_if / disabled_if that names no input:
// it never changes, so it is either pointless or hides / disables its target
// for good — an input the user can never reach while the model waits.
func (v *validator) constantConditions(path string, obj map[string]any) {
	for _, cond := range []string{"visible_if", "disabled_if"} {
		src, ok := obj[cond].(string)
		if !ok {
			continue
		}
		refs, err := ParseExpr(src)
		if err != nil {
			continue
		}
		if len(refs) == 0 {
			v.addf(path+"."+cond, "names no input, so it never changes; drop it (a component is shown and enabled by default)")
			continue
		}
		// A visible_if that reads only inputs it hides itself (the component,
		// or inputs inside it) locks itself: once hidden, nothing the user can
		// reach changes it, and a hidden input is left out of the submission.
		if cond == "visible_if" {
			owned := map[string]map[string]any{}
			collectInputs([]any{obj}, owned)
			self := true
			for _, r := range refs {
				if _, ok := owned[r]; !ok {
					self = false
					break
				}
			}
			if self {
				v.addf(path+".visible_if", "reads only inputs it hides, so once hidden it can never be shown again; gate it on another input")
			}
		}
	}
}

func (v *validator) componentRules(path, typ string, obj map[string]any) {
	v.constantConditions(path, obj)
	num := func(k string) (float64, bool) { f, ok := obj[k].(float64); return f, ok }
	// A required input must have room for an answer: a zero cap on a
	// required field can never be satisfied.
	if req, _ := obj["required"].(bool); req {
		for _, k := range []string{"max_items", "max_length"} {
			if hi, ok := num(k); ok && hi == 0 {
				v.addf(path+"."+k, "a required field cannot have %s 0", k)
			}
		}
	}
	switch typ {
	case "section":
		// A collapsed section's header is its only way open; it needs a name.
		if c, _ := obj["collapsible"].(bool); c {
			if t, _ := obj["title"].(string); strings.TrimSpace(t) == "" {
				v.addf(path+".title", "required when collapsible: it is the toggle that opens the section")
			}
		}
	case "text_input":
		lo, okLo := num("min_length")
		hi, okHi := num("max_length")
		if okLo && okHi && lo > hi {
			v.addf(path, "min_length must not exceed max_length")
		}
	case "date":
		lo, _ := obj["min"].(string)
		hi, _ := obj["max"].(string)
		if lo != "" && hi != "" && lo > hi {
			v.addf(path, "min must not be after max")
		}
	case "slider", "number":
		lo, okLo := num("min")
		hi, okHi := num("max")
		if okLo && okHi && lo >= hi {
			v.addf(path, "min must be less than max")
		}
		if st, ok := num("step"); ok && st <= 0 {
			v.addf(path+".step", "must be greater than 0")
		}
	case "multi_select", "include_exclude":
		_, hasOpts := obj["options"]
		custom, _ := obj["allow_custom"].(bool)
		if !hasOpts && !custom {
			v.addf(path, "needs options, or allow_custom: true for free entry")
		}
	case "progress":
		if hi, ok := num("max"); ok && hi <= 0 {
			v.addf(path+".max", "must be greater than 0")
		}
	case "list_input":
		// A paste can be enormous; the card-level max_items may narrow the
		// protocol cap but never raise it (the browser enforces the cap even
		// when max_items is absent).
		if hi, ok := num("max_items"); ok && hi > MaxListItems {
			v.addf(path+".max_items", "at most %d", MaxListItems)
		}
	case "repeater":
		v.repeaterRules(path, obj, num)
	case "table":
		v.tableRules(path, obj)
	case "chart":
		v.chartRules(path, obj)
	}
}

func (v *validator) repeaterRules(path string, obj map[string]any, num func(string) (float64, bool)) {
	lo, okLo := num("min_items")
	hi, okHi := num("max_items")
	if okHi && hi > MaxRepeaterItems {
		v.addf(path+".max_items", "at most %d", MaxRepeaterItems)
	}
	// min_items is materialized eagerly by the browser (one empty item each),
	// so it is bounded by the same protocol limit.
	if okLo && lo > MaxRepeaterItems {
		v.addf(path+".min_items", "at most %d", MaxRepeaterItems)
	}
	if okLo && okHi && lo > hi {
		v.addf(path, "min_items must not exceed max_items")
	}
	if kids, ok := obj["fields"].([]any); ok {
		v.noNestedRepeater(path+".fields", kids)
	}
}

// noNestedRepeater refuses a repeater anywhere under another repeater's
// fields — through section / columns / tabs wrappers too. Card state and
// error paths are one repeater level deep ("rep[i].field"), so an inner
// repeater would write to the wrong place.
func (v *validator) noNestedRepeater(path string, kids []any) {
	for i, k := range kids {
		m, ok := k.(map[string]any)
		if !ok {
			continue
		}
		kp := fmt.Sprintf("%s[%d]", path, i)
		if t, _ := m["type"].(string); t == "repeater" {
			v.addf(kp, "repeaters cannot nest (not even inside a section, columns or tabs); flatten the inner list or use a second card")
			continue
		}
		if c, ok := m["children"].([]any); ok {
			v.noNestedRepeater(kp+".children", c)
		}
		if tabs, ok := m["tabs"].([]any); ok {
			for j, t := range tabs {
				if tm, ok := t.(map[string]any); ok {
					if c, ok := tm["children"].([]any); ok {
						v.noNestedRepeater(fmt.Sprintf("%s.tabs[%d].children", kp, j), c)
					}
				}
			}
		}
	}
}

func (v *validator) tableRules(path string, obj map[string]any) {
	cols, _ := obj["columns"].([]any)
	if len(cols) > MaxTableColumns {
		v.addf(path+".columns", "at most %d columns", MaxTableColumns)
	}
	keys := map[string]bool{}
	for i, c := range cols {
		if m, ok := c.(map[string]any); ok {
			if k, ok := m["key"].(string); ok {
				cp := fmt.Sprintf("%s.columns[%d].key", path, i)
				switch {
				case strings.TrimSpace(k) == "":
					v.addf(cp, "column keys must be non-empty")
				case keys[k]:
					v.addf(cp, "duplicate column key %q", k)
				}
				keys[k] = true
			}
		}
	}
	sel, _ := obj["select"].(string)
	if sel == "single" || sel == "multi" {
		if _, ok := obj["id"].(string); !ok {
			v.addf(path+".id", "required when select is single or multi (the selection is submitted under it)")
		}
		rk, _ := obj["row_key"].(string)
		if !keys[rk] {
			v.addf(path+".row_key", "required when selectable: the column key whose value identifies a row")
		} else {
			v.selectableRowKeys(path, rk, obj)
		}
	} else {
		if _, ok := obj["id"]; ok {
			v.addf(path+".id", "only a selectable table (select: single|multi) takes an id")
		}
		if _, ok := obj["required"]; ok {
			v.addf(path+".required", "only a selectable table (select: single|multi) can be required: a display table collects nothing")
		}
	}
}

// selectableRowKeys requires every row of a selectable table to carry a
// unique, non-empty STRING under row_key (the submitted identifier must be the
// row's own value, not display text derived from a number or null), and a
// default selection to name existing rows.
func (v *validator) selectableRowKeys(path, rk string, obj map[string]any) {
	rows, _ := obj["rows"].([]any)
	if req, _ := obj["required"].(bool); req && len(rows) == 0 {
		v.addf(path+".rows", "a required selectable table has no rows to pick from; add rows or drop required")
	}
	seen := map[string]bool{}
	for i, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		key, ok := m[rk].(string)
		rp := fmt.Sprintf("%s.rows[%d].%s", path, i, rk)
		switch {
		case !ok || strings.TrimSpace(key) == "":
			v.addf(rp, "every row of a selectable table needs a non-empty string %q (the value submitted for that row)", rk)
		case seen[key]:
			v.addf(rp, "duplicate row_key value %q; each row needs its own", key)
		default:
			seen[key] = true
		}
	}
	var picked []string
	switch val := obj["value"].(type) {
	case string:
		if val != "" {
			picked = []string{val}
		}
	case []any:
		for _, e := range val {
			if s, ok := e.(string); ok {
				picked = append(picked, s)
			}
		}
	}
	for _, k := range picked {
		if !seen[k] {
			v.addf(path+".value", "%q is not the row_key of any row", k)
		}
	}
}

func (v *validator) chartRules(path string, obj map[string]any) {
	labels, _ := obj["labels"].([]any)
	if len(labels) > MaxChartPoints {
		v.addf(path+".labels", "at most %d points", MaxChartPoints)
	}
	series, _ := obj["series"].([]any)
	if len(series) > MaxChartSeries {
		v.addf(path+".series", "at most %d series", MaxChartSeries)
	}
	for i, s := range series {
		if m, ok := s.(map[string]any); ok {
			if vals, ok := m["values"].([]any); ok && len(vals) != len(labels) {
				v.addf(fmt.Sprintf("%s.series[%d].values", path, i), "has %d values but there are %d labels", len(vals), len(labels))
			}
		}
	}
}

// checkValue type-checks an input's default value against its component.
func (v *validator) checkValue(path, typ string, obj map[string]any, val any) {
	strList := func(x any) bool {
		arr, ok := x.([]any)
		if !ok {
			return false
		}
		for _, e := range arr {
			if _, ok := e.(string); !ok {
				return false
			}
		}
		return true
	}
	inOptions := func(s string) bool {
		opts, ok := optionValues(obj["options"])
		return !ok || containsStr(opts, s)
	}
	switch typ {
	case "text_input":
		if _, ok := val.(string); !ok {
			v.addf(path, "must be a string")
		}
	case "date":
		if s, ok := val.(string); !ok {
			v.addf(path, "must be a YYYY-MM-DD string")
		} else if s != "" {
			if !isCalendarDate(s) {
				v.addf(path, "must be a YYYY-MM-DD string (year 0001 or later)")
			}
		}
	case "number", "slider":
		n, ok := val.(float64)
		if !ok && val != nil {
			v.addf(path, "must be a number")
		}
		if ok && typ == "slider" {
			v.sliderValue(path, obj, n)
		}
	case "toggle":
		if _, ok := val.(bool); !ok {
			v.addf(path, "must be true or false")
		}
	case "select", "choice":
		s, ok := val.(string)
		if !ok {
			v.addf(path, "must be a string option value")
		} else if s != "" && !inOptions(s) {
			v.addf(path, "%q is not one of the options", s)
		}
	default:
		v.checkCollectionValue(path, typ, obj, val, strList, inOptions)
	}
}

// checkCollectionValue type-checks the list- and object-valued inputs.
func (v *validator) checkCollectionValue(path, typ string, obj map[string]any, val any, strList func(any) bool, inOptions func(string) bool) {
	switch typ {
	case "multi_select", "list_input":
		if !strList(val) {
			v.addf(path, "must be an array of strings")
			return
		}
		if typ == "multi_select" {
			if custom, _ := obj["allow_custom"].(bool); !custom {
				for _, e := range val.([]any) {
					if !inOptions(e.(string)) {
						v.addf(path, "%q is not one of the options (set allow_custom: true to permit free entry)", e)
						return
					}
				}
			}
		}
		if typ == "list_input" {
			if len(val.([]any)) > MaxListItems {
				v.addf(path, "at most %d items", MaxListItems)
			}
		}
	case "include_exclude":
		m, ok := val.(map[string]any)
		if !ok {
			v.addf(path, "must be {\"include\": [...], \"exclude\": [...]}")
			return
		}
		for k, side := range m {
			if k != "include" && k != "exclude" {
				v.addf(path+"."+k, "only include and exclude are allowed")
				continue
			}
			if !strList(side) {
				v.addf(path+"."+k, "must be an array of strings")
			}
		}
		v.includeExcludeEntries(path, obj, m, inOptions)
	case "table":
		sel, _ := obj["select"].(string)
		switch sel {
		case "single":
			if _, ok := val.(string); !ok {
				v.addf(path, "a single-select table's value is one row_key string")
			}
		case "multi":
			if !strList(val) {
				v.addf(path, "a multi-select table's value is an array of row_key strings")
			}
		default:
			v.addf(path, "only a selectable table takes a value")
		}
	case "repeater":
		arr, ok := val.([]any)
		if !ok {
			v.addf(path, "must be an array of item objects keyed by the repeater's field ids")
			return
		}
		if len(arr) > MaxRepeaterItems {
			v.addf(path, "at most %d items", MaxRepeaterItems)
		}
		fields := map[string]map[string]any{}
		if kids, ok := obj["fields"].([]any); ok {
			collectInputs(kids, fields)
		}
		for i, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				v.addf(fmt.Sprintf("%s[%d]", path, i), "must be an object")
				continue
			}
			for k, fv := range m {
				ip := fmt.Sprintf("%s[%d].%s", path, i, k)
				f, known := fields[k]
				if !known {
					v.addf(ip, "not a field of this repeater (fields: %s)", strings.Join(sortedCompKeys(fields), ", "))
					continue
				}
				// Each item value is checked exactly like a top-level default.
				ft, _ := f["type"].(string)
				v.checkValue(ip, ft, f, fv)
			}
		}
	default:
		v.addf(path, "%s does not take a value", typ)
	}
}

// includeExcludeEntries refuses a default the user could not have built: an
// entry outside the options (unless allow_custom), or one in both lanes.
func (v *validator) includeExcludeEntries(path string, obj, m map[string]any, inOptions func(string) bool) {
	custom, _ := obj["allow_custom"].(bool)
	seen := map[string]string{}
	for _, k := range []string{"include", "exclude"} {
		side, _ := m[k].([]any)
		for _, e := range side {
			s, ok := e.(string)
			if !ok {
				continue
			}
			if !custom && !inOptions(s) {
				v.addf(path+"."+k, "%q is not one of the options (set allow_custom: true to permit free entry)", s)
			}
			if other, dup := seen[s]; dup && other != k {
				v.addf(path, "%q is in both include and exclude", s)
			}
			seen[s] = k
		}
	}
}

// collectInputs gathers the input components under a component list by id,
// descending through layout containers (but not into a nested repeater's
// fields).
func collectInputs(kids []any, into map[string]map[string]any) {
	for _, k := range kids {
		m, ok := k.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		if spec, ok := components[typ]; ok && (spec.input || typ == "table") {
			if id, ok := m["id"].(string); ok {
				into[id] = m
			}
		}
		if typ == "repeater" {
			continue
		}
		if c, ok := m["children"].([]any); ok {
			collectInputs(c, into)
		}
		if tabs, ok := m["tabs"].([]any); ok {
			for _, t := range tabs {
				if tm, ok := t.(map[string]any); ok {
					if c, ok := tm["children"].([]any); ok {
						collectInputs(c, into)
					}
				}
			}
		}
	}
}

func (v *validator) actions(raw any, hasInput bool) {
	if raw == nil {
		if hasInput {
			v.addf("actions", "the card has inputs but no actions; add one with kind \"submit\" so the user can send their answers")
		}
		return
	}
	arr, ok := raw.([]any)
	if !ok {
		v.addf("actions", "must be an array of {id, label, kind, ...}")
		return
	}
	if len(arr) > MaxActions {
		v.addf("actions", "at most %d actions", MaxActions)
	}
	seen := map[string]bool{}
	submit := false
	for i, a := range arr {
		ap := fmt.Sprintf("actions[%d]", i)
		obj, ok := a.(map[string]any)
		if !ok {
			v.addf(ap, "must be an object")
			continue
		}
		for k, val := range obj {
			pr, ok := actionProps[k]
			if !ok {
				v.addf(ap+"."+k, "unknown action property (allowed: %s)", strings.Join(sortedKeys(actionProps), ", "))
				continue
			}
			v.checkProp(ap+"."+k, pr, val, "")
		}
		for k, pr := range actionProps {
			if _, present := obj[k]; pr.required && !present {
				v.addf(ap+"."+k, "required")
			}
		}
		id, _ := obj["id"].(string)
		if !idRe.MatchString(id) {
			v.addf(ap+".id", "ids must match %s", idRe.String())
		}
		if seen[id] {
			v.addf(ap+".id", "duplicate action id %q", id)
		}
		seen[id] = true
		if lbl, ok := obj["label"].(string); ok && strings.TrimSpace(lbl) == "" {
			v.addf(ap+".label", "must be non-blank: it is the button's only name")
		}
		for _, cond := range []string{"visible_if", "disabled_if"} {
			src, ok := obj[cond].(string)
			if !ok {
				continue
			}
			// A condition that names no input the user can change never
			// changes, so the button is always or never available: either the
			// condition is pointless, or the card can never be answered while
			// the model waits for it.
			refs, err := ParseExpr(src)
			if err != nil {
				continue
			}
			if len(refs) == 0 {
				v.addf(ap+"."+cond, "names no input, so it never changes; drop it (an action is shown and enabled by default)")
			} else if v.allFixed(refs) {
				v.addf(ap+"."+cond, "reads only disabled inputs, which the user cannot change, so it never changes; drop it or make one of them editable")
			}
		}
		kind, _ := obj["kind"].(string)
		if kind == "" {
			kind = "submit"
		}
		if kind == "message" {
			if m, _ := obj["message"].(string); strings.TrimSpace(m) == "" {
				v.addf(ap+".message", "required for kind \"message\": the text sent as the user's reply")
			}
		} else {
			submit = true
			if _, has := obj["message"]; has {
				v.addf(ap+".message", "only kind \"message\" takes a message")
			}
		}
	}
	if hasInput && !submit {
		v.addf("actions", "the card has inputs but no submit action; add one with kind \"submit\"")
	}
}

// sliderValue refuses a slider default the native range control would coerce
// (outside [min, max], or off the step grid from min): the thumb would show
// one number while the card holds and submits another.
func (v *validator) sliderValue(path string, obj map[string]any, n float64) {
	minV, hasMin := obj["min"].(float64)
	maxV, hasMax := obj["max"].(float64)
	if (hasMin && n < minV) || (hasMax && n > maxV) {
		v.addf(path, "%v is outside the slider's min..max", n)
		return
	}
	if step, ok := obj["step"].(float64); ok && step > 0 {
		base := 0.0
		if hasMin {
			base = minV
		}
		q := (n - base) / step
		if math.Abs(q-math.Round(q)) > 1e-9 {
			v.addf(path, "%v is not on the slider's step grid (min + k × step)", n)
		}
	}
}

// isCalendarDate: a real YYYY-MM-DD day an HTML date input can hold. The
// input has no year 0, which time.Parse accepts. Mirrors isCalendarDate in
// web/src/app/chat/ui/genui/model.ts.
func isCalendarDate(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	return err == nil && t.Year() >= 1
}

// allFixed reports whether every input id in refs is one the user can never
// change: disabled itself, or a field of a disabled repeater. An unknown id is
// not fixed (the expression check reports it).
func (v *validator) allFixed(refs []string) bool {
	for _, r := range refs {
		f, ok := v.fields[r]
		if !ok {
			return false
		}
		if f.fixed {
			continue
		}
		if rep, ok := v.fields[f.repeater]; ok && f.repeater != "" && rep.fixed {
			continue
		}
		return false
	}
	return true
}

func (v *validator) fieldErrors(raw any) {
	if raw == nil {
		return
	}
	arr, ok := raw.([]any)
	if !ok {
		v.addf("field_errors", "must be an array of {field, message}")
		return
	}
	if len(arr) > MaxFieldErrors {
		v.addf("field_errors", "at most %d", MaxFieldErrors)
		return
	}
	for i, e := range arr {
		ep := fmt.Sprintf("field_errors[%d]", i)
		obj, ok := e.(map[string]any)
		if !ok {
			v.addf(ep, "must be {field, message}")
			continue
		}
		for k, val := range obj {
			pr, ok := fieldErrorProps[k]
			if !ok {
				v.addf(ep+"."+k, "unknown property (allowed: field, message)")
				continue
			}
			v.checkProp(ep+"."+k, pr, val, "")
		}
		for k, pr := range fieldErrorProps {
			if _, present := obj[k]; pr.required && !present {
				v.addf(ep+"."+k, "required")
			}
		}
	}
}

var fieldPathRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(?:\[(\d+)\]\.([A-Za-z_][A-Za-z0-9_]*))?$`)

// checkFieldPath validates a field_errors / status_list target: "id" for a
// card-level input, or "repeater[i].field" for a field of one repeater item.
func (v *validator) checkFieldPath(path string) string {
	m := fieldPathRe.FindStringSubmatch(path)
	if m == nil {
		return `must be "field_id" or "repeater_id[index].field_id"`
	}
	root, ok := v.fields[m[1]]
	if !ok {
		return fmt.Sprintf("no input with id %q", m[1])
	}
	if m[2] == "" {
		if root.repeater != "" {
			return fmt.Sprintf("%q is a field of repeater %q; address it as %s[index].%s", m[1], root.repeater, root.repeater, m[1])
		}
		return ""
	}
	if root.typ != "repeater" {
		return fmt.Sprintf("%q is not a repeater", m[1])
	}
	inner, ok := v.fields[m[3]]
	if !ok || inner.repeater != m[1] {
		return fmt.Sprintf("%q is not a field of repeater %q", m[3], m[1])
	}
	if idx, err := strconv.Atoi(m[2]); err != nil || idx >= root.items {
		return fmt.Sprintf("repeater %q opens with %d item(s); there is no item %s", m[1], root.items, m[2])
	}
	return ""
}

// initialItems is how many items a repeater shows when the card opens:
// its value's length, else max(1, min_items) — mirroring defaultValue in
// web/src/app/chat/ui/genui/model.ts.
func initialItems(obj map[string]any) int {
	if arr, ok := obj["value"].([]any); ok {
		return len(arr)
	}
	if lo, ok := obj["min_items"].(float64); ok && lo > 1 {
		return int(lo)
	}
	return 1
}

// resolveExprs parses every collected expression and checks each name it
// reads is in scope: a card-level input anywhere; inside a repeater, also
// that repeater's own fields and "index". A repeater field read from outside
// its repeater is refused with the aggregate spelling that works.
func (v *validator) resolveExprs() {
	for _, e := range v.exprs {
		refs, paths, err := parseExprPaths(e.src)
		if err != nil {
			v.addf(e.path, "expression %q: %v", truncateRunes(e.src, 120), err)
			continue
		}
		for _, r := range refs {
			if r == "index" {
				if e.repeater == "" {
					v.addf(e.path, "expression %q: index is only defined inside a repeater's fields", truncateRunes(e.src, 120))
				}
				continue
			}
			f, ok := v.fields[r]
			if !ok {
				v.addf(e.path, "expression %q reads %q, which is not an input id on this card (inputs: %s)", truncateRunes(e.src, 120), r, v.inputNames())
				continue
			}
			if f.repeater != "" && f.repeater != e.repeater {
				v.addf(e.path, "expression %q reads %q, a field of repeater %q; outside that repeater use %s.%s (an array) with sum/count/join", truncateRunes(e.src, 120), r, f.repeater, f.repeater, r)
			}
		}
		for _, mp := range paths {
			if f, ok := v.fields[mp[0]]; ok && len(mp) > 1 {
				if msg := v.memberError(f, mp[1:]); msg != "" {
					v.addf(e.path, "expression %q reads %s: %s", truncateRunes(e.src, 120), strings.Join(mp, "."), msg)
				}
			}
		}
	}
	for _, fp := range v.fieldPaths {
		if msg := v.checkFieldPath(fp.target); msg != "" {
			v.addf(fp.path, "%s", msg)
		}
	}
}

// memberError checks the member names read off a field: a repeater exposes
// its own fields (each an array across items), an include_exclude its two
// lanes, and nothing else has members. A typo here would otherwise evaluate
// to null in the browser and silently zero a total or flip a condition.
func (v *validator) memberError(f *field, members []string) string {
	switch f.typ {
	case "repeater":
		inner, ok := v.fields[members[0]]
		if !ok || inner.repeater != f.id {
			return fmt.Sprintf("%q is not a field of repeater %q (fields: %s)", members[0], f.id, v.repeaterFieldNames(f.id))
		}
		if len(members) > 1 {
			return v.memberError(inner, members[1:])
		}
		return ""
	case "include_exclude":
		if members[0] != "include" && members[0] != "exclude" {
			return fmt.Sprintf("an include_exclude value has only .include and .exclude, not %q", members[0])
		}
		if len(members) > 1 {
			return "include / exclude are lists of strings and have no fields"
		}
		return ""
	default:
		return fmt.Sprintf("%q is a %s input and has no fields", f.id, f.typ)
	}
}

func (v *validator) repeaterFieldNames(rep string) string {
	var names []string
	for id, f := range v.fields {
		if f.repeater == rep {
			names = append(names, id)
		}
	}
	sortStrings(names)
	return strings.Join(names, ", ")
}

func (v *validator) inputNames() string {
	names := make([]string, 0, len(v.fields))
	for k := range v.fields {
		names = append(names, k)
	}
	sortStrings(names)
	if len(names) == 0 {
		return "none"
	}
	if len(names) > maxNamesInHintText {
		names = append(names[:maxNamesInHintText], "…")
	}
	return strings.Join(names, ", ")
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]prop) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortedCompKeys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) { sort.Strings(s) }

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// componentProps checks one component's own properties, recursing into
// children and tabs.
func (v *validator) componentProps(path, typ string, spec compSpec, obj map[string]any, depth int, repeater string) {
	for k, val := range obj {
		pr, ok := spec.props[k]
		if !ok {
			pr, ok = commonProps[k]
		}
		if !ok {
			v.addf(path+"."+k, "unknown property for %s (allowed: %s)", typ, strings.Join(propNames(spec), ", "))
			continue
		}
		if k == "type" || k == "id" {
			continue
		}
		if pr.kind == kValue {
			continue // an input default: checked once the component is known
		}
		if pr.kind == kChildren {
			kids, ok := val.([]any)
			if !ok || len(kids) == 0 {
				v.addf(path+"."+k, "must be a non-empty array of components")
				continue
			}
			if typ == "columns" && len(kids) > MaxColumns {
				v.addf(path+"."+k, "at most %d columns", MaxColumns)
			}
			inner := repeater
			if spec.scope {
				inner, _ = obj["id"].(string)
			}
			for i, kid := range kids {
				v.component(fmt.Sprintf("%s.%s[%d]", path, k, i), kid, depth+1, inner)
			}
			continue
		}
		if k == "tabs" {
			v.tabs(path, val, depth, repeater)
			continue
		}
		scope := repeater
		if spec.scope && k == "item_label" {
			scope, _ = obj["id"].(string)
		}
		v.checkProp(path+"."+k, pr, val, scope)
	}
	for k, pr := range spec.props {
		if _, present := obj[k]; pr.required && !present {
			v.addf(path+"."+k, "required for %s", typ)
		}
	}
}
