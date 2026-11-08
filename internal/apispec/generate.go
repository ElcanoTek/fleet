// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package apispec

import (
	"bytes"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/goccy/go-yaml"
)

// Route is one registered {method, pattern} pair, as a router walk reports it.
type Route struct {
	Method  string
	Pattern string
}

// Key is the catalog key for a route: "METHOD /pattern".
func (r Route) Key() string { return r.Method + " " + r.Pattern }

// Family is one handler family's contract: the operations it describes and
// the hand-maintained document they are generated into.
type Family struct {
	// Name labels errors ("orchestrator").
	Name string
	// Prefix is where the family is mounted in the router being walked. Only
	// routes under it belong to the family, and it is stripped before the
	// catalog lookup, so the document's paths stay relative to its servers
	// URL. Empty today: the orchestrator router is walked on its own. Phase 3
	// (one chi tree, #2050) walks the unified router once and gives each
	// family its mount point here.
	Prefix string
	// Base is the document without its `paths` key: openapi, info, servers,
	// the x-fleet-* extensions and components. The generated document is
	// Base, unchanged byte for byte, followed by the generated `paths:`.
	Base []byte
	// Operations is keyed by Route.Key() of the route as registered
	// (after Prefix is stripped).
	Operations map[string]Operation
	// PathParams types path parameters by name. A name not listed is
	// `{type: string}`. One entry per name keeps a parameter's type the same
	// on every route that uses it.
	PathParams map[string]*Schema
}

// WalkChi lists every route a chi router registers.
func WalkChi(r chi.Routes) ([]Route, error) {
	var out []Route
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, Route{Method: strings.ToUpper(method), Pattern: route})
		return nil
	})
	return out, err
}

// methodOrder is the order methods appear in a path item.
var methodOrder = []string{"GET", "PUT", "POST", "DELETE", "OPTIONS", "HEAD", "PATCH", "TRACE"}

var pathParamRe = regexp.MustCompile(`\{([^{}/]+)\}`)

// baseDoc is what Generate reads back from Family.Base to check references.
type baseDoc struct {
	Paths      any `yaml:"paths"`
	Components struct {
		Schemas         map[string]any `yaml:"schemas"`
		Responses       map[string]any `yaml:"responses"`
		SecuritySchemes map[string]any `yaml:"securitySchemes"`
	} `yaml:"components"`
}

// Generate returns the family's OpenAPI document for the given routes. It
// fails, listing every problem at once, when a route has no operation, an
// operation has no route, or an operation names a scheme, schema or response
// the base document does not define.
func Generate(f Family, routes []Route) ([]byte, error) {
	var base baseDoc
	if err := yaml.Unmarshal(f.Base, &base); err != nil {
		return nil, fmt.Errorf("%s: base document: %w", f.Name, err)
	}
	if base.Paths != nil {
		return nil, fmt.Errorf("%s: base document has a paths key; paths are generated", f.Name)
	}
	if !bytes.HasSuffix(f.Base, []byte("\n")) {
		return nil, fmt.Errorf("%s: base document must end with a newline", f.Name)
	}

	var problems []string
	// Group the family's routes by path.
	byPath := map[string]map[string]Operation{}
	seen := map[string]bool{}
	for _, r := range routes {
		if f.Prefix != "" {
			if r.Pattern != f.Prefix && !strings.HasPrefix(r.Pattern, f.Prefix+"/") {
				continue
			}
			r.Pattern = strings.TrimPrefix(r.Pattern, f.Prefix)
		}
		key := r.Key()
		if seen[key] {
			continue
		}
		seen[key] = true
		if !slices.Contains(methodOrder, r.Method) {
			problems = append(problems, fmt.Sprintf("route %s: method %s has no OpenAPI operation key", key, r.Method))
			continue
		}
		op, ok := f.Operations[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("route %s is registered but has no Operation (describe it in the %s catalog)", key, f.Name))
			continue
		}
		if byPath[r.Pattern] == nil {
			byPath[r.Pattern] = map[string]Operation{}
		}
		byPath[r.Pattern][r.Method] = op
	}
	if len(seen) == 0 {
		problems = append(problems, fmt.Sprintf("the router registers no routes under %q", f.Prefix))
	}
	for key := range f.Operations {
		if !seen[key] {
			problems = append(problems, fmt.Sprintf("Operation %s describes a route the router does not register", key))
		}
	}

	refs := refChecker{base: &base}
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var out yaml.MapSlice
	for _, p := range paths {
		var item yaml.MapSlice
		if names := pathParamRe.FindAllStringSubmatch(p, -1); len(names) > 0 {
			var params []any
			for _, m := range names {
				s := f.PathParams[m[1]]
				if s == nil {
					s = &Schema{Type: "string"}
				}
				params = append(params, yaml.MapSlice{
					{Key: "name", Value: m[1]},
					{Key: "in", Value: "path"},
					{Key: "required", Value: true},
					{Key: "schema", Value: refs.schema(s, "path parameter "+m[1]+" of "+p)},
				})
			}
			item = append(item, yaml.MapItem{Key: "parameters", Value: params})
		}
		for _, m := range methodOrder {
			op, ok := byPath[p][m]
			if !ok {
				continue
			}
			where := m + " " + p
			item = append(item, yaml.MapItem{Key: strings.ToLower(m), Value: refs.operation(op, where)})
		}
		out = append(out, yaml.MapItem{Key: p, Value: item})
	}
	problems = append(problems, refs.problems...)
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("%s contract:\n  %s", f.Name, strings.Join(problems, "\n  "))
	}

	body, err := yaml.MarshalWithOptions(yaml.MapSlice{{Key: "paths", Value: out}},
		yaml.Indent(2), yaml.IndentSequence(true), yaml.UseLiteralStyleIfMultiline(true))
	if err != nil {
		return nil, fmt.Errorf("%s: marshal paths: %w", f.Name, err)
	}
	doc := append([]byte{}, f.Base...)
	doc = append(doc, generatedMarker...)
	return append(doc, body...), nil
}

// generatedMarker heads the generated half of the document.
const generatedMarker = "# ── Generated from the router and the route catalog (internal/apispec, ADR-0109). Do not edit below this line: `make api-contracts`. ──\n"

// refChecker converts the typed operation into YAML and records every
// reference the base document cannot resolve.
type refChecker struct {
	base     *baseDoc
	problems []string
}

func (c *refChecker) failf(format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

func (c *refChecker) operation(op Operation, where string) yaml.MapSlice {
	var m yaml.MapSlice
	if op.Summary == "" {
		c.failf("%s: Operation has no Summary", where)
	}
	m = appendStr(m, "summary", op.Summary)
	m = appendStr(m, "description", op.Description)
	if !op.Security.set {
		c.failf("%s: Operation declares no Security (use apispec.Public() or apispec.AnyOf(...))", where)
	}
	sec := make([]any, 0, len(op.Security.schemes))
	for _, s := range op.Security.schemes {
		if _, ok := c.base.Components.SecuritySchemes[s]; !ok {
			c.failf("%s: security scheme %q is not defined in components.securitySchemes", where, s)
		}
		sec = append(sec, yaml.MapSlice{{Key: s, Value: []any{}}})
	}
	m = append(m, yaml.MapItem{Key: "security", Value: sec})
	if op.Permission == "" {
		c.failf("%s: Operation has no Permission (x-fleet-permission)", where)
	}
	m = appendStr(m, "x-fleet-permission", op.Permission)
	if len(op.Parameters) > 0 {
		params := make([]any, 0, len(op.Parameters))
		for _, p := range op.Parameters {
			if p.In == "path" {
				c.failf("%s: path parameter %s is derived from the pattern; type it in Family.PathParams", where, p.Name)
			}
			var pm yaml.MapSlice
			pm = append(pm, yaml.MapItem{Key: "name", Value: p.Name}, yaml.MapItem{Key: "in", Value: p.In})
			pm = appendStr(pm, "description", p.Description)
			if p.Required {
				pm = append(pm, yaml.MapItem{Key: "required", Value: true})
			}
			if p.Schema != nil {
				pm = append(pm, yaml.MapItem{Key: "schema", Value: c.schema(p.Schema, where+" parameter "+p.Name)})
			}
			params = append(params, pm)
		}
		m = append(m, yaml.MapItem{Key: "parameters", Value: params})
	}
	if rb := op.RequestBody; rb != nil {
		var bm yaml.MapSlice
		bm = appendStr(bm, "description", rb.Description)
		if rb.Required {
			bm = append(bm, yaml.MapItem{Key: "required", Value: true})
		}
		bm = append(bm, yaml.MapItem{Key: "content", Value: c.content(rb.Content, where+" request body")})
		m = append(m, yaml.MapItem{Key: "requestBody", Value: bm})
	}
	if len(op.Responses) == 0 {
		c.failf("%s: Operation has no Responses", where)
	}
	rs := make(yaml.MapSlice, 0, len(op.Responses))
	codes := map[string]bool{}
	for _, r := range op.Responses {
		if codes[r.Code] {
			c.failf("%s: response %s is listed twice", where, r.Code)
		}
		codes[r.Code] = true
		rs = append(rs, yaml.MapItem{Key: r.Code, Value: c.response(r, where+" response "+r.Code)})
	}
	return append(m, yaml.MapItem{Key: "responses", Value: rs})
}

func (c *refChecker) response(r Response, where string) yaml.MapSlice {
	if r.Ref != "" {
		if r.Description != "" || len(r.Headers) > 0 || len(r.Content) > 0 {
			c.failf("%s: a $ref response carries nothing else", where)
		}
		if _, ok := c.base.Components.Responses[r.Ref]; !ok {
			c.failf("%s: response %q is not defined in components.responses", where, r.Ref)
		}
		return yaml.MapSlice{{Key: "$ref", Value: "#/components/responses/" + r.Ref}}
	}
	if r.Description == "" {
		c.failf("%s: a response needs a description", where)
	}
	m := yaml.MapSlice{{Key: "description", Value: r.Description}}
	if len(r.Headers) > 0 {
		var hs yaml.MapSlice
		for _, h := range r.Headers {
			hs = append(hs, yaml.MapItem{Key: h.Name, Value: yaml.MapSlice{{Key: "schema", Value: c.schema(h.Schema, where+" header "+h.Name)}}})
		}
		m = append(m, yaml.MapItem{Key: "headers", Value: hs})
	}
	if len(r.Content) > 0 {
		m = append(m, yaml.MapItem{Key: "content", Value: c.content(r.Content, where)})
	}
	return m
}

func (c *refChecker) content(media []Media, where string) yaml.MapSlice {
	m := make(yaml.MapSlice, 0, len(media))
	for _, md := range media {
		var mm yaml.MapSlice
		if md.Schema != nil {
			mm = append(mm, yaml.MapItem{Key: "schema", Value: c.schema(md.Schema, where+" "+md.Type)})
		}
		var v any = mm
		if mm == nil {
			v = yaml.MapSlice{}
		}
		m = append(m, yaml.MapItem{Key: md.Type, Value: v})
	}
	return m
}

func (c *refChecker) schema(s *Schema, where string) yaml.MapSlice {
	if s == nil {
		c.failf("%s: nil schema", where)
		return yaml.MapSlice{}
	}
	var m yaml.MapSlice
	if s.Ref != "" {
		if _, ok := c.base.Components.Schemas[s.Ref]; !ok {
			c.failf("%s: schema %q is not defined in components.schemas", where, s.Ref)
		}
		m = append(m, yaml.MapItem{Key: "$ref", Value: "#/components/schemas/" + s.Ref})
	}
	m = appendStr(m, "type", s.Type)
	m = appendStr(m, "format", s.Format)
	m = appendStr(m, "description", s.Description)
	if len(s.Enum) > 0 {
		m = append(m, yaml.MapItem{Key: "enum", Value: s.Enum})
	}
	m = appendAny(m, "default", s.Default)
	if s.Nullable {
		m = append(m, yaml.MapItem{Key: "nullable", Value: true})
	}
	if s.Deprecated {
		m = append(m, yaml.MapItem{Key: "deprecated", Value: true})
	}
	m = appendStr(m, "pattern", s.Pattern)
	m = appendAny(m, "example", s.Example)
	m = appendAny(m, "minimum", s.Minimum)
	m = appendAny(m, "maximum", s.Maximum)
	m = appendAny(m, "minLength", s.MinLength)
	m = appendAny(m, "maxLength", s.MaxLength)
	m = appendAny(m, "maxItems", s.MaxItems)
	if s.Items != nil {
		m = append(m, yaml.MapItem{Key: "items", Value: c.schema(s.Items, where+" items")})
	}
	if len(s.Properties) > 0 {
		var ps yaml.MapSlice
		for _, p := range s.Properties {
			ps = append(ps, yaml.MapItem{Key: p.Name, Value: c.schema(p.Schema, where+"."+p.Name)})
		}
		m = append(m, yaml.MapItem{Key: "properties", Value: ps})
	}
	if len(s.Required) > 0 {
		have := map[string]bool{}
		for _, p := range s.Properties {
			have[p.Name] = true
		}
		for _, r := range s.Required {
			if len(s.Properties) > 0 && !have[r] {
				c.failf("%s: required property %q is not among the properties", where, r)
			}
		}
		m = append(m, yaml.MapItem{Key: "required", Value: s.Required})
	}
	if s.AdditionalProperties != nil {
		m = append(m, yaml.MapItem{Key: "additionalProperties", Value: c.schema(s.AdditionalProperties, where+" additionalProperties")})
	}
	if len(s.AllOf) > 0 {
		var all []any
		for _, a := range s.AllOf {
			all = append(all, c.schema(a, where+" allOf"))
		}
		m = append(m, yaml.MapItem{Key: "allOf", Value: all})
	}
	if m == nil {
		// `{}` is the any-value schema; keep it as an explicit empty map.
		return yaml.MapSlice{}
	}
	return m
}

func appendStr(m yaml.MapSlice, k, v string) yaml.MapSlice {
	if v == "" {
		return m
	}
	return append(m, yaml.MapItem{Key: k, Value: v})
}

func appendAny(m yaml.MapSlice, k string, v any) yaml.MapSlice {
	if v == nil {
		return m
	}
	return append(m, yaml.MapItem{Key: k, Value: v})
}
