package agentcore

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/metrics"
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

// TestNormalizeToolSchemaNotesNameEachRewrite: every rewrite is reported with
// the JSON Pointer of the schema it changed, so an operator can tell the
// connector's authors exactly what to update. The `\p{` strip (an OpenAI
// compatibility step) and an already-valid schema produce no notes.
func TestNormalizeToolSchemaNotesNameEachRewrite(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "pages date_range tuple (pages#111)",
			in:   pagesDateRangeSchema,
			want: []string{"/properties/expect/properties/date_range/additionalProperties: positional items array rewritten to prefixItems (additionalItems moved to items)"},
		},
		{
			name: "exclusive bounds, hoisted required, ignored additionalItems",
			in: `{"type":"object","properties":{
			        "n":{"type":"number","minimum":0,"exclusiveMinimum":true,"maximum":9,"exclusiveMaximum":false},
			        "a/b":{"type":"string","required":true},
			        "l":{"type":"array","items":{"type":"string"},"additionalItems":false}}}`,
			want: []string{
				"/properties/a~1b: boolean required moved into the parent's required list",
				"/properties/l: additionalItems beside a single items schema dropped (draft-07 ignored it)",
				"/properties/n: boolean exclusiveMaximum rewritten to the numeric form",
				"/properties/n: boolean exclusiveMinimum rewritten to the numeric form",
			},
		},
		{
			name: "unicode pattern strip and valid schema are silent",
			in:   `{"type":"object","properties":{"s":{"type":"string","pattern":"^\\p{L}+$"},"p":{"type":"array","prefixItems":[{"type":"string"}]}}}`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, notes := normalizeToolSchemaWithNotes(jsonMap(t, tc.in))
			slices.Sort(notes)
			if !reflect.DeepEqual(notes, tc.want) {
				t.Fatalf("notes:\n got  %q\n want %q", notes, tc.want)
			}
		})
	}
}

func TestCheckMCPToolSchemaClassifies(t *testing.T) {
	if issue, ok := CheckMCPToolSchema("srv", mcp.Tool{Name: "clean", InputSchema: jsonMap(t, `{"type":"object","properties":{"q":{"type":"string"}}}`)}); ok {
		t.Fatalf("clean schema reported: %+v", issue)
	}
	issue, ok := CheckMCPToolSchema("pages", mcp.Tool{Name: "update_page_data", InputSchema: jsonMap(t, pagesDateRangeSchema)})
	if !ok || issue.Status != ToolSchemaRewritten || issue.Server != "pages" || issue.Tool != "update_page_data" ||
		!strings.Contains(issue.Detail, "/properties/expect/properties/date_range/additionalProperties") {
		t.Fatalf("pages tuple = %+v, %v; want rewritten naming the location", issue, ok)
	}
	issue, ok = CheckMCPToolSchema("srv", mcp.Tool{Name: "broken", InputSchema: jsonMap(t, `{"type":"object","properties":{"q":{"type":42}}}`)})
	if !ok || issue.Status != ToolSchemaInvalid || !strings.Contains(issue.Detail, "/properties/q") {
		t.Fatalf("broken = %+v, %v; want invalid naming /properties/q", issue, ok)
	}
}

// TestToolSchemaRecordFollowsTurns: the runtime record `fleet mcp
// schema-issues` and `fleet doctor` read. Each turn refreshes it; the same
// finding on a later turn is not re-counted (the metric moves once per new
// finding); a connector that fixes its schema clears on its next turn.
func TestToolSchemaRecordFollowsTurns(t *testing.T) {
	resetToolSchemaRecordForTest()
	t.Cleanup(resetToolSchemaRecordForTest)
	build := func(catalog []mcp.ServerTool) {
		t.Helper()
		if _, err := buildFantasyTools(nil, catalog, &fakeBroker{}, nil, passPolicy{}, nil, nil, toolBuildConfig{}); err != nil {
			t.Fatalf("buildFantasyTools: %v", err)
		}
	}
	broken := schemaTool("recsrv_b", "broken", jsonMap(t, `{"type":"object","properties":{"q":{"type":42}}}`))
	tuple := schemaTool("recsrv_a", "update_page_data", jsonMap(t, pagesDateRangeSchema))
	clean := schemaTool("recsrv_a", "get_page", jsonMap(t, `{"type":"object","properties":{"slug":{"type":"string"}}}`))

	build([]mcp.ServerTool{broken, tuple, clean})
	build([]mcp.ServerTool{broken, tuple, clean}) // a second turn: same findings

	issues := ToolSchemaIssues()
	if len(issues) != 2 {
		t.Fatalf("record = %+v, want 2 findings", issues)
	}
	if issues[0].Server != "recsrv_a" || issues[0].Status != ToolSchemaRewritten || issues[1].Server != "recsrv_b" || issues[1].Status != ToolSchemaInvalid {
		t.Fatalf("record not sorted by server/tool with the right statuses: %+v", issues)
	}
	if issues[0].LastSeen.IsZero() {
		t.Fatal("LastSeen not set")
	}
	rendered := metrics.Render()
	for _, want := range []string{
		`fleet_mcp_tool_schema_issues_total{server="recsrv_a",status="rewritten"} 1`,
		`fleet_mcp_tool_schema_issues_total{server="recsrv_b",status="invalid"} 1`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("metrics missing %q (a repeat turn must not re-count)", want)
		}
	}

	// The connector ships the fixed schema: its entry clears on the next turn.
	fixed := schemaTool("recsrv_a", "update_page_data", jsonMap(t,
		`{"type":"object","properties":{"expect":{"type":"object","properties":{"date_range":{"type":"object","additionalProperties":{"type":"array","items":{"type":"string"},"minItems":2,"maxItems":2}}}}}}`))
	build([]mcp.ServerTool{broken, fixed, clean})
	issues = ToolSchemaIssues()
	if len(issues) != 1 || issues[0].Tool != "broken" {
		t.Fatalf("after the fix, record = %+v, want only the still-broken tool", issues)
	}
	if ToolSchemaRecordSince().IsZero() {
		t.Fatal("ToolSchemaRecordSince is zero")
	}
}

// TestCheckMCPToolSchemaCacheKeepsEachToolsIdentity: the verdict cache is
// keyed by schema content, so two tools with the same schema share one entry,
// but each finding still names its own server and tool.
func TestCheckMCPToolSchemaCacheKeepsEachToolsIdentity(t *testing.T) {
	schema := `{"type":"object","properties":{"q":{"type":"string","minimum":0,"exclusiveMinimum":true},"cache_probe_unique":{"type":"string"}}}`
	toolSchemaVerdicts.Lock()
	before := len(toolSchemaVerdicts.m)
	toolSchemaVerdicts.Unlock()
	a, okA := CheckMCPToolSchema("srv_one", mcp.Tool{Name: "alpha", InputSchema: jsonMap(t, schema)})
	b, okB := CheckMCPToolSchema("srv_two", mcp.Tool{Name: "beta", InputSchema: jsonMap(t, schema)})
	toolSchemaVerdicts.Lock()
	added := len(toolSchemaVerdicts.m) - before
	toolSchemaVerdicts.Unlock()
	if !okA || !okB || a.Status != ToolSchemaRewritten || b.Status != ToolSchemaRewritten {
		t.Fatalf("findings = %+v %v / %+v %v", a, okA, b, okB)
	}
	if a.Server != "srv_one" || a.Tool != "alpha" || b.Server != "srv_two" || b.Tool != "beta" {
		t.Fatalf("cached verdict leaked identity: %+v / %+v", a, b)
	}
	if a.Detail != b.Detail {
		t.Fatalf("same schema, different detail: %q vs %q", a.Detail, b.Detail)
	}
	if added != 1 {
		t.Fatalf("identical schemas added %d cache entries, want 1", added)
	}
}

// TestToolSchemaRecordConcurrentTurns: concurrent turns share the verdict
// cache and the runtime record; run under -race (CI's go-race job).
func TestToolSchemaRecordConcurrentTurns(t *testing.T) {
	resetToolSchemaRecordForTest()
	t.Cleanup(resetToolSchemaRecordForTest)
	catalog := []mcp.ServerTool{
		schemaTool("concsrv", "tuple", jsonMap(t, pagesDateRangeSchema)),
		schemaTool("concsrv", "broken", jsonMap(t, `{"type":"object","properties":{"q":{"type":42}}}`)),
		schemaTool("concsrv", "clean", jsonMap(t, `{"type":"object","properties":{"s":{"type":"string"}}}`)),
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if _, err := buildFantasyTools(nil, catalog, &fakeBroker{}, nil, passPolicy{}, nil, nil, toolBuildConfig{}); err != nil {
					t.Errorf("buildFantasyTools: %v", err)
				}
				_ = ToolSchemaIssues()
			}
		}()
	}
	wg.Wait()
	if got := ToolSchemaIssues(); len(got) != 2 {
		t.Fatalf("record after concurrent turns = %+v, want tuple + broken", got)
	}
}

// TestInfoInlinesTopLevelDefsForFastMCPSchemas reproduces the Raptive
// ix_publisher_mcp failure on Gemini (2026-11-03): FastMCP / Pydantic emits
// top-level `$defs` and `"$ref": "#/$defs/ReportFilter"` under
// `properties.filters.anyOf.0.items`. Because mcpTool.Info() extracts only
// `{properties, required}`, leaving `$ref` un-inlined produced a dangling
// reference that Gemini rejected with 400 INVALID_ARGUMENT ("reference to
// undefined schema at properties.filters.anyOf.0.items") and hid the nested
// object fields from tool_describe.
func TestInfoInlinesTopLevelDefsForFastMCPSchemas(t *testing.T) {
	const ixRunReportSchema = `{
	  "$defs": {
	    "ReportFilter": {
	      "description": "One report filter; all filters must match (AND).",
	      "properties": {
	        "field": {"type": "string"},
	        "operator": {"type": "string", "enum": ["=", "!=", "isAnyOf"]},
	        "values": {"type": "array", "items": {"type": "string"}}
	      },
	      "required": ["field", "operator", "values"],
	      "title": "ReportFilter",
	      "type": "object"
	    }
	  },
	  "properties": {
	    "filters": {
	      "anyOf": [
	        {"type": "array", "items": {"$ref": "#/$defs/ReportFilter"}},
	        {"type": "null"}
	      ],
	      "default": null,
	      "title": "Filters"
	    }
	  },
	  "type": "object"
	}`

	raw := jsonMap(t, ixRunReportSchema)
	before := jsonMap(t, ixRunReportSchema)
	mt := &mcpTool{serverName: "ix_publisher_mcp", tool: mcp.Tool{Name: "ixpub_run_report", Description: "Run report", InputSchema: raw}}

	info := mt.Info()
	encoded, err := json.Marshal(info.Parameters)
	if err != nil {
		t.Fatalf("marshal Parameters: %v", err)
	}
	if strings.Contains(string(encoded), `"$ref"`) {
		t.Fatalf("mcpTool.Info().Parameters still contains a dangling $ref: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"operator"`) || !strings.Contains(string(encoded), `"isAnyOf"`) {
		t.Fatalf("inlined ReportFilter fields missing from Parameters: %s", encoded)
	}
	if !reflect.DeepEqual(raw, before) {
		t.Fatal("mcpTool.Info() mutated the catalog InputSchema")
	}
	if issue, ok := CheckMCPToolSchema("ix_publisher_mcp", mt.tool); ok {
		t.Fatalf("valid 2020-12 $defs schema reported as schema issue: %+v", issue)
	}
}

func TestNormalizeToolSchemaInlinesNestedAndCyclicDefs(t *testing.T) {
	const nestedAndCyclic = `{
	  "$defs": {
	    "Leaf~Name/1": {
	      "type": "object",
	      "properties": {"v": {"type": "number", "minimum": 0, "exclusiveMinimum": true}},
	      "required": ["v"]
	    },
	    "Wrapper": {
	      "type": "object",
	      "description": "generic wrapper",
	      "properties": {"leaf": {"$ref": "#/$defs/Leaf~0Name~11"}}
	    },
	    "Node": {
	      "type": "object",
	      "title": "Node",
	      "properties": {
	        "name": {"type": "string"},
	        "children": {"type": "array", "items": {"$ref": "#/$defs/Node"}}
	      }
	    },
	    "Always": true,
	    "Never": false,
	    "LongString": {"type": "string", "minLength": 10},
	    "RootOnly": {"type": "string"}
	  },
	  "definitions": {
	    "LegacyTag": {"type": "string", "minLength": 1}
	  },
	  "type": "object",
	  "properties": {
	    "wrapped": {"$ref": "#/$defs/Wrapper", "description": "custom override"},
	    "tag": {"$ref": "#/definitions/LegacyTag", "maxLength": 20},
	    "conflicting": {"$ref": "#/$defs/LongString", "minLength": 1},
	    "always_ok": {"$ref": "#/$defs/Always"},
	    "never_ok": {"$ref": "#/$defs/Never", "description": "ignored on false"},
	    "tree": {"$ref": "#/$defs/Node"},
	    "nested_resource": {
	      "$id": "https://example.com/sub.json",
	      "$defs": {
	        "LongString": {"type": "integer"}
	      },
	      "type": "object",
	      "properties": {
	        "local_def": {"$ref": "#/$defs/LongString"},
	        "outer_def": {"$ref": "#/$defs/RootOnly"}
	      }
	    },
	    "dangling": {"$ref": "#/$defs/DoesNotExist"}
	  }
	}`

	got := normalizeToolSchema(jsonMap(t, nestedAndCyclic))
	props := got["properties"].(map[string]any)

	wrapped := props["wrapped"].(map[string]any)
	if wrapped["description"] != "custom override" || wrapped["type"] != "object" {
		t.Fatalf("wrapped sibling override or inlined type lost: %v", wrapped)
	}
	leaf := wrapped["properties"].(map[string]any)["leaf"].(map[string]any)
	leafV := leaf["properties"].(map[string]any)["v"].(map[string]any)
	if leafV["exclusiveMinimum"] != float64(0) {
		t.Fatalf("older-draft rewrite inside nested $defs not preserved: %v", leafV)
	}

	tag := props["tag"].(map[string]any)
	if tag["type"] != "string" || tag["minLength"] != float64(1) || tag["maxLength"] != float64(20) {
		t.Fatalf("definitions $ref or non-conflicting sibling not inlined: %v", tag)
	}

	conflicting := props["conflicting"].(map[string]any)
	if conflicting["minLength"] != float64(10) {
		t.Fatalf("conflicting sibling overwrote referenced minLength=10: %v", conflicting)
	}
	allOf, ok := conflicting["allOf"].([]any)
	if !ok || len(allOf) != 1 || allOf[0].(map[string]any)["minLength"] != float64(1) {
		t.Fatalf("conflicting sibling constraint not preserved in allOf: %v", conflicting)
	}

	if props["always_ok"] != true {
		t.Fatalf("boolean true $defs target not inlined: %v", props["always_ok"])
	}
	if props["never_ok"] != false {
		t.Fatalf("boolean false $defs target not inlined: %v", props["never_ok"])
	}

	tree := props["tree"].(map[string]any)
	if tree["type"] != "object" {
		t.Fatalf("cyclic Node not inlined at top level: %v", tree)
	}
	childrenItems := tree["properties"].(map[string]any)["children"].(map[string]any)["items"].(map[string]any)
	if _, hasRef := childrenItems["$ref"]; hasRef || childrenItems["type"] != "object" || childrenItems["title"] != "Node" {
		t.Fatalf("cyclic back-reference should be converted to provider-safe object stub without $ref, got: %v", childrenItems)
	}

	nestedProps := props["nested_resource"].(map[string]any)["properties"].(map[string]any)
	localDef := nestedProps["local_def"].(map[string]any)
	if localDef["type"] != "integer" {
		t.Fatalf("nested $id resource should resolve against its own $defs, got: %v", localDef)
	}
	outerDef := nestedProps["outer_def"].(map[string]any)
	if outerDef["$ref"] != "#/$defs/RootOnly" {
		t.Fatalf("nested $id resource should not resolve missing fragment against root $defs, got: %v", outerDef)
	}

	dangling := props["dangling"].(map[string]any)
	if dangling["$ref"] != "#/$defs/DoesNotExist" {
		t.Fatalf("unresolvable $ref should be left intact, got: %v", dangling)
	}
}

func TestInfoInlinesRootRefSchema(t *testing.T) {
	const rootRefSchema = `{
	  "$ref": "#/$defs/Input",
	  "$defs": {
	    "Input": {
	      "type": "object",
	      "properties": {
	        "query": {"type": "string"}
	      },
	      "required": ["query"]
	    }
	  }
	}`
	mt := &mcpTool{serverName: "srv", tool: mcp.Tool{Name: "search", Description: "Search", InputSchema: jsonMap(t, rootRefSchema)}}
	info := mt.Info()
	if !reflect.DeepEqual(info.Required, []string{"query"}) {
		t.Fatalf("root $ref Required = %v, want [query]", info.Required)
	}
	q, ok := info.Parameters["query"].(map[string]any)
	if !ok || q["type"] != "string" {
		t.Fatalf("root $ref Parameters = %v, want query:string", info.Parameters)
	}
}
