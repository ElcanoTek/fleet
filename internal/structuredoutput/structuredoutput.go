// Package structuredoutput implements the schema side of structured-output mode
// (#244): compiling a task's declared JSON Schema at create time and validating
// an agent's final answer against it after a run. It is the single source of
// truth for both checks so the create-time gate and the post-run validation can
// never disagree on what "valid" means.
package structuredoutput

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// schemaResourceURL is the in-memory resource name the compiler keys the schema
// under; it never touches the network or filesystem.
const schemaResourceURL = "fleet://output_schema.json"

// CompileSchema validates that raw is a usable draft-07-style JSON Schema object
// and returns the compiled schema. A nil/empty raw is a programming error here
// (callers gate on len first); a non-object or uncompilable schema returns an
// error suitable for surfacing to the task author at create time.
func CompileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty schema")
	}
	// A JSON Schema must itself be a JSON object — reject arrays/scalars early
	// with a clearer message than the compiler's.
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("must be a JSON object: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaResourceURL, bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	sch, err := c.Compile(schemaResourceURL)
	if err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	return sch, nil
}

// ValidateSchema reports whether raw is a usable JSON Schema. Used at task-create
// time to reject a malformed schema before it is ever persisted.
func ValidateSchema(raw json.RawMessage) error {
	_, err := CompileSchema(raw)
	return err
}

// PromptAugmentation returns the system-prompt addendum that instructs the agent
// to emit ONLY a JSON object conforming to schema (#244). An empty schema yields
// the empty string so the caller can append unconditionally.
func PromptAugmentation(schema json.RawMessage) string {
	if len(schema) == 0 {
		return ""
	}
	indented := []byte(schema)
	if b, err := json.MarshalIndent(schema, "", "  "); err == nil {
		indented = b
	}
	return "\n\n--- STRUCTURED OUTPUT REQUIREMENT ---\n" +
		"Your final response MUST be a valid JSON object conforming to the following JSON Schema. " +
		"Do not include any text, markdown fences, or explanation outside the JSON object itself.\n\n" +
		"JSON Schema:\n" + string(indented)
}

// ValidateOutput parses finalText as a single JSON value and validates it against
// schema, returning the compact JSON on success. It tolerates a model that
// wrapped its answer in a ```json fence or surrounded it with prose by extracting
// the outermost JSON object/array before parsing. On any failure it returns an
// error describing what went wrong (not valid JSON / schema violation) so the
// driver can decide whether to retry.
func ValidateOutput(finalText string, schema json.RawMessage) (json.RawMessage, error) {
	sch, err := CompileSchema(schema)
	if err != nil {
		return nil, err
	}
	candidate := extractJSON(finalText)
	var v any
	if err := json.Unmarshal([]byte(candidate), &v); err != nil {
		return nil, fmt.Errorf("final response is not valid JSON: %w", err)
	}
	if err := sch.Validate(v); err != nil {
		return nil, fmt.Errorf("final response does not conform to output_schema: %w", err)
	}
	// Re-marshal so what we persist is compact, canonical JSON regardless of the
	// model's whitespace/fencing.
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("re-marshal validated output: %w", err)
	}
	return out, nil
}

// extractJSON best-effort-isolates the JSON value in a model response: it strips a
// leading/trailing ```json fence and, failing a direct parse, falls back to the
// substring between the first opening and last closing brace/bracket. Returns the
// trimmed input unchanged when no JSON delimiters are present (the caller then
// surfaces a clean "not valid JSON" error).
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		// Drop the opening fence line (```json / ```), then a trailing fence.
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	}
	if json.Valid([]byte(s)) {
		return s
	}
	// Fall back to the outermost object/array span.
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return s
	}
	open := s[start]
	closeByte := byte('}')
	if open == '[' {
		closeByte = ']'
	}
	if end := strings.LastIndexByte(s, closeByte); end > start {
		return s[start : end+1]
	}
	return s
}
