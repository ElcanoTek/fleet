package main

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/goccy/go-yaml"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sched/handlers"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// httpMethods is the exact set of OpenAPI path-item keys that denote an
// operation. Any other key (parameters, summary, description, servers, $ref) is
// NOT a route and must be skipped, or it would phantom-fail the parity diff.
var httpMethods = map[string]bool{
	"GET": true, "PUT": true, "POST": true, "DELETE": true,
	"OPTIONS": true, "HEAD": true, "PATCH": true, "TRACE": true,
}

// TestOpenAPIRouteParity walks the REAL orchestrator router and asserts its
// {method, path} set equals the committed docs/openapi.yaml's — so the published
// contract cannot drift from the shipped routes without CI going red. It guards
// route + method (and, via TestOpenAPISecuritySchemesDefined, that every op's
// security names a defined scheme). Request/response BODY shapes are guarded
// separately and statically by TestOpenAPISchemaDrift (reusable component
// schemas vs. their backing Go models); status codes remain documentary.
func TestOpenAPIRouteParity(t *testing.T) {
	codeRoutes := liveRoutes(t)
	specRoutes := specRoutes(t)

	for r := range codeRoutes {
		if !specRoutes[r] {
			t.Errorf("route registered in code but MISSING from docs/openapi.yaml: %s", r)
		}
	}
	for r := range specRoutes {
		if !codeRoutes[r] {
			t.Errorf("path in docs/openapi.yaml but NOT registered in code: %s", r)
		}
	}
}

// liveRoutes builds the real router (pure value-init constructors, no DB) and
// walks it into a {"METHOD /path"} set.
func liveRoutes(t *testing.T) map[string]bool {
	t.Helper()
	h := handlers.New(handlers.Config{}, nil, nil)
	notes := handlers.NewNotesHandlers(nil, h)
	router, ok := buildOrchestratorMux(h, notes).(chi.Routes)
	if !ok {
		t.Fatal("orchestrator router is not chi.Routes")
	}
	out := map[string]bool{}
	if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out[strings.ToUpper(method)+" "+route] = true
		return nil
	}); err != nil {
		t.Fatalf("walk router: %v", err)
	}
	return out
}

// openAPIDoc is the minimal shape the drift test parses. It captures paths and
// security schemes (for the route + auth gates) plus the reusable component
// schemas (for the body-schema gate in TestOpenAPISchemaDrift).
type openAPIDoc struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		SecuritySchemes map[string]any        `yaml:"securitySchemes"`
		Schemas         map[string]schemaNode `yaml:"schemas"`
	} `yaml:"components"`
}

// schemaNode is the subset of an OpenAPI 3.1 schema object the body-schema gate
// reasons about: its declared type, its object properties, and which of those
// properties are required. Everything else in a schema (descriptions, examples,
// formats, enums) is documentary and not statically cross-checked against Go.
type schemaNode struct {
	Type       string                `yaml:"type"`
	Properties map[string]schemaNode `yaml:"properties"`
	Required   []string              `yaml:"required"`
	Ref        string                `yaml:"$ref"`
	Items      *schemaNode           `yaml:"items"`
}

func loadOpenAPI(t *testing.T) openAPIDoc {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	specPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "openapi.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc openAPIDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return doc
}

func specRoutes(t *testing.T) map[string]bool {
	t.Helper()
	doc := loadOpenAPI(t)
	out := map[string]bool{}
	for path, item := range doc.Paths {
		for key := range item {
			if httpMethods[strings.ToUpper(key)] {
				out[strings.ToUpper(key)+" "+path] = true
			}
		}
	}
	return out
}

// TestOpenAPISecuritySchemesDefined: every security scheme referenced by any
// operation must be defined in components.securitySchemes — the spec can't invent
// auth. (Self-consistency, so the documented auth stays honest.)
func TestOpenAPISecuritySchemesDefined(t *testing.T) {
	doc := loadOpenAPI(t)
	defined := map[string]bool{}
	for name := range doc.Components.SecuritySchemes {
		defined[name] = true
	}
	if len(defined) == 0 {
		t.Fatal("no securitySchemes defined")
	}
	var bad []string
	for path, item := range doc.Paths {
		for key, op := range item {
			if !httpMethods[strings.ToUpper(key)] {
				continue
			}
			for _, name := range securitySchemeNames(op) {
				if !defined[name] {
					bad = append(bad, key+" "+path+": "+name)
				}
			}
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("operation references undefined security scheme: %s", b)
	}
}

// securitySchemeNames pulls the scheme names out of an operation's `security`
// list ([]{schemeName: []scope}).
func securitySchemeNames(op any) []string {
	m, ok := op.(map[string]any)
	if !ok {
		return nil
	}
	sec, ok := m["security"].([]any)
	if !ok {
		return nil
	}
	var names []string
	for _, entry := range sec {
		if em, ok := entry.(map[string]any); ok {
			for name := range em {
				names = append(names, name)
			}
		}
	}
	return names
}

// ── Body-schema drift gate (issue #207) ──
//
// The route/method/security gates above prove the *wiring* matches the spec.
// They do NOT look at request/response BODIES, so a handler could rename a field
// (return `detail` while the spec promises `error`) or drop a documented field
// and CI would stay green. TestOpenAPISchemaDrift closes the tractable, fully
// static subset of that gap.
//
// WHAT IT CHECKS (deterministic, no DB, no HTTP server, no new dependency):
// for every reusable `components.schemas.<Name>` that has a backing Go model
// (see schemaModelRegistry), it walks the model's json tags via reflection and
// asserts, against the committed spec:
//
//  1. PROPERTY EXISTENCE (hard fail): every property the spec documents on the
//     schema exists as a json field on the Go type. This catches the dangerous
//     drift — a documented field the handler can never emit/accept (a rename, a
//     deletion, a typo'd field name).
//  2. REQUIRED INTEGRITY (hard fail): every name in the schema's `required:`
//     list exists as a json field AND is not `omitempty`. A `required` field the
//     handler omits when zero-valued is a contract lie the spec must not make.
//  3. TYPE-KIND AGREEMENT (hard fail): a documented property's spec `type:`
//     (string/integer/number/boolean/array/object) is compatible with the Go
//     field's reflect.Kind, so a string↔integer swap is caught.
//  4. UNDOCUMENTED FIELDS (warn, not fail): a Go json field absent from the spec
//     schema is reported via t.Logf. Many response models are a deliberate
//     superset of the curated public contract (internal/lease/persistence
//     fields), so this stays a warning rather than a false-positive failure; it
//     can be promoted to a hard fail per-schema once a schema is known-complete.
//
// WHAT IT DOES NOT CHECK (out of static reach; honestly out of scope here):
//   - Inline (anonymous) request/response schemas declared directly on an
//     operation rather than via $ref — they have no single Go type to bind to
//     (e.g. POST /tasks/model, the /mcp-servers envelope, GET /api/me which
//     returns an ad-hoc map[string]any). These are NOT in the registry.
//   - Whether a handler ACTUALLY populates a field at runtime (only the static
//     json-tag surface is visible); behavioural correctness, status codes, and
//     value-level validation remain documentary, exactly as before.
//   - Schemas whose Go backing is an unexported type in another package
//     (e.g. the workspace listing's handlers.workspaceEntry) — not reachable by
//     reflection from this package, so cross-checked structurally by hand in the
//     spec rather than gated here.
//
// schemaCoverage (below) is itself gated: every schema in the spec must be
// either registered or in the explicit, documented exclusion set, so a newly
// added schema cannot silently escape this test.

// schemaModelRegistry maps a `components.schemas.<Name>` to the Go value whose
// json tags are that schema's real shape. Only schemas with a single, exported,
// reflectable backing type appear here; inline/ad-hoc/unexported-backed schemas
// are listed in schemaExcluded with the reason.
var schemaModelRegistry = map[string]any{
	"ErrorResponse":    models.ErrorResponse{},
	"HealthResponse":   models.HealthResponse{},
	"MCPChoice":        models.MCPChoice{},
	"TaskCreate":       models.TaskCreate{},
	"Task":             models.Task{},
	"NodeRegistration": models.NodeRegistration{},
	"Node":             models.Node{},
	"APIKeyCreate":     models.APIKeyCreate{},
	"APIKeyCreated":    models.APIKeyCreated{},
	"UserCreate":       models.UserCreate{},
	"UserResponse":     models.UserResponse{},
	"LoginRequest":     models.UserLogin{},
	"LoginResponse":    models.LoginResponse{},
	"DashboardStats":   models.DashboardStats{},
	// Pre-submission cost forecast (#233/#405). The estimate handler returns
	// agentcore.CostForecast verbatim via writeJSON, so these three reusable
	// schemas are backed by the exported, reflectable agentcore types.
	"CostForecast": agentcore.CostForecast{},
	"CostRange":    agentcore.CostRange{},
	"ModelPrice":   agentcore.ModelPrice{},
}

// schemaExcluded lists every `components.schemas.<Name>` that is intentionally
// NOT bound to a Go type, each with the reason. The coverage check forces this
// list (plus the registry) to stay exhaustive, so adding a schema to the spec
// without a decision here fails the build.
var schemaExcluded = map[string]string{
	// A bare array alias (`type: array, items: $ref MCPChoice`); the element
	// type MCPChoice is gated, and the alias has no fields of its own.
	"MCPSelection": "array alias of MCPChoice (element gated separately)",
	// Backed by the unexported handlers.workspaceEntry/workspaceListResponse,
	// which reflection cannot reach from this package. Their shapes are kept in
	// sync by hand; if they are ever exported they should move into the registry.
	"WorkspaceEntry":   "backing Go type is unexported (handlers.workspaceEntry)",
	"WorkspaceListing": "backing Go type is unexported (handlers.workspaceListResponse)",
	// A documentary placeholder (LogSession is described prose-only, with no
	// properties) — there is nothing to cross-check.
	"LogSession": "documentary-only schema (no properties declared)",
}

// TestOpenAPISchemaDrift is the body-schema half of the drift gate. See the
// block comment above for the precise scope (and its honest limits).
func TestOpenAPISchemaDrift(t *testing.T) {
	doc := loadOpenAPI(t)

	// Coverage: every spec schema must be a registered model or an explicit,
	// reasoned exclusion. This stops a newly added schema from slipping past the
	// body gate unnoticed.
	for name := range doc.Components.Schemas {
		_, reg := schemaModelRegistry[name]
		_, exc := schemaExcluded[name]
		if !reg && !exc {
			t.Errorf("schema %q is in docs/openapi.yaml but neither bound to a Go model "+
				"(schemaModelRegistry) nor explicitly excluded (schemaExcluded) — add one", name)
		}
	}
	// And the reverse: a registry/exclusion entry naming a schema the spec no
	// longer defines is stale and must be removed.
	for name := range schemaModelRegistry {
		if _, ok := doc.Components.Schemas[name]; !ok {
			t.Errorf("schemaModelRegistry names %q but it is not defined in docs/openapi.yaml", name)
		}
	}
	for name := range schemaExcluded {
		if _, ok := doc.Components.Schemas[name]; !ok {
			t.Errorf("schemaExcluded names %q but it is not defined in docs/openapi.yaml", name)
		}
	}

	for name, model := range schemaModelRegistry {
		spec, ok := doc.Components.Schemas[name]
		if !ok {
			continue // reported above
		}
		t.Run(name, func(t *testing.T) {
			checkSchemaAgainstModel(t, name, spec, reflect.TypeOf(model))
		})
	}
}

// checkSchemaAgainstModel runs the four assertions (property existence, required
// integrity, type-kind agreement, undocumented-field warnings) for one schema.
func checkSchemaAgainstModel(t *testing.T, name string, spec schemaNode, typ reflect.Type) {
	t.Helper()
	goFields := jsonFieldsOf(typ)

	// 1 + 3: every documented property must exist on the Go type, with a
	// compatible kind.
	for prop, propSchema := range spec.Properties {
		field, ok := goFields[prop]
		if !ok {
			t.Errorf("schema %q documents property %q that has no json field on Go type %s "+
				"(a documented field the handler can never produce/accept — drift)",
				name, prop, typ)
			continue
		}
		if propSchema.Type != "" {
			if want := kindGroupForSpecType(propSchema.Type); want != "" {
				if got := kindGroup(field.Type); got != "" && got != want {
					t.Errorf("schema %q property %q: spec type %q (%s) is incompatible with "+
						"Go field %s.%s kind %s (%s)",
						name, prop, propSchema.Type, want, typ.Name(), field.Name, field.Type.Kind(), got)
				}
			}
		}
	}

	// 2: every required field must exist and must not be omitempty.
	for _, req := range spec.Required {
		field, ok := goFields[req]
		if !ok {
			t.Errorf("schema %q lists %q as required but Go type %s has no such json field",
				name, req, typ)
			continue
		}
		if field.omitempty {
			t.Errorf("schema %q lists %q as required, but Go field %s.%s is `omitempty` — "+
				"the handler drops it when zero-valued, so the contract is a lie",
				name, req, typ.Name(), field.Name)
		}
	}

	// 4: surface (do not fail on) Go fields the spec does not document.
	for jsonName := range goFields {
		if _, ok := spec.Properties[jsonName]; !ok {
			t.Logf("WARN: Go type %s exposes json field %q not documented in schema %q "+
				"(undocumented response/request surface; promote to a hard check once %q is complete)",
				typ.Name(), jsonName, name, name)
		}
	}
}

// jsonField is one resolved json-serialized field of a Go struct.
type jsonField struct {
	Name      string // Go field name (for messages)
	Type      reflect.Type
	omitempty bool
}

// jsonFieldsOf returns the effective json object shape of a struct type, keyed
// by json name. It honors `json:"-"` (skipped), the `,omitempty` option, and
// the default (the Go field name) when no json tag is present, and recurses into
// anonymous embedded structs so an embedded type's fields are flattened exactly
// as encoding/json would serialize them (e.g. APIKeyCreated embeds
// APIKeyResponse).
func jsonFieldsOf(t reflect.Type) map[string]jsonField {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	out := map[string]jsonField{}
	if t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		// Anonymous embedded struct with no json name → flatten its fields.
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for k, v := range jsonFieldsOf(ft) {
					if _, exists := out[k]; !exists {
						out[k] = v
					}
				}
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		out[name] = jsonField{
			Name:      f.Name,
			Type:      f.Type,
			omitempty: strings.Contains(opts, "omitempty"),
		}
	}
	return out
}

// kindGroupForSpecType maps an OpenAPI scalar `type:` to a coarse kind group
// used for compatibility checks. Unknown/compound types return "" (skip).
func kindGroupForSpecType(specType string) string {
	switch specType {
	case "string":
		return "string"
	case "integer", "number":
		return "number"
	case "boolean":
		return "boolean"
	case "array":
		return "array"
	case "object":
		return "object"
	default:
		return ""
	}
}

// kindGroup maps a Go field type to the same coarse groups, unwrapping pointers
// and treating named string types (e.g. TaskStatus, NodeStatus) as strings and
// []byte/uuid-like as strings too. Returns "" when no confident mapping exists
// (e.g. uuid.UUID is an array-backed type that serializes as a string), so the
// caller skips rather than false-fails.
func kindGroup(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// Types that marshal to a JSON string despite a non-string Kind: anything
	// implementing encoding.TextMarshaler or json.Marshaler (uuid.UUID,
	// time.Time) is documented as a string in the spec. Detect the common ones
	// structurally to avoid a false array/struct mismatch.
	if marshalsAsString(t) {
		return "string"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Bool:
		return "boolean"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Struct, reflect.Map:
		return "object"
	default:
		return ""
	}
}

// marshalsAsString reports whether values of t serialize to a JSON string via a
// TextMarshaler/json.Marshaler (uuid.UUID, time.Time). Checked on both the type
// and its pointer, matching how encoding/json resolves the interface.
func marshalsAsString(t reflect.Type) bool {
	textMarshaler := reflect.TypeOf((*interface{ MarshalText() ([]byte, error) })(nil)).Elem()
	if t.Implements(textMarshaler) || reflect.PointerTo(t).Implements(textMarshaler) {
		return true
	}
	return false
}
