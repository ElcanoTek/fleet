package main

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/goccy/go-yaml"

	"github.com/ElcanoTek/fleet/internal/sched/handlers"
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
// security names a defined scheme); it does NOT guard request/response BODIES or
// status codes — those are documentation, not gated here.
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

// openAPIDoc is the minimal shape the drift test parses.
type openAPIDoc struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		SecuritySchemes map[string]any `yaml:"securitySchemes"`
	} `yaml:"components"`
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
