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

// ValidateOutput finds the JSON value in finalText that conforms to schema and
// returns it as compact JSON. It tolerates a model that wrapped its answer in a
// ```json fence, surrounded it with prose, or emitted SEVERAL JSON values (a
// narrated intermediate plus a restated final answer — observed live): every
// complete top-level JSON value in the text is a candidate, and the LAST one
// that validates wins, since a model restating its answer states it last. On
// failure the error says what went wrong (no JSON at all / none conforming) so
// the driver can decide whether to retry.
func ValidateOutput(finalText string, schema json.RawMessage) (json.RawMessage, error) {
	sch, err := CompileSchema(schema)
	if err != nil {
		return nil, err
	}
	candidates := extractJSONCandidates(finalText)
	parsedAny := false
	var lastValidationErr error
	for i := len(candidates) - 1; i >= 0; i-- {
		var v any
		if err := json.Unmarshal([]byte(candidates[i]), &v); err != nil {
			continue
		}
		parsedAny = true
		if err := sch.Validate(v); err != nil {
			lastValidationErr = err
			continue
		}
		// Re-marshal so what we persist is compact, canonical JSON regardless
		// of the model's whitespace/fencing.
		out, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("re-marshal validated output: %w", err)
		}
		return out, nil
	}
	if !parsedAny {
		return nil, fmt.Errorf("final response is not valid JSON: no parseable JSON value found")
	}
	return nil, fmt.Errorf("final response does not conform to output_schema: %w", lastValidationErr)
}

// extractJSONCandidates isolates every complete top-level JSON value in a model
// response, in order of appearance. Fences are treated as plain surrounding
// text (the scanner skips to the next JSON delimiter anyway). A whole-string
// valid JSON short-circuits to a single candidate. Scanning uses json.Decoder
// from each opening delimiter so nested braces inside strings can't confuse
// extraction the way the old first-{/last-} span did.
func extractJSONCandidates(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if json.Valid([]byte(s)) {
		return []string{s}
	}
	var out []string
	for i := 0; i < len(s); {
		j := strings.IndexAny(s[i:], "{[")
		if j < 0 {
			break
		}
		start := i + j
		dec := json.NewDecoder(strings.NewReader(s[start:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			i = start + 1 // not a value here — advance past this delimiter
			continue
		}
		out = append(out, string(raw))
		i = start + int(dec.InputOffset())
	}
	return out
}
