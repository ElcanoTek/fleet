package agentcore

import (
	"context"
	"slices"

	"charm.land/fantasy"
)

// geminiSchemaModel hands Gemini only tool schemas it accepts. fantasy's
// Gemini conversion reads a schema's types, properties, array items (when
// items is a schema object) and top-level required list, and the Gemini API
// refuses the whole request with a 400 for an array without items ("items:
// missing field") or a required name with no property ("property is not
// defined"). Every tool goes out on every step, so one connected MCP tool
// declaring a bare {"type":"array"} would otherwise fail every turn on a
// google model.
type geminiSchemaModel struct{ fantasy.LanguageModel }

func (m geminiSchemaModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return m.LanguageModel.Generate(ctx, geminiCallTools(call))
}

func (m geminiSchemaModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	return m.LanguageModel.Stream(ctx, geminiCallTools(call))
}

// geminiCallTools returns call with each function tool's schema made
// acceptable to Gemini (geminiToolSchema). Only the tools that change are
// copied; the caller's tools and schemas are never modified.
func geminiCallTools(call fantasy.Call) fantasy.Call {
	var tools []fantasy.Tool
	for i, tool := range call.Tools {
		ft, ok := tool.(fantasy.FunctionTool)
		if !ok {
			continue
		}
		fixed, changed := geminiToolSchema(ft.InputSchema)
		if !changed {
			continue
		}
		if tools == nil {
			tools = slices.Clone(call.Tools)
		}
		ft.InputSchema = fixed
		tools[i] = ft
	}
	if tools != nil {
		call.Tools = tools
	}
	return call
}

// geminiToolSchema gives each array that fantasy would send without items
// (no items, items: true, or a tuple rewritten to prefixItems) string items,
// the type fantasy itself falls back to for what it cannot read, and drops the
// top-level required names that have no property. It returns schema itself
// when nothing needs changing, else a changed copy.
func geminiToolSchema(schema map[string]any) (map[string]any, bool) {
	props, _ := schema["properties"].(map[string]any)
	required, undeclared := geminiDeclaredRequired(schema, props)
	if !undeclared && !geminiItemlessArrays(props, false) {
		return schema, false
	}
	fixed, _ := copyJSONValue(schema).(map[string]any)
	fixedProps, _ := fixed["properties"].(map[string]any)
	geminiItemlessArrays(fixedProps, true)
	if undeclared {
		if len(required) == 0 {
			delete(fixed, "required")
		} else {
			fixed["required"] = required
		}
	}
	return fixed, true
}

// geminiDeclaredRequired returns the top-level required names that name a
// property, and whether any did not. fantasy reads the list only as a
// []string; any other form never reaches Gemini.
func geminiDeclaredRequired(schema, props map[string]any) ([]string, bool) {
	required, ok := schema["required"].([]string)
	if !ok {
		return nil, false
	}
	declared := make([]string, 0, len(required))
	for _, name := range required {
		if _, ok := props[name]; ok {
			declared = append(declared, name)
		}
	}
	return declared, len(declared) != len(required)
}

// geminiItemlessArrays reports whether any of props, or a schema nested in
// them the way fantasy reads one (an object's properties, an array's items),
// is an array without a schema object for items. With fix set it gives each
// such array string items in place.
func geminiItemlessArrays(props map[string]any, fix bool) bool {
	found := false
	for _, p := range props {
		if geminiItemlessNode(p, fix) {
			if !fix {
				return true
			}
			found = true
		}
	}
	return found
}

// geminiItemlessNode is geminiItemlessArrays for one schema node.
func geminiItemlessNode(p any, fix bool) bool {
	node, ok := p.(map[string]any)
	if !ok {
		return false
	}
	switch node["type"] {
	case "array":
		if items, ok := node["items"].(map[string]any); ok {
			return geminiItemlessNode(items, fix)
		}
		if fix {
			node["items"] = map[string]any{"type": "string"}
		}
		return true
	case "object":
		nested, _ := node["properties"].(map[string]any)
		return geminiItemlessArrays(nested, fix)
	}
	return false
}
