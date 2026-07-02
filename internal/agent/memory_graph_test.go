package agent

import (
	"encoding/json"
	"testing"

	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// TestMemoryGraphSchema pins the #523 extraction contract without a live
// model (the same posture as TestMemoryExtractionSchema): the schema
// compiles, conforming output validates and parses into ExtractedGraph, and
// non-conforming payloads are rejected. This is what guards
// ExtractMemoryGraph from persisting garbage the model might emit.
func TestMemoryGraphSchema(t *testing.T) {
	schema := json.RawMessage(memoryGraphSchema)

	if err := structuredoutput.ValidateSchema(schema); err != nil {
		t.Fatalf("memoryGraphSchema does not compile: %v", err)
	}

	t.Run("conforming output validates and parses", func(t *testing.T) {
		out, err := structuredoutput.ValidateOutput(
			`{"entities":[{"name":"Ada","type":"person"},{"name":"Elcano Corp","type":"organization"}],`+
				`"relations":[{"subject":"Ada","predicate":"works at","object":"Elcano Corp"},`+
				`{"subject":"Ada","predicate":"prefers","object":"tabs"}]}`, schema)
		if err != nil {
			t.Fatalf("conforming output should validate: %v", err)
		}
		g, err := parseExtractedGraph(out)
		if err != nil {
			t.Fatalf("parseExtractedGraph: %v", err)
		}
		if len(g.Entities) != 2 || g.Entities[0].Name != "Ada" || g.Entities[0].Type != "person" {
			t.Errorf("entities = %+v", g.Entities)
		}
		if len(g.Relations) != 2 || g.Relations[0].Predicate != "works at" || g.Relations[1].Object != "tabs" {
			t.Errorf("relations = %+v", g.Relations)
		}
	})

	t.Run("empty lists are valid (a fact with no graph content)", func(t *testing.T) {
		out, err := structuredoutput.ValidateOutput(`{"entities":[],"relations":[]}`, schema)
		if err != nil {
			t.Fatalf("empty graph should validate: %v", err)
		}
		g, err := parseExtractedGraph(out)
		if err != nil || len(g.Entities) != 0 || len(g.Relations) != 0 {
			t.Errorf("empty graph = %+v, %v", g, err)
		}
	})

	t.Run("blank names and partial triples are dropped at parse time", func(t *testing.T) {
		out, err := structuredoutput.ValidateOutput(
			`{"entities":[{"name":"  "}],"relations":[{"subject":"Ada","predicate":" ","object":"x"}]}`, schema)
		if err != nil {
			t.Fatalf("whitespace passes the schema (shape-only): %v", err)
		}
		g, err := parseExtractedGraph(out)
		if err != nil || len(g.Entities) != 0 || len(g.Relations) != 0 {
			t.Errorf("blank rows should be dropped, got %+v, %v", g, err)
		}
	})

	t.Run("non-conforming output is rejected", func(t *testing.T) {
		for _, bad := range []string{
			`{"entities":[]}`,                                                                  // relations required
			`{"relations":[]}`,                                                                 // entities required
			`{"entities":["Ada"],"relations":[]}`,                                              // entities must be objects
			`{"entities":[{"type":"person"}],"relations":[]}`,                                  // name required
			`{"entities":[{"name":"Ada","type":"wizard"}],"relations":[]}`,                     // type outside the closed set
			`{"entities":[],"relations":[{"subject":"a","predicate":"b"}]}`,                    // object required
			`{"entities":[],"relations":[{"subject":"a","predicate":"b","object":"c","x":1}]}`, // no extra relation fields
			`{"entities":[],"relations":[],"extra":true}`,                                      // no extra top-level fields
			`{}`, // both keys required
		} {
			if _, err := structuredoutput.ValidateOutput(bad, schema); err == nil {
				t.Errorf("expected %s to be rejected", bad)
			}
		}
	})
}
