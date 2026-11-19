// A gated MCP call whose arguments the server is certain to reject is refused
// before an approval card exists: the model gets the error at once and can
// call again, and nobody is asked to approve a call that fails after they
// click Approve. Same idea as prevalidateStagedTaskPrompt (#1601), for MCP
// tools.
//
// The check is deliberately narrow. It reads only a top-level string argument
// against the fixed set of values the tool's own input schema allows for it —
// the `action` of a multiplexed tool is the case this exists for. Anything the
// schema does not pin to a closed set of strings is left to the server: a
// false refusal here would block a legitimate call outright, which is worse
// than a card for a call that then fails.

package httpapi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// maxListedAllowedValues bounds how many allowed values a refusal names.
const maxListedAllowedValues = 40

// prevalidateStagedMCPArguments refuses a cataloged MCP tool call that gives a
// top-level string argument a value outside the closed set its input schema
// declares. A tool that is not in the catalog, arguments that do not parse,
// and every argument the schema leaves open pass.
func prevalidateStagedMCPArguments(catalog []mcp.ServerTool, toolName, rawInput string) error {
	server, tool, err := resolveMCPTool(catalog, toolName)
	if err != nil {
		return nil //nolint:nilerr // not a cataloged MCP tool: nothing to check against
	}
	schema := catalogInputSchema(catalog, server, tool)
	if schema == nil {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return nil //nolint:nilerr // intentional: unparseable JSON is handled by the card and executor
	}
	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	sort.Strings(names)
	var problems []string
	for _, name := range names {
		value, isString := args[name].(string)
		if !isString {
			continue
		}
		allowed, closed := allowedStringValues(schema, name)
		if !closed || containsString(allowed, value) {
			continue
		}
		problems = append(problems, fmt.Sprintf("%q is not a valid %s for %s (expected one of: %s)", value, name, toolName, listAllowedValues(allowed)))
	}
	if len(problems) == 0 {
		return nil
	}
	return &agentcore.StageRefusedError{Reason: fmt.Sprintf("%s. Nothing was staged for approval and nothing ran: fix the arguments and call %s again", strings.Join(problems, "; "), toolName)}
}

// allowedStringValues returns the closed set of strings a tool's input schema
// allows for the top-level argument name. closed is false when the schema does
// not pin the argument to such a set: no declaration, a non-string or open
// alternative, or a construct this reader does not follow (`$ref`, `allOf`).
//
// Two shapes are read: the argument's own declaration, and a root `oneOf` /
// `anyOf` whose every branch declares it (a tool discriminated by `action`).
func allowedStringValues(schema map[string]any, name string) (allowed []string, closed bool) {
	if decl, ok := propertySchema(schema, name); ok {
		return closedStringSet(decl)
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		branches, ok := schema[key].([]any)
		if !ok || len(branches) == 0 {
			continue
		}
		for _, b := range branches {
			branch, ok := b.(map[string]any)
			if !ok {
				return nil, false
			}
			decl, ok := propertySchema(branch, name)
			if !ok {
				return nil, false
			}
			values, ok := closedStringSet(decl)
			if !ok {
				return nil, false
			}
			allowed = append(allowed, values...)
		}
		return allowed, len(allowed) > 0
	}
	return nil, false
}

// propertySchema returns schema.properties[name] when it is an object.
func propertySchema(schema map[string]any, name string) (map[string]any, bool) {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil, false
	}
	decl, ok := props[name].(map[string]any)
	return decl, ok
}

// closedStringSet reads a declaration that allows only fixed strings: `const`,
// `enum`, or a `oneOf` / `anyOf` of such declarations.
func closedStringSet(decl map[string]any) ([]string, bool) {
	if _, hasRef := decl["$ref"]; hasRef {
		return nil, false
	}
	if _, hasAllOf := decl["allOf"]; hasAllOf {
		return nil, false
	}
	if c, ok := decl["const"]; ok {
		s, isString := c.(string)
		return []string{s}, isString
	}
	if e, ok := decl["enum"]; ok {
		values, isList := e.([]any)
		if !isList || len(values) == 0 {
			return nil, false
		}
		out := make([]string, 0, len(values))
		for _, v := range values {
			s, isString := v.(string)
			if !isString {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		alternatives, ok := decl[key].([]any)
		if !ok || len(alternatives) == 0 {
			continue
		}
		var out []string
		for _, a := range alternatives {
			alt, ok := a.(map[string]any)
			if !ok {
				return nil, false
			}
			values, ok := closedStringSet(alt)
			if !ok {
				return nil, false
			}
			out = append(out, values...)
		}
		return out, true
	}
	return nil, false
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// listAllowedValues renders the allowed values for a refusal, bounded.
func listAllowedValues(values []string) string {
	seen := make(map[string]struct{}, len(values))
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	if len(quoted) > maxListedAllowedValues {
		return strings.Join(quoted[:maxListedAllowedValues], ", ") + fmt.Sprintf(", and %d more", len(quoted)-maxListedAllowedValues)
	}
	return strings.Join(quoted, ", ")
}
