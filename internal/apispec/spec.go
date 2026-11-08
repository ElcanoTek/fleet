// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

// Package apispec is fleet's code-first HTTP contract layer (ADR-0109,
// docs/HTTP-CONTRACTS.md). A handler family describes each route it registers
// as an Operation, keyed by "METHOD /pattern" exactly as chi registers it, and
// Generate joins those descriptions with the routes a real router walk
// reports to produce the `paths` of an OpenAPI 3.1 document.
//
// The router stays the source of truth for which routes exist. Generate
// refuses a registered route with no Operation and an Operation with no
// registered route, so the document cannot list a route the server lacks or
// miss one it serves. What the router cannot know — the summary, the
// permission a caller needs, the bodies — is what an Operation carries.
//
// What is derived rather than written by hand:
//
//   - the path list and each path's methods (the router walk);
//   - path parameters: one per {name} in the pattern, `in: path`,
//     `required: true`, typed from Family.PathParams (string by default);
//   - `$ref` targets: every Ref must name a schema (or response) the base
//     document defines, or Generate fails;
//   - the `security` requirement's presence: an Operation that declares
//     neither Public() nor AnyOf(...) is an error, not a silently public route.
//
// The document-level parts (info, servers, the vendor extensions, components)
// come from a hand-maintained base document (Family.Base); see
// docs/HTTP-CONTRACTS.md for what moves out of it in Phase 3/4 of #2050.
package apispec

// Operation describes one route: what the OpenAPI operation object for
// "METHOD /pattern" says. Fields map one-to-one onto OpenAPI keys, so the
// generated YAML reads like the struct.
type Operation struct {
	Summary     string
	Description string
	// Security is required: Public() for a route that takes no credential,
	// AnyOf(scheme...) otherwise. The zero value is rejected by Generate.
	Security Security
	// Permission is the x-fleet-permission vendor extension: the
	// models.Permission an API-key principal needs to be admitted, or one of
	// none / authenticated / webhook / hmac (docs/openapi.yaml info).
	Permission  string
	Parameters  []Parameter // query and header parameters; path ones are derived
	RequestBody *RequestBody
	Responses   []Response
}

// Security is an operation's `security` list: alternatives, any one of which
// admits the request. Construct it with Public or AnyOf.
type Security struct {
	schemes []string
	set     bool
}

// Public is `security: []` — the operation requires no credential.
func Public() Security { return Security{set: true} }

// AnyOf is a security list whose every entry names one scheme
// (`[{a: []}, {b: []}]`): any one of them admits the request.
func AnyOf(schemes ...string) Security { return Security{schemes: schemes, set: true} }

// Parameter is a non-path parameter (path parameters are derived from the
// route pattern; see Family.PathParams).
type Parameter struct {
	Name        string
	In          string // query | header
	Description string
	Required    bool
	Schema      *Schema
}

// RequestBody is an operation's request body.
type RequestBody struct {
	Description string
	Required    bool
	Content     []Media
}

// Response is one status code's response. Ref, when set, is the name of a
// components.responses entry and every other field must be empty.
type Response struct {
	Code        string
	Ref         string
	Description string
	Headers     []Header
	Content     []Media
}

// Header is a response header.
type Header struct {
	Name   string
	Schema *Schema
}

// Media is one media type of a body: `content: {<Type>: {schema: ...}}`.
type Media struct {
	Type   string
	Schema *Schema
}

// Schema is the subset of a JSON Schema / OpenAPI 3.1 schema object fleet's
// contracts use. Ref names a components.schemas entry (not a JSON pointer).
// Numeric bounds and examples are `any` so an integer bound stays an integer
// in the YAML.
type Schema struct {
	Ref                  string
	Type                 string
	Format               string
	Description          string
	Enum                 []any
	Default              any
	Nullable             bool
	Deprecated           bool
	Pattern              string
	Example              any
	Minimum              any
	Maximum              any
	MinLength            any
	MaxLength            any
	MaxItems             any
	Items                *Schema
	Properties           []Property
	Required             []string
	AdditionalProperties *Schema
	AllOf                []*Schema
}

// Property is one named property of an object schema, in document order.
type Property struct {
	Name   string
	Schema *Schema
}

// ── Constructors: keep a family's catalog short and readable. ──

// Ref is a schema that is `$ref: "#/components/schemas/<name>"`.
func Ref(name string) *Schema { return &Schema{Ref: name} }

// JSON is an application/json body with the given schema.
func JSON(s *Schema) Media { return Media{Type: "application/json", Schema: s} }

// OK is a response with a description and, optionally, content.
func OK(code, description string, content ...Media) Response {
	return Response{Code: code, Description: description, Content: content}
}

// RefResponse is `<code>: {$ref: "#/components/responses/<name>"}`.
func RefResponse(code, name string) Response { return Response{Code: code, Ref: name} }

// Prop is a Property constructor for object schemas.
func Prop(name string, s *Schema) Property { return Property{Name: name, Schema: s} }

// Object is `type: object` with the given properties in order.
func Object(props ...Property) *Schema { return &Schema{Type: "object", Properties: props} }

// Typed is a schema with only a type (and optionally a format).
func Typed(typ string, format ...string) *Schema {
	s := &Schema{Type: typ}
	if len(format) > 0 {
		s.Format = format[0]
	}
	return s
}

// ArrayOf is `type: array` with the given items.
func ArrayOf(items *Schema) *Schema { return &Schema{Type: "array", Items: items} }
