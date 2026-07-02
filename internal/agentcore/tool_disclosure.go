package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// BM25 progressive tool disclosure (#506). When the registered roster would
// exceed the disclosure threshold, the deferrable (MCP) tools are hidden behind
// three bridge tools — tool_search → tool_describe → tool_call — backed by an
// in-process BM25 index (internal/tools). Core tools (native, loader,
// pre-gated, confirm_audit) are NEVER deferred, so bash/python/approvals/etc.
// stay directly callable.
//
// A deferred call routes through the SAME *mcpTool wrapper a direct call would:
// tool_call looks the tool up by name and invokes its Run, so policy gating
// (BeforeToolCall/RecordToolResult), the MCP broker + credential allowlist,
// output redaction, the output ceiling, and audit are all applied identically —
// a deferred tool is first-class, just not always advertised.

// disclosureThreshold returns the roster size at or below which nothing is
// deferred (small catalogs are byte-for-byte unchanged). Above it, MCP tools
// defer. FLEET_TOOL_DISCLOSURE_THRESHOLD overrides; default is the provider
// ceiling so a roster that would otherwise ERROR instead degrades to disclosure.
func disclosureThreshold() int {
	if v := strings.TrimSpace(os.Getenv("FLEET_TOOL_DISCLOSURE_THRESHOLD")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return maxToolsPerRequest
}

// deferredToolRegistry holds the hidden tools keyed by their advertised name,
// plus the BM25 index over their metadata, shared by the three bridges.
type deferredToolRegistry struct {
	byName map[string]fantasy.AgentTool
	descs  map[string]string // name → full description (for tool_describe)
	index  *tools.BM25Index
}

func newDeferredToolRegistry(deferred []fantasy.AgentTool) *deferredToolRegistry {
	r := &deferredToolRegistry{
		byName: make(map[string]fantasy.AgentTool, len(deferred)),
		descs:  make(map[string]string, len(deferred)),
	}
	docs := make([]tools.BM25Doc, 0, len(deferred))
	for _, t := range deferred {
		info := t.Info()
		// Last write wins on a name collision (same as direct registration).
		r.byName[info.Name] = t
		r.descs[info.Name] = info.Description
		docs = append(docs, tools.BM25Doc{ID: info.Name, Text: info.Name + " " + info.Description})
	}
	r.index = tools.NewBM25Index(docs)
	return r
}

// bridgeTools returns the three disclosure bridge tools. They are plain native
// tools (the pool's policy wraps them like any native tool); the underlying
// deferred call it dispatches carries its own gating.
func (r *deferredToolRegistry) bridgeTools() []fantasy.AgentTool {
	return []fantasy.AgentTool{r.searchTool(), r.describeTool(), r.callTool()}
}

const toolSearchDefaultLimit = 10

type toolSearchParams struct {
	Query string `json:"query" description:"Keywords describing the capability you need (e.g. 'send slack message', 'create jira ticket'). Returns the best-matching tool names + one-line descriptions."`
	Limit int    `json:"limit,omitempty" description:"Max results (default 10)."`
}

func (r *deferredToolRegistry) searchTool() fantasy.AgentTool {
	desc := fmt.Sprintf(`Search the %d additional tools available in this workspace by keyword.

Many tools are not listed directly (there are too many to show at once). Use
tool_search to find the one you need, then tool_describe to see its exact
parameters, then tool_call to invoke it. Returns matching tool NAMES with a
short description each.`, len(r.byName))
	return fantasy.NewAgentTool("tool_search", desc,
		func(_ context.Context, p toolSearchParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			q := strings.TrimSpace(p.Query)
			if q == "" {
				return fantasy.NewTextErrorResponse("tool_search: query is required"), nil
			}
			limit := p.Limit
			if limit <= 0 {
				limit = toolSearchDefaultLimit
			}
			hits := r.index.Search(q, limit)
			if len(hits) == 0 {
				return fantasy.NewTextResponse("No tools matched. Try different keywords, or the capability may not be available."), nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Matching tools (use tool_describe for parameters, then tool_call):\n")
			for _, h := range hits {
				fmt.Fprintf(&b, "- %s: %s\n", h.ID, oneLine(r.descs[h.ID], 160))
			}
			return fantasy.NewTextResponse(b.String()), nil
		})
}

type toolDescribeParams struct {
	Name string `json:"name" description:"The exact tool name from tool_search."`
}

func (r *deferredToolRegistry) describeTool() fantasy.AgentTool {
	return fantasy.NewAgentTool("tool_describe",
		"Show a deferred tool's full description and JSON parameter schema (from tool_search). Call this before tool_call so you pass the right arguments.",
		func(_ context.Context, p toolDescribeParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			name := strings.TrimSpace(p.Name)
			t, ok := r.byName[name]
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("tool_describe: no tool named %q — use tool_search to find the right name.", name)), nil
			}
			info := t.Info()
			schema, _ := json.MarshalIndent(info.Parameters, "", "  ")
			return fantasy.NewTextResponse(fmt.Sprintf("Tool: %s\n\n%s\n\nParameters (JSON Schema):\n%s\n\nCall it with tool_call {\"name\":%q,\"arguments\":{…}}.",
				info.Name, info.Description, string(schema), info.Name)), nil
		})
}

type toolCallParams struct {
	Name      string          `json:"name" description:"The exact tool name from tool_search/tool_describe."`
	Arguments json.RawMessage `json:"arguments" description:"The tool's arguments as a JSON object, matching its parameter schema."`
}

func (r *deferredToolRegistry) callTool() fantasy.AgentTool {
	return fantasy.NewAgentTool("tool_call",
		"Invoke a deferred tool by name with its arguments (from tool_describe). The call runs under the same policy, credential, and audit controls as any tool.",
		func(ctx context.Context, p toolCallParams, tc fantasy.ToolCall) (fantasy.ToolResponse, error) {
			name := strings.TrimSpace(p.Name)
			t, ok := r.byName[name]
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("tool_call: no tool named %q — use tool_search to find the right name.", name)), nil
			}
			args := string(p.Arguments)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			// Dispatch through the real tool's Run with the SAME call id, so its
			// policy gate + broker + audit + redaction all apply as if it had been
			// called directly. The result is returned verbatim.
			return t.Run(ctx, fantasy.ToolCall{ID: tc.ID, Name: name, Input: args})
		})
}

// oneLine collapses whitespace and clamps a description for the search listing.
func oneLine(s string, maxChars int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxChars {
		s = s[:maxChars] + "…"
	}
	return s
}
