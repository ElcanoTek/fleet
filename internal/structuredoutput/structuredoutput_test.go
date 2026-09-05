package structuredoutput

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const personSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string"},
    "age": {"type": "integer", "minimum": 0}
  },
  "required": ["name", "age"],
  "additionalProperties": false
}`

func TestValidateSchema(t *testing.T) {
	if err := ValidateSchema(json.RawMessage(personSchema)); err != nil {
		t.Errorf("valid schema rejected: %v", err)
	}
	// A schema must be a JSON object, not an array/scalar, and must be valid JSON.
	for _, bad := range []string{`[]`, `"a string"`, `42`, `null`, `{not json`, ``} {
		if err := ValidateSchema(json.RawMessage(bad)); err == nil {
			t.Errorf("expected %q to be rejected as a schema", bad)
		}
	}
}

func TestValidateSchema_ComplexityLimits(t *testing.T) {
	t.Run("bytes", func(t *testing.T) {
		raw := json.RawMessage(`{"type":"object","description":"` + strings.Repeat("x", MaxSchemaBytes) + `"}`)
		if err := ValidateSchema(raw); err == nil || !strings.Contains(err.Error(), "maximum") {
			t.Fatalf("oversized schema error = %v", err)
		}
	})

	t.Run("depth", func(t *testing.T) {
		var nested any = map[string]any{"type": "string"}
		for range MaxSchemaDepth {
			nested = map[string]any{"allOf": []any{nested}}
		}
		raw, err := json.Marshal(nested)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateSchema(raw); err == nil || !strings.Contains(err.Error(), "depth") {
			t.Fatalf("over-deep schema error = %v", err)
		}
	})

	t.Run("nodes", func(t *testing.T) {
		values := make([]any, MaxSchemaNodes)
		for i := range values {
			values[i] = i
		}
		raw, err := json.Marshal(map[string]any{"enum": values})
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) >= MaxSchemaBytes {
			t.Fatalf("node fixture unexpectedly hit byte limit first: %d", len(raw))
		}
		if err := ValidateSchema(raw); err == nil || !strings.Contains(err.Error(), "nodes") {
			t.Fatalf("over-complex schema error = %v", err)
		}
	})
}

// TestValidateSchema_RejectsExternalRef (#585): an untrusted output_schema with
// an external $ref must be rejected by the compiler's loader — WITHOUT the host
// opening the target. The proof is in the error: it is our "must be
// self-contained" refusal, identical whether or not the file:// target exists
// (no "no such file", no parse-of-content error), so no filesystem open and no
// existence oracle.
func TestValidateSchema_RejectsExternalRef(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "schema.json")
	// A well-formed schema on disk: if the loader opened and consumed it, the
	// compile would SUCCEED — the "not allowed" error proves it was never read.
	if err := os.WriteFile(existing, []byte(`{"type":"object"}`), 0o600); err != nil {
		t.Fatalf("write schema file: %v", err)
	}
	refs := map[string]string{
		"file existing": "file://" + existing,
		"file missing":  "file://" + filepath.Join(dir, "does-not-exist.json"),
		"file oracle":   "file:///etc/hostname",
		"http":          "http://169.254.169.254/latest/meta-data",
		"https":         "https://example.com/schema.json",
	}
	for name, ref := range refs {
		t.Run(name, func(t *testing.T) {
			err := ValidateSchema(json.RawMessage(`{"$ref":"` + ref + `"}`))
			if err == nil {
				t.Fatalf("schema with external $ref %q compiled; want rejection", ref)
			}
			if !strings.Contains(err.Error(), "must be self-contained") {
				t.Errorf("rejection is not the loader's refusal (the ref may have been resolved): %v", err)
			}
			if strings.Contains(err.Error(), "no such file") {
				t.Errorf("error leaks file existence (the host touched the filesystem): %v", err)
			}
		})
	}

	// Internal refs are untouched: a self-contained schema using #/definitions
	// still compiles.
	internal := `{
	  "type": "object",
	  "properties": {"person": {"$ref": "#/definitions/person"}},
	  "definitions": {"person": {"type": "string"}}
	}`
	if err := ValidateSchema(json.RawMessage(internal)); err != nil {
		t.Errorf("self-contained schema with internal $ref rejected: %v", err)
	}
}

func TestValidateOutput_Conforming(t *testing.T) {
	out, err := ValidateOutput(`{"name":"Ada","age":36}`, json.RawMessage(personSchema))
	if err != nil {
		t.Fatalf("conforming output rejected: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("returned output is not valid JSON: %v", err)
	}
	if got["name"] != "Ada" {
		t.Errorf("got %v", got)
	}
}

func TestValidateOutput_PreservesLargeInteger(t *testing.T) {
	schema := json.RawMessage(`{
	  "type":"object",
	  "properties":{"value":{"type":"integer","const":9007199254740993}},
	  "required":["value"],
	  "additionalProperties":false
	}`)
	out, err := ValidateOutput(`{"value":9007199254740993}`, schema)
	if err != nil {
		t.Fatalf("large integer output rejected: %v", err)
	}
	if string(out) != `{"value":9007199254740993}` {
		t.Fatalf("large integer was rounded: %s", out)
	}
	if _, err := ValidateOutput(`{"value":9007199254740992}`, schema); err == nil {
		t.Fatal("adjacent rounded integer unexpectedly satisfied exact const")
	}
}

func TestValidateOutput_StripsFenceAndProse(t *testing.T) {
	// Model wrapped the JSON in a ```json fence — should still validate.
	fenced := "```json\n{\"name\":\"Ada\",\"age\":36}\n```"
	if _, err := ValidateOutput(fenced, json.RawMessage(personSchema)); err != nil {
		t.Errorf("fenced output rejected: %v", err)
	}
	// Model surrounded the JSON with prose — the outermost object is extracted.
	prose := "Sure! Here is the result:\n{\"name\":\"Ada\",\"age\":36}\nLet me know if you need more."
	if _, err := ValidateOutput(prose, json.RawMessage(personSchema)); err != nil {
		t.Errorf("prose-wrapped output rejected: %v", err)
	}
}

func TestValidateOutput_Rejects(t *testing.T) {
	cases := map[string]string{
		"not json":            "this is not json at all",
		"missing required":    `{"name":"Ada"}`,             // age required
		"wrong type":          `{"name":"Ada","age":"old"}`, // age must be integer
		"additional property": `{"name":"Ada","age":36,"x":1}`,
		"empty":               "",
	}
	for label, text := range cases {
		if _, err := ValidateOutput(text, json.RawMessage(personSchema)); err == nil {
			t.Errorf("%s: expected rejection, got nil for %q", label, text)
		}
	}
}

func TestPromptAugmentation(t *testing.T) {
	if got := PromptAugmentation(nil); got != "" {
		t.Errorf("nil schema should yield empty augmentation, got %q", got)
	}
	got := PromptAugmentation(json.RawMessage(personSchema))
	if !strings.Contains(got, "STRUCTURED OUTPUT REQUIREMENT") {
		t.Errorf("augmentation missing the requirement header: %q", got)
	}
	if !strings.Contains(got, "JSON Schema:") || !strings.Contains(got, `"name"`) {
		t.Errorf("augmentation should embed the schema: %q", got)
	}
}

func TestExtractJSONCandidates(t *testing.T) {
	cases := map[string][]string{
		`{"a":1}`:                    {`{"a":1}`},
		"```json\n{\"a\":1}\n```":    {`{"a":1}`},
		"```\n[1,2,3]\n```":          {`[1,2,3]`},
		"prefix {\"a\":1} suffix":    {`{"a":1}`},
		"no json here":               nil,
		"text [1, 2] trailing words": {`[1, 2]`},
		// Multiple top-level values — the live-run shape the old single-span
		// extraction (first '{' … last '}') turned into invalid JSON.
		"step: {\"partial\":true}\nFinal: {\"a\":1}": {`{"partial":true}`, `{"a":1}`},
		// A brace inside a string must not derail the scan.
		`note {"msg":"look: } inside"} end`: {`{"msg":"look: } inside"}`},
		// A stray '{' that never closes is skipped; later values still found.
		"broken { then {\"a\":2}": {`{"a":2}`},
	}
	for in, want := range cases {
		got := extractJSONCandidates(in)
		if len(got) != len(want) {
			t.Errorf("extractJSONCandidates(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("extractJSONCandidates(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}

// TestValidateOutputPicksLastConforming pins the live-run regression: a final
// message carrying an intermediate JSON value AND a conforming restated answer
// must validate to the conforming one (the LAST), not fail outright.
func TestValidateOutputPicksLastConforming(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"sum":{"type":"integer"}},"required":["sum"],"additionalProperties":false}`)
	out, err := ValidateOutput("intermediate: {\"step\":\"compute\"}\nFinal answer:\n{\"sum\": 650}", schema)
	if err != nil {
		t.Fatalf("ValidateOutput: %v", err)
	}
	if string(out) != `{"sum":650}` {
		t.Errorf("out = %s", out)
	}
	// Conforming value first, junk after: still found (scan is order-immune).
	out, err = ValidateOutput("{\"sum\": 1} and then some prose {\"unrelated\":true}", schema)
	if err != nil || string(out) != `{"sum":1}` {
		t.Errorf("out=%s err=%v", out, err)
	}
	// Nothing conforming: a schema error, not a parse error.
	if _, err = ValidateOutput(`{"other": 1}`, schema); err == nil || !strings.Contains(err.Error(), "does not conform") {
		t.Errorf("want conform error, got %v", err)
	}
	// No JSON at all: a parse error.
	if _, err = ValidateOutput("just words", schema); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("want parse error, got %v", err)
	}
}

// TestValidateOutputReportsLastCandidateError: when nothing conforms, the
// error must describe the LAST candidate (the model's restated final answer),
// not the first intermediate value. The reverse walk sees the last candidate
// first, so a loop that overwrote the error on every iteration reported the
// least relevant failure.
func TestValidateOutputReportsLastCandidateError(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"sum":{"type":"integer"}},"required":["sum"],"additionalProperties":false}`)
	// First candidate is missing "sum"; the last has "sum" of the wrong type.
	text := "working: {\"step\":\"compute\"}\nFinal answer:\n{\"sum\": \"six hundred fifty\"}"
	_, err := ValidateOutput(text, schema)
	if err == nil {
		t.Fatal("expected a conformance error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "does not conform") {
		t.Fatalf("want conform error, got %v", err)
	}
	// The last candidate's failure is a type mismatch on "sum"; the first
	// candidate's is a missing required property (and an unexpected "step").
	if !strings.Contains(msg, "sum") || strings.Contains(msg, "step") {
		t.Errorf("error should describe the LAST candidate's failure (sum type), got: %s", msg)
	}
}
