// Package structuredoutput implements the schema side of structured-output mode
// (#244): compiling a task's declared JSON Schema at enqueue and validating each
// terminal, runner, and storage candidate. It is the single source of truth for
// every gate so no lifecycle layer can disagree on what "valid" means.
package structuredoutput

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// schemaResourceURL is the in-memory resource name the compiler keys the schema
// under; it never touches the network or filesystem.
const schemaResourceURL = "fleet://output_schema.json"

// Public task schemas are copied into both a terminal provider request and a
// prompt instruction. Keep all three dimensions bounded before compilation so
// an untrusted schema cannot consume unbounded host memory, provider context,
// or compiler work. These are protocol limits rather than operator knobs: a
// task accepted on one fleet instance must remain runnable on another.
const (
	MaxSchemaBytes = 64 << 10
	MaxSchemaDepth = 32
	MaxSchemaNodes = 2048
)

// CompileSchema validates that raw is a usable draft-07-style JSON Schema object
// and returns the compiled schema. A nil/empty raw is a programming error here
// (callers gate on len first); a non-object or uncompilable schema returns an
// error suitable for surfacing to the task author at create time.
func CompileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty schema")
	}
	if len(raw) > MaxSchemaBytes {
		return nil, fmt.Errorf("schema is %d bytes; maximum is %d bytes", len(raw), MaxSchemaBytes)
	}
	// A JSON Schema must itself be a JSON object — reject arrays/scalars early
	// with a clearer message than the compiler's.
	if !json.Valid(raw) {
		return nil, fmt.Errorf("must be a JSON object: invalid JSON")
	}
	var obj map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&obj); err != nil || obj == nil {
		if err == nil {
			err = fmt.Errorf("null is not an object")
		}
		return nil, fmt.Errorf("must be a JSON object: %w", err)
	}
	if err := validateComplexity(obj, 1, new(int)); err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	// output_schema is UNTRUSTED task input (POST /tasks), and the compiler's
	// default loaders resolve external $refs — including file:// URLs — at
	// compile time, which would let a task author make the fleet HOST process
	// open arbitrary local files (a file-existence oracle and a blocking-open
	// DoS that crosses the mandatory-sandbox boundary, #585). A self-contained
	// draft-07 schema never needs an external ref, so refuse every URL load;
	// internal "#/..." refs resolve against the AddResource'd document and
	// never reach this loader.
	c.LoadURL = func(url string) (io.ReadCloser, error) {
		return nil, fmt.Errorf("external $ref %q is not allowed: output_schema must be self-contained", url)
	}
	if err := c.AddResource(schemaResourceURL, bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	sch, err := c.Compile(schemaResourceURL)
	if err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	return sch, nil
}

// validateComplexity counts every JSON value and tracks container nesting. The
// byte ceiling runs first, so even a schema made mostly of scalar leaves stays
// bounded while this walk executes.
func validateComplexity(v any, depth int, nodes *int) error {
	(*nodes)++
	if *nodes > MaxSchemaNodes {
		return fmt.Errorf("schema has more than %d nodes", MaxSchemaNodes)
	}
	if depth > MaxSchemaDepth {
		return fmt.Errorf("schema nesting exceeds maximum depth of %d", MaxSchemaDepth)
	}
	switch x := v.(type) {
	case map[string]any:
		for _, child := range x {
			if err := validateComplexity(child, depth+1, nodes); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range x {
			if err := validateComplexity(child, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateSchema reports whether raw is a usable JSON Schema. Used at task-create
// time to reject a malformed schema before it is ever persisted.
func ValidateSchema(raw json.RawMessage) error {
	_, err := CompileSchema(raw)
	return err
}

// PromptAugmentation returns the system-prompt addendum that instructs the agent
// to emit ONLY a JSON value conforming to schema (#244). An empty schema yields
// the empty string so the caller can append unconditionally.
func PromptAugmentation(schema json.RawMessage) string {
	if len(schema) == 0 {
		return ""
	}
	// Do not pretty-print an untrusted schema: indentation grows with both node
	// count and depth and would make its prompt contribution much larger than
	// the enqueue-time byte limit. Compact preserves the exact semantics while
	// keeping this logical copy at or below MaxSchemaBytes.
	compact := &bytes.Buffer{}
	if err := json.Compact(compact, schema); err != nil {
		return ""
	}
	return "\n\n--- STRUCTURED OUTPUT REQUIREMENT ---\n" +
		"Your final response MUST be a valid JSON value conforming to the following JSON Schema. " +
		"Do not include any text, markdown fences, or explanation outside the JSON value itself.\n\n" +
		"JSON Schema:\n" + compact.String()
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
		decoder := json.NewDecoder(strings.NewReader(candidates[i]))
		// Keep JSON numbers exact through validation and persistence. Decoding
		// through float64 would silently round integers above 2^53, allowing the
		// stored value to differ from the provider's schema-valid candidate.
		decoder.UseNumber()
		if err := decoder.Decode(&v); err != nil {
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
