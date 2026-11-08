// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package apispec

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/goccy/go-yaml"
)

const testBase = `openapi: 3.1.0
info: { title: t, version: "1" }
components:
  securitySchemes:
    key: { type: apiKey, in: header, name: X-API-Key }
  schemas:
    Thing: { type: object }
  responses:
    Error: { description: Error }
`

func noop(http.ResponseWriter, *http.Request) {}

func testRouter() chi.Router {
	r := chi.NewRouter()
	r.Get("/things", noop)
	r.Get("/things/{id}", noop)
	r.Delete("/things/{id}", noop)
	r.Get("/things/{id}/parts/{n}", noop)
	return r
}

func testFamily() Family {
	keyed := AnyOf("key")
	return Family{
		Name: "test",
		Base: []byte(testBase),
		Operations: map[string]Operation{
			"GET /things": {
				Summary: "List things.", Security: Public(), Permission: "none",
				Parameters: []Parameter{{Name: "limit", In: "query", Schema: Typed("integer")}},
				Responses:  []Response{OK("200", "OK", JSON(ArrayOf(Ref("Thing"))))},
			},
			"GET /things/{id}": {
				Summary: "Get a thing.", Security: keyed, Permission: "view",
				Responses: []Response{OK("200", "OK", JSON(Ref("Thing"))), RefResponse("404", "Error")},
			},
			"DELETE /things/{id}": {
				Summary: "Delete a thing.", Security: keyed, Permission: "admin",
				Responses: []Response{OK("204", "Deleted")},
			},
			"GET /things/{id}/parts/{n}": {
				Summary: "Get a part.", Security: keyed, Permission: "view",
				Responses: []Response{OK("200", "OK", JSON(Object(Prop("n", Typed("integer")))))},
			},
		},
		PathParams: map[string]*Schema{"n": Typed("integer")},
	}
}

func generate(t *testing.T, f Family, r chi.Routes) ([]byte, error) {
	t.Helper()
	routes, err := WalkChi(r)
	if err != nil {
		t.Fatal(err)
	}
	return Generate(f, routes)
}

func TestGenerateDerivesPathsAndParams(t *testing.T) {
	out, err := generate(t, testFamily(), testRouter())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), testBase) {
		t.Fatal("the base document must be copied byte for byte")
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("generated document does not parse: %v\n%s", err, out)
	}
	if len(doc.Paths) != 3 {
		t.Fatalf("want 3 paths, got %v", doc.Paths)
	}
	item := doc.Paths["/things/{id}/parts/{n}"]
	params, _ := item["parameters"].([]any)
	if len(params) != 2 {
		t.Fatalf("want two derived path parameters, got %v", item["parameters"])
	}
	n := params[1].(map[string]any)
	if n["name"] != "n" || n["in"] != "path" || n["required"] != true || n["schema"].(map[string]any)["type"] != "integer" {
		t.Fatalf("path parameter n: %v", n)
	}
	if _, ok := doc.Paths["/things/{id}"]["delete"]; !ok {
		t.Fatal("DELETE /things/{id} missing")
	}
	if sec := doc.Paths["/things"]["get"].(map[string]any)["security"]; len(sec.([]any)) != 0 {
		t.Fatalf("Public() must be `security: []`, got %v", sec)
	}

	again, err := generate(t, testFamily(), testRouter())
	if err != nil || string(again) != string(out) {
		t.Fatal("Generate is not deterministic")
	}
}

func TestGenerateRefusesDrift(t *testing.T) {
	r := testRouter()
	r.Post("/things", noop) // registered, not described
	f := testFamily()
	f.Operations["GET /gone"] = Operation{Summary: "x", Security: Public(), Permission: "none", Responses: []Response{OK("200", "OK")}}
	_, err := generate(t, f, r)
	if err == nil {
		t.Fatal("want an error for route/catalog drift")
	}
	for _, want := range []string{
		"route POST /things is registered but has no Operation",
		"Operation GET /gone describes a route the router does not register",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestGenerateRefusesBadOperations(t *testing.T) {
	f := testFamily()
	f.Operations["GET /things"] = Operation{
		Summary:    "List things.",
		Permission: "none",
		Responses:  []Response{OK("200", "OK", JSON(Ref("Nope"))), RefResponse("404", "Missing")},
	}
	f.Operations["GET /things/{id}"] = Operation{
		Summary: "Get a thing.", Security: AnyOf("cookie"),
		Parameters: []Parameter{{Name: "id", In: "path"}},
		Responses:  []Response{OK("200", "OK", JSON(&Schema{Type: "object", Required: []string{"x"}, Properties: []Property{Prop("y", Typed("string"))}}))},
	}
	_, err := generate(t, f, testRouter())
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{
		"GET /things: Operation declares no Security",
		`schema "Nope" is not defined`,
		`response "Missing" is not defined`,
		`security scheme "cookie" is not defined`,
		"GET /things/{id}: Operation has no Permission",
		"path parameter id is derived from the pattern",
		`required property "x" is not among the properties`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestGenerateRefusesPathsInBase(t *testing.T) {
	f := testFamily()
	f.Base = []byte(testBase + "paths: {}\n")
	if _, err := generate(t, f, testRouter()); err == nil || !strings.Contains(err.Error(), "paths are generated") {
		t.Fatalf("want a refusal of a hand-written paths key, got %v", err)
	}
}

// TestGeneratePrefixSelectsAFamily is the Phase 3 seam: one router carrying
// several families, each generated from the routes under its mount point.
func TestGeneratePrefixSelectsAFamily(t *testing.T) {
	root := chi.NewRouter()
	root.Mount("/v1", testRouter())
	root.Get("/other", noop) // another family's route: ignored, not an error
	f := testFamily()
	f.Prefix = "/v1"
	out, err := generate(t, f, root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "/other") || !strings.Contains(string(out), "  /things/{id}:\n") {
		t.Fatalf("prefix not applied:\n%s", out)
	}
}
