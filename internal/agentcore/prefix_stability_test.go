package agentcore

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// Prompt-cache prefix-stability guard (#507). The provider's prompt cache only
// keeps hitting while the cacheable prefix — the system prompt + the tool
// definitions — is BYTE-STABLE across turns. A volatile value (a timestamp, a
// non-deterministic map iteration order) landing in that prefix silently
// restores full input billing with no build-time signal. These tests fence the
// tool-definition half of the prefix (the agentcore-owned, highest-map-order-risk
// component): the exact bytes the model receives for the tool roster must be
// reproducible build-to-build and must not drift without a conscious update.
//
// See docs/PROMPT-CACHE-CONTRACT.md for the full contract (including the
// system-prompt half, which the drivers own) and the breakpoint placement in
// cache.go.

// guardInput is a native-tool parameter struct with several fields, so the
// native-tool serialization path (fantasy reflects the schema from this struct)
// is exercised alongside the MCP map-schema path.
type guardInput struct {
	Query string `json:"query" description:"the search query"`
	Limit int    `json:"limit" description:"max results"`
}

// guardMCPTools returns a fixed catalog of MCP tools whose InputSchemas carry
// multi-key `properties` maps and multi-element `required` slices — the two
// serialization surfaces where non-determinism (Go map iteration order, unsorted
// slices) would silently bust the cache prefix.
func guardMCPTools() []mcp.ServerTool {
	return []mcp.ServerTool{
		{ServerName: "github", Tool: mcp.Tool{
			Name:        "create_issue",
			Description: "Create a GitHub issue.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"title":    map[string]interface{}{"type": "string", "description": "issue title"},
					"body":     map[string]interface{}{"type": "string", "description": "issue body"},
					"labels":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					"assignee": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"title", "body"},
			},
		}},
		{ServerName: "github", Tool: mcp.Tool{
			Name:        "close_issue",
			Description: "Close a GitHub issue by number.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"number": map[string]interface{}{"type": "integer"}},
				"required":   []interface{}{"number"},
			},
		}},
	}
}

// buildGuardTools assembles a representative tool roster through the real
// buildFantasyTools path (native + MCP), the same seam interactive and scheduled
// runs feed the model.
func buildGuardTools(t *testing.T, includeNative bool) []fantasy.AgentTool {
	t.Helper()
	var native []fantasy.AgentTool
	if includeNative {
		native = []fantasy.AgentTool{
			fantasy.NewAgentTool(
				"search_docs",
				"Search the documentation.",
				func(_ context.Context, _ guardInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
					return fantasy.NewTextResponse("ok"), nil
				},
			),
		}
	}
	tools, err := buildFantasyTools(native, guardMCPTools(), nil, nil, nil, nil, nil, toolBuildConfig{})
	if err != nil {
		t.Fatalf("buildFantasyTools: %v", err)
	}
	return tools
}

// serializeToolPrefix renders the exact projection of the tool roster that forms
// the cacheable prefix: for each tool, name + description + parameters + required
// + parallel (fantasy.ToolInfo's JSON shape). Byte-equality of this projection is
// the cache-hit invariant.
func serializeToolPrefix(t *testing.T, tools []fantasy.AgentTool) []byte {
	t.Helper()
	infos := make([]fantasy.ToolInfo, len(tools))
	for i, tl := range tools {
		infos[i] = tl.Info()
	}
	b, err := json.MarshalIndent(infos, "", "  ")
	if err != nil {
		t.Fatalf("marshal tool infos: %v", err)
	}
	return b
}

// TestToolPrefixDeterministic is the core regression fence: building the SAME
// tool roster many times must serialize byte-identically every time. This is
// what catches a future change that introduces non-deterministic ordering near
// the prefix (e.g. a schema assembled by iterating a Go map without sorting, or
// a `fmt.Sprintf("%v", someMap)` that does not sort keys) — the exact silent
// cache-buster the issue describes.
func TestToolPrefixDeterministic(t *testing.T) {
	want := serializeToolPrefix(t, buildGuardTools(t, true))
	for i := 0; i < 64; i++ {
		got := serializeToolPrefix(t, buildGuardTools(t, true))
		if !bytes.Equal(want, got) {
			t.Fatalf("tool-prefix serialization is NON-DETERMINISTIC (iteration %d differs)\n--- first ---\n%s\n--- iter %d ---\n%s",
				i, want, i, got)
		}
	}
}

// TestToolPrefixOrderStable locks the roster ORDER: native tools first (in the
// order given), then MCP tools in catalog order. buildFantasyTools appends to a
// slice in this order; if a future change sourced the catalog from a Go map
// (unstable iteration), the prefix would reorder turn-to-turn and bust the cache.
func TestToolPrefixOrderStable(t *testing.T) {
	tools := buildGuardTools(t, true)
	got := make([]string, len(tools))
	for i, tl := range tools {
		got[i] = tl.Info().Name
	}
	want := []string{"search_docs", "mcp_github_create_issue", "mcp_github_close_issue"}
	if len(got) != len(want) {
		t.Fatalf("tool count = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tool order[%d] = %q, want %q (full order: %v)", i, got[i], want[i], got)
		}
	}
}

// wantToolPrefixGolden is the byte-exact serialization of the MCP tool roster
// (guardMCPTools, no native tools). It is the regression fence for the
// serialization FORMAT: any change to how a tool definition is serialized (field
// order, an added/removed field, whitespace, schema-key sorting) fails this test
// and forces a conscious update — at which point the author must confirm the
// prompt-cache contract still holds. MCP-only (no fantasy struct reflection) so
// the golden is stable across fantasy dependency bumps.
//
// NOTE: `properties` keys are sorted by encoding/json (Go sorts map keys on
// marshal) — that automatic sorting is precisely the byte-stability this golden
// documents and protects.
const wantToolPrefixGolden = `[
  {
    "name": "mcp_github_create_issue",
    "description": "Create a GitHub issue.",
    "parameters": {
      "assignee": {
        "type": "string"
      },
      "body": {
        "description": "issue body",
        "type": "string"
      },
      "labels": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "title": {
        "description": "issue title",
        "type": "string"
      }
    },
    "required": [
      "title",
      "body"
    ],
    "parallel": false
  },
  {
    "name": "mcp_github_close_issue",
    "description": "Close a GitHub issue by number.",
    "parameters": {
      "number": {
        "type": "integer"
      }
    },
    "required": [
      "number"
    ],
    "parallel": false
  }
]`

func TestToolPrefixGolden(t *testing.T) {
	got := serializeToolPrefix(t, buildGuardTools(t, false))
	if string(got) != wantToolPrefixGolden {
		t.Fatalf("tool-prefix serialization drifted from the golden.\n"+
			"If this change is intentional, update wantToolPrefixGolden AND confirm the\n"+
			"prompt-cache contract (docs/PROMPT-CACHE-CONTRACT.md) still holds.\n\n--- got ---\n%s\n\n--- want ---\n%s",
			got, wantToolPrefixGolden)
	}
}
