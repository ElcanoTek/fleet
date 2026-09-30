package agentcore

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// jsonMap decodes a JSON literal the way the MCP client decodes a catalog.
func jsonMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("bad fixture %s: %v", raw, err)
	}
	return m
}

func schemaTool(server, name string, schema map[string]any) mcp.ServerTool {
	return mcp.ServerTool{ServerName: server, Tool: mcp.Tool{Name: name, Description: name, InputSchema: schema}}
}

// pagesDateRangeSchema is update_page_data's input schema fragment exactly as
// Pages served it before pages#111 (MCP TypeScript SDK, draft-07): the zod
// tuple became `items: [...]`. Every Claude turn with Pages on failed with
// "tools.N.custom.input_schema: JSON schema is invalid" (2026-09-30).
const pagesDateRangeSchema = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "properties": {
    "slug": {"type": "string"},
    "expect": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "date_range": {
          "type": "object",
          "propertyNames": {"type": "string"},
          "additionalProperties": {
            "type": "array",
            "items": [{"type": "string"}, {"type": "string"}],
            "additionalItems": false,
            "minItems": 2,
            "maxItems": 2
          }
        }
      }
    }
  },
  "required": ["slug"]
}`

func TestNormalizeToolSchemaRewritesOlderDrafts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "closed tuple becomes prefixItems with items false",
			in:   `{"type":"array","items":[{"type":"string"},{"type":"number"}],"additionalItems":false}`,
			want: `{"type":"array","prefixItems":[{"type":"string"},{"type":"number"}],"items":false}`,
		},
		{
			name: "tuple rest schema becomes items",
			in:   `{"type":"array","items":[{"type":"string"}],"additionalItems":{"type":"integer"}}`,
			want: `{"type":"array","prefixItems":[{"type":"string"}],"items":{"type":"integer"}}`,
		},
		{
			name: "open tuple keeps extra elements allowed",
			in:   `{"type":"array","items":[{"type":"string"}]}`,
			want: `{"type":"array","prefixItems":[{"type":"string"}]}`,
		},
		{
			name: "additionalItems beside a single items schema was ignored and is dropped",
			in:   `{"type":"array","items":{"type":"string"},"additionalItems":false}`,
			want: `{"type":"array","items":{"type":"string"}}`,
		},
		{
			name: "boolean exclusive bounds fold into numeric form",
			in:   `{"type":"number","minimum":0,"exclusiveMinimum":true,"maximum":10,"exclusiveMaximum":false}`,
			want: `{"type":"number","exclusiveMinimum":0,"maximum":10}`,
		},
		{
			name: "boolean exclusive bound without a bound is dropped",
			in:   `{"type":"number","exclusiveMaximum":true}`,
			want: `{"type":"number"}`,
		},
		{
			name: "numeric exclusive bound is already 2020-12 and untouched",
			in:   `{"type":"number","minimum":1,"exclusiveMinimum":0}`,
			want: `{"type":"number","minimum":1,"exclusiveMinimum":0}`,
		},
		{
			name: "draft-03 boolean required hoists into the parent list without duplicates",
			in:   `{"type":"object","required":["a"],"properties":{"a":{"type":"string","required":true},"b":{"type":"string","required":true},"c":{"type":"string","required":false}}}`,
			want: `{"type":"object","required":["a","b"],"properties":{"a":{"type":"string"},"b":{"type":"string"},"c":{"type":"string"}}}`,
		},
		{
			name: "unicode property pattern is dropped, an ECMA-262 pattern is kept",
			in:   `{"type":"object","properties":{"name":{"type":"string","pattern":"^\\p{L}+$"},"code":{"type":"string","pattern":"^[A-Z]+$"}}}`,
			want: `{"type":"object","properties":{"name":{"type":"string"},"code":{"type":"string","pattern":"^[A-Z]+$"}}}`,
		},
		{
			name: "rewrites reach nested applicators, $defs, definitions and dependencies",
			in: `{"anyOf":[{"type":"array","items":[{"type":"string"}]}],
			      "$defs":{"pair":{"type":"array","items":[{"type":"string"}],"additionalItems":false}},
			      "definitions":{"n":{"type":"number","minimum":1,"exclusiveMinimum":true}},
			      "dependencies":{"a":["b"],"c":{"properties":{"d":{"type":"string","required":true}}}}}`,
			want: `{"anyOf":[{"type":"array","prefixItems":[{"type":"string"}]}],
			        "$defs":{"pair":{"type":"array","prefixItems":[{"type":"string"}],"items":false}},
			        "definitions":{"n":{"type":"number","exclusiveMinimum":1}},
			        "dependencies":{"a":["b"],"c":{"required":["d"],"properties":{"d":{"type":"string"}}}}}`,
		},
		{
			// Property NAMES and data keywords are not schema positions: a
			// property called "items", "pattern" or "required", and default/
			// examples/enum/const values, pass through byte-for-byte.
			name: "property names and data values are never rewritten",
			in: `{"type":"object","properties":{
			        "items":{"type":"array","items":{"type":"string"}},
			        "pattern":{"type":"string","default":"\\p{L}"},
			        "required":{"type":"boolean"},
			        "shape":{"type":"object","default":{"items":[1,2],"additionalItems":false,"exclusiveMinimum":true,"pattern":"\\p{L}"},
			                 "examples":[{"items":["x"]}],"enum":[{"required":true}],"const":{"minimum":1,"exclusiveMinimum":true}}}}`,
			want: `{"type":"object","properties":{
			        "items":{"type":"array","items":{"type":"string"}},
			        "pattern":{"type":"string","default":"\\p{L}"},
			        "required":{"type":"boolean"},
			        "shape":{"type":"object","default":{"items":[1,2],"additionalItems":false,"exclusiveMinimum":true,"pattern":"\\p{L}"},
			                 "examples":[{"items":["x"]}],"enum":[{"required":true}],"const":{"minimum":1,"exclusiveMinimum":true}}}}`,
		},
		{
			name: "boolean schemas and a valid 2020-12 schema are unchanged",
			in:   `{"type":"object","additionalProperties":false,"properties":{"any":true,"none":false,"list":{"type":"array","prefixItems":[{"type":"string"}],"items":false}}}`,
			want: `{"type":"object","additionalProperties":false,"properties":{"any":true,"none":false,"list":{"type":"array","prefixItems":[{"type":"string"}],"items":false}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := jsonMap(t, tc.in)
			before := jsonMap(t, tc.in)
			got := normalizeToolSchema(in)
			if want := jsonMap(t, tc.want); !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				t.Fatalf("normalizeToolSchema:\n got  %s\n want %s", gotJSON, wantJSON)
			}
			if !reflect.DeepEqual(in, before) {
				t.Fatalf("normalizeToolSchema mutated its input (the shared catalog entry)")
			}
		})
	}
}

// TestNormalizedOlderDraftSchemasPass2020Metaschema: every older-draft input
// above is invalid under 2020-12 as sent today, and valid once normalized. This
// is the property the model boundary needs, independent of the exact rewrite.
func TestNormalizedOlderDraftSchemasPass2020Metaschema(t *testing.T) {
	schemas := map[string]string{
		"pages date_range tuple (pages#111)": pagesDateRangeSchema,
		"boolean exclusive bound":            `{"type":"object","properties":{"n":{"type":"number","minimum":0,"exclusiveMinimum":true}}}`,
		"draft-03 boolean required":          `{"type":"object","properties":{"o":{"type":"object","properties":{"a":{"type":"string","required":true}}}}}`,
	}
	for name, raw := range schemas {
		t.Run(name, func(t *testing.T) {
			tool := schemaTool("srv", "tool", jsonMap(t, raw))

			// As sent before this change: properties copied through unchanged.
			asSentBefore := fantasy.ToolInfo{Parameters: copyJSONValue(tool.Tool.InputSchema["properties"]).(map[string]any)}
			if toolInputSchemaProblem(asSentBefore) == nil {
				t.Fatalf("fixture is already valid under 2020-12; it no longer exercises a rewrite")
			}

			mt := &mcpTool{serverName: tool.ServerName, tool: tool.Tool}
			if problem := toolInputSchemaProblem(mt.Info()); problem != nil {
				t.Fatalf("normalized schema still invalid under 2020-12: %v", problem)
			}
		})
	}
}

func TestToolInputSchemaProblemNamesTheLocation(t *testing.T) {
	valid := &mcpTool{serverName: "srv", tool: mcp.Tool{Name: "ok", InputSchema: jsonMap(t,
		`{"type":"object","properties":{"q":{"type":"string","minLength":1}},"required":["q"]}`)}}
	if problem := toolInputSchemaProblem(valid.Info()); problem != nil {
		t.Fatalf("valid schema reported invalid: %v", problem)
	}
	// "type": 42 has no older-draft meaning to translate: still invalid.
	broken := &mcpTool{serverName: "srv", tool: mcp.Tool{Name: "bad", InputSchema: jsonMap(t,
		`{"type":"object","properties":{"q":{"type":42}}}`)}}
	problem := toolInputSchemaProblem(broken.Info())
	if problem == nil {
		t.Fatal("irreparable schema reported valid")
	}
	if !strings.Contains(problem.Error(), "/properties/q") {
		t.Fatalf("problem %q does not name the offending location", problem)
	}
	// Unresolvable $ref and unknown formats are NOT metaschema violations: the
	// gate must not drop tools the providers accept today on those grounds.
	lenient := &mcpTool{serverName: "srv", tool: mcp.Tool{Name: "ref", InputSchema: jsonMap(t,
		`{"type":"object","properties":{"a":{"$ref":"#/$defs/missing"},"b":{"type":"string","format":"not-a-real-format"}}}`)}}
	if problem := toolInputSchemaProblem(lenient.Info()); problem != nil {
		t.Fatalf("dangling $ref / unknown format treated as invalid: %v", problem)
	}
}

func TestInfoHoistsTopLevelBooleanRequired(t *testing.T) {
	mt := &mcpTool{serverName: "srv", tool: mcp.Tool{Name: "t", InputSchema: jsonMap(t,
		`{"type":"object","required":["a"],"properties":{"a":{"type":"string"},"b":{"type":"string","required":true}}}`)}}
	info := mt.Info()
	if !reflect.DeepEqual(info.Required, []string{"a", "b"}) {
		t.Fatalf("Required = %v, want [a b]", info.Required)
	}
	if _, has := info.Parameters["b"].(map[string]any)["required"]; has {
		t.Fatalf("boolean required left on the property: %v", info.Parameters["b"])
	}
}

// recordingSchemaObserver captures mcp_tool_schema_invalid events.
type recordingSchemaObserver struct{ tools []string }

func (o *recordingSchemaObserver) Observe(event string, payload map[string]any) {
	if event == "mcp_tool_schema_invalid" {
		o.tools = append(o.tools, payload["tool"].(string))
	}
}

// TestGate5SkipsOnlyTheInvalidTool: one irreparable tool is withheld (and
// reported) while its server's other tools, and a repairable older-draft tool,
// still register. Before this gate the bad tool was advertised and Anthropic
// refused every request. Checked in both registration modes.
func TestGate5SkipsOnlyTheInvalidTool(t *testing.T) {
	catalog := []mcp.ServerTool{
		schemaTool("pages", "update_page_data", jsonMap(t, pagesDateRangeSchema)),
		schemaTool("pages", "get_page", jsonMap(t, `{"type":"object","properties":{"slug":{"type":"string"}}}`)),
		schemaTool("pages", "broken", jsonMap(t, `{"type":"object","properties":{"q":{"type":42}}}`)),
		variantTool("other", "ping"),
	}
	for _, mode := range []struct {
		name      string
		threshold string
	}{{"direct", "128"}, {"deferred", "1"}} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", mode.threshold)
			obs := &recordingSchemaObserver{}
			_, roster, err := buildFantasyToolsWithRoster(nil, catalog, &fakeBroker{}, nil, passPolicy{}, nil, nil, toolBuildConfig{observer: obs})
			if err != nil {
				t.Fatalf("buildFantasyToolsWithRoster: %v", err)
			}
			if mode.name == "deferred" && roster.deferredMCP == 0 {
				t.Fatal("threshold did not force deferral; the deferred path is untested")
			}
			for _, want := range []string{"mcp_pages_update_page_data", "mcp_pages_get_page", "mcp_other_ping"} {
				if !slices.Contains(roster.mcp, want) {
					t.Errorf("%s not registered: %v", want, roster.mcp)
				}
			}
			if slices.Contains(roster.mcp, "mcp_pages_broken") {
				t.Errorf("tool with an invalid schema was registered: %v", roster.mcp)
			}
			if !reflect.DeepEqual(obs.tools, []string{"mcp_pages_broken"}) {
				t.Errorf("mcp_tool_schema_invalid events = %v, want [mcp_pages_broken]", obs.tools)
			}
		})
	}
}
