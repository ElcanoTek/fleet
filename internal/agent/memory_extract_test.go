package agent

import (
	"encoding/json"
	"testing"

	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// TestMemoryExtractionSchema pins the extraction contract without a live model:
// the schema compiles, a conforming {"facts":[{...}]} validates and parses, and
// a non-conforming payload is rejected. This is what guards ExtractMemories
// from silently accepting garbage the model might emit. Since #515 each fact is
// a typed OBJECT (content + optional kind + optional replaces number).
func TestMemoryExtractionSchema(t *testing.T) {
	schema := json.RawMessage(memoryExtractionSchema)

	if err := structuredoutput.ValidateSchema(schema); err != nil {
		t.Fatalf("memoryExtractionSchema does not compile: %v", err)
	}

	t.Run("conforming output validates and parses", func(t *testing.T) {
		out, err := structuredoutput.ValidateOutput(
			`{"facts":[{"content":"uses ruff for linting","kind":"preference"},{"content":"prod db host is db.prod.internal","replaces":2}]}`, schema)
		if err != nil {
			t.Fatalf("conforming output should validate: %v", err)
		}
		var parsed struct {
			Facts []struct {
				Content  string `json:"content"`
				Kind     string `json:"kind"`
				Replaces int    `json:"replaces"`
			} `json:"facts"`
		}
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(parsed.Facts) != 2 || parsed.Facts[0].Content != "uses ruff for linting" ||
			parsed.Facts[0].Kind != "preference" || parsed.Facts[1].Replaces != 2 {
			t.Errorf("facts = %+v", parsed.Facts)
		}
	})

	t.Run("empty facts array is valid (the common no-op answer)", func(t *testing.T) {
		if _, err := structuredoutput.ValidateOutput(`{"facts":[]}`, schema); err != nil {
			t.Errorf("empty facts should validate: %v", err)
		}
	})

	t.Run("non-conforming output is rejected", func(t *testing.T) {
		for _, bad := range []string{
			`{"facts":"not-an-array"}`,
			`{"facts":["bare-string"]}`,                  // pre-#515 shape: facts must be objects now
			`{"facts":[{"kind":"fact"}]}`,                // content is required
			`{"facts":[{"content":"ok","kind":"vibe"}]}`, // kind outside the closed set
			`{"facts":[{"content":"ok","replaces":0}]}`,  // replaces is 1-based
			`{"facts":[{"content":"ok","extra":true}]}`,  // no extra fact fields
			`{"facts":[{"content":"ok"}],"extra":true}`,  // no extra top-level fields
			`{}`, // facts required
		} {
			if _, err := structuredoutput.ValidateOutput(bad, schema); err == nil {
				t.Errorf("expected %s to be rejected", bad)
			}
		}
	})
}
