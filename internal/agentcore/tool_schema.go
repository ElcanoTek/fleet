package agentcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"charm.land/fantasy"
	fantasyschema "charm.land/fantasy/schema"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// MCP tool input schemas reach the model rebuilt as {type, properties,
// required} (mcpTool.Info), so the server's `$schema` marker never travels.
// Anthropic then validates every tool as JSON Schema draft 2020-12 and rejects
// the WHOLE request when one tool fails ("tools.N.custom.input_schema: JSON
// schema is invalid"), which fails every turn in any conversation that has the
// offending connector on. OpenAI and Gemini accept the older forms, so the
// failure is Claude-only and looks model-specific. Two layers:
//
//  1. normalizeToolSchema rewrites the older-draft constructs that are INVALID
//     under 2020-12 into their 2020-12 equivalents, preserving what the tool
//     accepts. The MCP TypeScript SDK emits draft-07, where a zod tuple becomes
//     `items: [...]` (Pages, 2026-09-30).
//  2. toolInputSchemaProblem validates the final schema against the 2020-12
//     metaschema, so buildFantasyToolsWithRoster can skip a tool that is still
//     invalid instead of letting it take every other tool down with it.

// Keywords whose value is a schema, an array of schemas, or a map of name ->
// schema. Only these positions are schemas: property NAMES and the values of
// annotation/validation keywords (default, examples, const, enum, required)
// are data and are copied verbatim, so a property literally named "items" or a
// default object with a "pattern" key is never rewritten.
var (
	schemaValuedKeywords = map[string]bool{
		"items": true, "additionalItems": true, "additionalProperties": true,
		"contains": true, "propertyNames": true, "not": true,
		"if": true, "then": true, "else": true,
		"unevaluatedItems": true, "unevaluatedProperties": true, "contentSchema": true,
	}
	schemaArrayKeywords = map[string]bool{
		"allOf": true, "anyOf": true, "oneOf": true, "prefixItems": true,
	}
	schemaMapKeywords = map[string]bool{
		"properties": true, "patternProperties": true, "$defs": true,
		"definitions": true, "dependentSchemas": true,
		// draft-07 `dependencies`: each value is a schema or a string array.
		// normalizeToolSchema passes non-schema values through unchanged.
		"dependencies": true,
	}
)

// normalizeToolSchema returns a deep copy of an MCP tool's input schema with
// older-draft constructs rewritten for draft 2020-12:
//
//   - positional `items: [a, b]` becomes `prefixItems: [a, b]`, and
//     `additionalItems: X` becomes `items: X` (without additionalItems, extra
//     elements stay allowed, exactly as in draft-07). `additionalItems` next
//     to a non-array `items` was ignored by draft-07 and is dropped.
//   - draft-04 boolean `exclusiveMinimum`/`exclusiveMaximum` fold into the
//     numeric 2020-12 form (`minimum: 5, exclusiveMinimum: true` becomes
//     `exclusiveMinimum: 5`); `false` is dropped.
//   - draft-03 boolean `required` on a property is hoisted into the parent's
//     `required` array.
//   - `pattern` values using `\p{…}` Unicode property escapes are dropped:
//     OpenAI's function-calling validator accepts ECMA-262 only.
func normalizeToolSchema(schema map[string]any) map[string]any {
	out, _ := normalizeSchemaNode(schema).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func normalizeSchemaNode(node any) any {
	obj, ok := node.(map[string]any)
	if !ok {
		// A boolean schema, or a malformed value the validator will judge.
		return copyJSONValue(node)
	}
	out := make(map[string]any, len(obj))
	var hoisted []string
	for key, value := range obj {
		switch {
		case key == "pattern":
			if s, ok := value.(string); ok && strings.Contains(s, `\p{`) {
				continue
			}
			out[key] = value
		case key == "required":
			if _, isBool := value.(bool); isBool {
				continue // draft-03 form; the parent hoists it
			}
			out[key] = copyJSONValue(value)
		case schemaValuedKeywords[key]:
			if items, isArray := value.([]any); isArray && key == "items" {
				out[key] = normalizeSchemaArray(items) // rewritten below
				continue
			}
			out[key] = normalizeSchemaNode(value)
		case schemaArrayKeywords[key]:
			if items, isArray := value.([]any); isArray {
				out[key] = normalizeSchemaArray(items)
			} else {
				out[key] = copyJSONValue(value)
			}
		case schemaMapKeywords[key]:
			members, isMap := value.(map[string]any)
			if !isMap {
				out[key] = copyJSONValue(value)
				continue
			}
			normalized := make(map[string]any, len(members))
			for name, member := range members {
				if key == "properties" {
					if child, ok := member.(map[string]any); ok && child["required"] == true {
						hoisted = append(hoisted, name)
					}
				}
				normalized[name] = normalizeSchemaNode(member)
			}
			out[key] = normalized
		default:
			out[key] = copyJSONValue(value)
		}
	}

	if tuple, isArray := out["items"].([]any); isArray {
		out["prefixItems"] = tuple
		delete(out, "items")
		if rest, has := out["additionalItems"]; has {
			out["items"] = rest
		}
	}
	delete(out, "additionalItems")

	foldExclusiveBound(out, "exclusiveMinimum", "minimum")
	foldExclusiveBound(out, "exclusiveMaximum", "maximum")

	if len(hoisted) > 0 {
		out["required"] = mergeRequired(out["required"], hoisted)
	}
	return out
}

func normalizeSchemaArray(items []any) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = normalizeSchemaNode(item)
	}
	return out
}

// foldExclusiveBound rewrites the draft-04 boolean exclusive bound. A numeric
// value is already the 2020-12 form and is left alone.
func foldExclusiveBound(node map[string]any, exclusiveKey, boundKey string) {
	flag, isBool := node[exclusiveKey].(bool)
	if !isBool {
		return
	}
	delete(node, exclusiveKey)
	if bound, has := node[boundKey]; flag && has {
		node[exclusiveKey] = bound
		delete(node, boundKey)
	}
}

func mergeRequired(existing any, hoisted []string) []any {
	merged := []any{}
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			merged = append(merged, name)
		}
	}
	switch names := existing.(type) {
	case []any:
		for _, v := range names {
			if s, ok := v.(string); ok {
				add(s)
			}
		}
	case []string:
		for _, s := range names {
			add(s)
		}
	}
	for _, name := range hoisted {
		add(name)
	}
	return merged
}

// copyJSONValue deep-copies a decoded JSON value so a normalized schema never
// aliases the catalog entry it came from.
func copyJSONValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = copyJSONValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = copyJSONValue(vv)
		}
		return out
	case []string:
		return append([]string(nil), t...)
	default:
		return v
	}
}

var draft2020Metaschema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	return c.Compile("https://json-schema.org/draft/2020-12/schema")
})

// toolInputSchemaProblem reports why a tool's input schema, in the exact shape
// fantasy sends (the {type, properties, required} object after
// fantasyschema.Normalize), is not valid JSON Schema draft 2020-12. nil means
// valid. It checks the schema against the metaschema only: it does not resolve
// $ref or assert formats, so it never refuses a schema the providers accept
// today on those grounds.
func toolInputSchemaProblem(info fantasy.ToolInfo) error {
	meta, err := draft2020Metaschema()
	if err != nil {
		// Fail open: an unavailable validator must never remove tools.
		log.Printf("tool schema check disabled: compile draft 2020-12 metaschema: %v", err)
		return nil
	}
	required := info.Required
	if required == nil {
		required = []string{}
	}
	doc := map[string]any{
		"type":       "object",
		"properties": copyJSONValue(info.Parameters),
		"required":   required,
	}
	fantasyschema.Normalize(doc)
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode input schema: %w", err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("decode input schema: %w", err)
	}
	if err := meta.Validate(decoded); err != nil {
		return schemaValidationSummary(err)
	}
	return nil
}

// schemaValidationSummary reduces the validator's error tree to its first leaf
// ("at /properties/x/items: expected object or boolean"), which names the
// offending location; the full tree is noise in a log line.
func schemaValidationSummary(err error) error {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err
	}
	for len(ve.Causes) > 0 {
		ve = ve.Causes[0]
	}
	location := ve.InstanceLocation
	if location == "" {
		location = "/"
	}
	return fmt.Errorf("at %s: %s", location, ve.Message)
}

// emitToolSchemaInvalid reports a tool skipped by the schema gate, mirroring
// persona_tool_blocked.
func emitToolSchemaInvalid(obs Observer, tool, reason string) {
	if obs == nil {
		return
	}
	obs.Observe("mcp_tool_schema_invalid", map[string]any{
		"tool":   tool,
		"reason": reason,
	})
}
