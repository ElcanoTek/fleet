package structuredoutput

import (
	"encoding/json"
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
	for _, bad := range []string{`[]`, `"a string"`, `42`, `{not json`, ``} {
		if err := ValidateSchema(json.RawMessage(bad)); err == nil {
			t.Errorf("expected %q to be rejected as a schema", bad)
		}
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

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                    `{"a":1}`,
		"```json\n{\"a\":1}\n```":    `{"a":1}`,
		"```\n[1,2,3]\n```":          `[1,2,3]`,
		"prefix {\"a\":1} suffix":    `{"a":1}`,
		"no json here":               "no json here",
		"text [1, 2] trailing words": `[1, 2]`,
	}
	for in, want := range cases {
		if got := extractJSON(in); got != want {
			t.Errorf("extractJSON(%q) = %q, want %q", in, got, want)
		}
	}
}
