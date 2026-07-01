package agent

import (
	"encoding/json"
	"testing"

	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// TestMemoryExtractionSchema pins the extraction contract without a live model:
// the schema compiles, a conforming {"facts":[...]} validates and parses, and a
// non-conforming payload is rejected. This is what guards ExtractMemories from
// silently accepting garbage the model might emit.
func TestMemoryExtractionSchema(t *testing.T) {
	schema := json.RawMessage(memoryExtractionSchema)

	if err := structuredoutput.ValidateSchema(schema); err != nil {
		t.Fatalf("memoryExtractionSchema does not compile: %v", err)
	}

	t.Run("conforming output validates and parses", func(t *testing.T) {
		out, err := structuredoutput.ValidateOutput(`{"facts":["uses ruff for linting","prod db host is db.prod.internal"]}`, schema)
		if err != nil {
			t.Fatalf("conforming output should validate: %v", err)
		}
		var parsed struct {
			Facts []string `json:"facts"`
		}
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(parsed.Facts) != 2 || parsed.Facts[0] != "uses ruff for linting" {
			t.Errorf("facts = %+v", parsed.Facts)
		}
	})

	t.Run("empty facts array is valid (the common no-op answer)", func(t *testing.T) {
		if _, err := structuredoutput.ValidateOutput(`{"facts":[]}`, schema); err != nil {
			t.Errorf("empty facts should validate: %v", err)
		}
	})

	t.Run("non-conforming output is rejected", func(t *testing.T) {
		// facts must be an array of strings, and no extra properties are allowed.
		for _, bad := range []string{
			`{"facts":"not-an-array"}`,
			`{"facts":[1,2,3]}`,
			`{"facts":["ok"],"extra":true}`,
			`{}`,
		} {
			if _, err := structuredoutput.ValidateOutput(bad, schema); err == nil {
				t.Errorf("expected %s to be rejected", bad)
			}
		}
	})
}
