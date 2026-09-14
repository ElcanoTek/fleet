package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// BM25 progressive tool disclosure (#506). When the registered roster would
// exceed the disclosure threshold, the deferrable (MCP) tools are hidden behind
// three bridge tools — tool_search → tool_describe → tool_call — backed by an
// in-process BM25 index (internal/tools). Core tools (native, loader,
// confirm_audit) are NEVER deferred, so bash/python/approvals/etc.
// stay directly callable.
//
// A deferred call routes through the SAME *mcpTool wrapper a direct call would:
// tool_call looks the tool up by name and invokes its Run, so policy gating
// (BeforeToolCall/RecordToolResult), the MCP broker + credential allowlist,
// output redaction, the output ceiling, and audit are all applied identically —
// a deferred tool is first-class, just not always advertised.

// The three bridge tool names, as constants so the persona filter (#570) can
// recognize the bridges as disclosure plumbing rather than capabilities.
const (
	toolNameToolSearch   = "tool_search"
	toolNameToolDescribe = "tool_describe"
	toolNameToolCall     = "tool_call"
)

// isDisclosureBridge reports whether name is one of the three disclosure bridge
// tools. Used by resolvePersonaTools: a persona allow-list must not strand the
// bridges (the tools reachable through them are persona-filtered before they
// enter the deferred registry), though an explicit deny still removes them.
func isDisclosureBridge(name string) bool {
	return name == toolNameToolSearch || name == toolNameToolDescribe || name == toolNameToolCall
}

// disclosureThresholdOverride is the admin-settings live override (0 = unset).
// The admin Features panel sets it through SetToolDisclosureThreshold; the
// roster build reads it per turn, so a change applies on the next turn with no
// restart. Atomic because the setter runs on the admin-API goroutine while
// turns read concurrently.
var disclosureThresholdOverride atomic.Int64

// SetToolDisclosureThreshold installs (n > 0) or clears (n <= 0) the
// process-wide admin override for the disclosure threshold. Called by the
// workspace-settings apply hook in cmd/fleet.
func SetToolDisclosureThreshold(n int) {
	if n < 0 {
		n = 0
	}
	disclosureThresholdOverride.Store(int64(n))
}

// disclosureThreshold returns the roster size at or below which nothing is
// deferred (small catalogs are byte-for-byte unchanged). Above it, MCP tools
// defer. Precedence: admin override (workspace settings), then
// FLEET_TOOL_DISCLOSURE_THRESHOLD; default is the provider ceiling so a roster
// that would otherwise ERROR instead degrades to disclosure.
func disclosureThreshold() int {
	if n := disclosureThresholdOverride.Load(); n > 0 {
		return int(n)
	}
	return EnvToolDisclosureThreshold()
}

// EnvToolDisclosureThreshold returns the env-derived threshold (ignoring any
// admin override) — the workspace-settings boot wiring reports it as the value
// a reset reverts to.
func EnvToolDisclosureThreshold() int {
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
	return fantasy.NewAgentTool(toolNameToolSearch, desc,
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
	return fantasy.NewAgentTool(toolNameToolDescribe,
		"Show a deferred tool's full description and JSON parameter schema (from tool_search). Call this before tool_call so you pass the right arguments.",
		func(_ context.Context, p toolDescribeParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			name := strings.TrimSpace(p.Name)
			t, ok := r.byName[name]
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("tool_describe: no tool named %q — use tool_search to find the right name.", name)), nil
			}
			info := t.Info()
			// Print the schema the provider would receive for a directly
			// registered tool: type, properties AND required. Until #1006's
			// catalog audit this printed the properties map alone, so a model
			// in deferred mode could not learn that Stripe's every API tool
			// needs `stripe_context` and `livemode` — it omitted them and the
			// vendor answered HTTP 422 — while the same tool registered
			// directly (small roster) carried its required list natively.
			props := info.Parameters
			if props == nil {
				props = map[string]any{}
			}
			schema := map[string]any{"type": "object", "properties": props}
			if len(info.Required) > 0 {
				schema["required"] = info.Required
			}
			out, _ := json.MarshalIndent(schema, "", "  ")
			required := "none"
			if len(info.Required) > 0 {
				required = strings.Join(info.Required, ", ")
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Tool: %s\n\n%s\n\nParameters (JSON Schema):\n%s\n\nRequired arguments: %s.\n\nCall it with tool_call {\"name\":%q,\"arguments\":{…}}.",
				info.Name, info.Description, string(out), required, info.Name)), nil
		})
}

func (r *deferredToolRegistry) callTool() fantasy.AgentTool {
	return &deferredToolCall{registry: r}
}

// deferredToolCall is implemented explicitly rather than through
// fantasy.NewAgentTool. json.RawMessage's underlying Go type is []byte, so the
// reflection-based schema generator advertises it as an integer ARRAY even
// though a deferred tool's arguments must be a JSON OBJECT. Models that obeyed
// that schema wrapped the object in an array and then looped forever on the MCP
// decoder's "cannot unmarshal array into map" error (#606).
//
// Owning Info here makes the provider-facing contract truthful. Run also
// accepts the legacy singleton-array shape so an in-flight/cached model response
// generated from the old schema recovers immediately after a rolling deploy.
type deferredToolCall struct {
	registry        *deferredToolRegistry
	providerOptions fantasy.ProviderOptions
}

func (t *deferredToolCall) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name: "tool_call",
		Description: "Invoke a deferred tool by name with its arguments (from tool_describe). " +
			"The call runs under the same policy, credential, and audit controls as any tool.",
		Parameters: map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "The exact tool name from tool_search/tool_describe.",
			},
			"arguments": map[string]any{
				"type":                 "object",
				"description":          "The tool's arguments as a JSON object, matching its parameter schema.",
				"additionalProperties": true,
			},
		},
		Required: []string{"name", "arguments"},
	}
}

func (t *deferredToolCall) ProviderOptions() fantasy.ProviderOptions {
	return t.providerOptions
}

func (t *deferredToolCall) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.providerOptions = opts
}

func (t *deferredToolCall) Run(ctx context.Context, tc fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(tc.Input), &p); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid parameters: %s", err)), nil
	}

	name := strings.TrimSpace(p.Name)
	tool, ok := t.registry.byName[name]
	if !ok {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("tool_call: no tool named %q — use tool_search to find the right name.", name)), nil
	}

	args, err := normalizeDeferredArguments(p.Arguments)
	if err != nil {
		//nolint:nilerr // intentional: a malformed arguments payload is reported to the MODEL as a text error response (like the two rejections above) so it can correct the call; a non-nil Go error would abort the run.
		return fantasy.NewTextErrorResponse("tool_call: " + err.Error()), nil
	}
	// Refuse a call that omits an argument the tool's own schema marks
	// required, and NAME the missing ones. The vendor would refuse it anyway,
	// but its answer reaches the model only as the broker's masked
	// "credential-owner call failed" (the real text is a host-log line), which
	// reads as a broken credential rather than a fixable call — Stripe's
	// `stripe_context`/`livemode` 422s sent a model off to tell the user to
	// reconnect (#1006). Checked here, before the broker and before any
	// policy or audit record, so the correction costs one model step.
	info := tool.Info()
	if missing := missingRequiredArguments(info.Required, info.Parameters, args); len(missing) > 0 {
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"tool_call: %s requires argument(s) it did not receive: %s. Run tool_describe %q for the schema, then call again with every required argument.",
			name, strings.Join(missing, ", "), name)), nil
	}

	// Dispatch through the real tool's Run with the SAME call id, so its policy
	// gate + broker + audit + redaction all apply as if called directly.
	return tool.Run(ctx, fantasy.ToolCall{ID: tc.ID, Name: name, Input: string(args)})
}

func normalizeDeferredArguments(raw json.RawMessage) (json.RawMessage, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`{}`), nil
	}
	if raw[0] == '{' {
		return raw, nil
	}
	if raw[0] != '[' {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}

	var legacy []json.RawMessage
	if err := json.Unmarshal(raw, &legacy); err != nil || len(legacy) != 1 {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	one := json.RawMessage(strings.TrimSpace(string(legacy[0])))
	if len(one) == 0 || one[0] != '{' {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	return one, nil
}

// missingRequiredArguments returns the names in required that args (a JSON
// object) does not carry. JSON Schema's `required` means present, so a name
// carried as JSON null is present — unless the property's own schema (looked
// up in props) does not admit null, in which case the null is what a model
// emits when it knows the name but not the value, and the vendor would refuse
// it too. A property whose schema admits null (`type: ["string","null"]`,
// `nullable: true`, an `anyOf`/`oneOf` arm or `enum` member of null) keeps
// null as a valid value, so the deferred path never refuses a call the direct
// path would have executed. An unparsable object yields nothing: the dispatch
// path reports that shape on its own.
func missingRequiredArguments(required []string, props map[string]any, args json.RawMessage) []string {
	if len(required) == 0 {
		return nil
	}
	var have map[string]json.RawMessage
	if err := json.Unmarshal(args, &have); err != nil {
		return nil
	}
	var missing []string
	for _, name := range required {
		v, ok := have[name]
		if !ok || (strings.TrimSpace(string(v)) == "null" && !schemaAdmitsNull(props[name])) {
			missing = append(missing, name)
		}
	}
	return missing
}

// schemaAdmitsNull reports whether a JSON Schema property accepts the JSON
// null value. It recognises the shapes vendors use: `type: "null"` or a type
// list containing "null", OpenAPI's `nullable: true`, an `enum` or `const`
// naming null, an `anyOf`/`oneOf` arm that admits null, and an `allOf` whose
// every arm does. A schema that constrains the value with none of those
// keywords (`{}`, or description-only) accepts anything, null included — and
// so do the boolean schema `true` and a property with no schema at all —
// because the refusal must never be stricter than the vendor's own
// validation. The boolean schema `false` admits nothing.
func schemaAdmitsNull(schema any) bool {
	if b, ok := schema.(bool); ok {
		return b
	}
	m, ok := schema.(map[string]any)
	if !ok {
		return schema == nil
	}
	if b, ok := m["nullable"].(bool); ok && b {
		return true
	}
	constrained := false
	if t, ok := m["type"]; ok {
		constrained = true
		switch t := t.(type) {
		case string:
			if t == "null" {
				return true
			}
		case []any:
			for _, v := range t {
				if s, ok := v.(string); ok && s == "null" {
					return true
				}
			}
		}
	}
	if enum, ok := m["enum"].([]any); ok {
		constrained = true
		for _, v := range enum {
			if v == nil {
				return true
			}
		}
	}
	if c, ok := m["const"]; ok {
		constrained = true
		if c == nil {
			return true
		}
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		arms, ok := m[key].([]any)
		if !ok || len(arms) == 0 {
			continue
		}
		constrained = true
		for _, arm := range arms {
			if schemaAdmitsNull(arm) {
				return true
			}
		}
	}
	if arms, ok := m["allOf"].([]any); ok && len(arms) > 0 {
		constrained = true
		all := true
		for _, arm := range arms {
			if !schemaAdmitsNull(arm) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return !constrained
}

// oneLine collapses whitespace and clamps a description for the search listing.
func oneLine(s string, maxChars int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxChars {
		s = s[:maxChars] + "…"
	}
	return s
}
