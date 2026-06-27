package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// MCP tool wrapping + the ONE buildFantasyTools skeleton both modes feed
// (merged from chat + cutlass fantasy.go).
//
// The Gate-1 opt-in (`if optionalServers[name] && !optIn[name] { skip }`) is
// chat's gate and is byte-identical for both modes per the migration ledger:
// the scheduled producer derives optIn from its task's MCPSelection server
// names, the interactive producer from the conversation's opt-in list. Accounts
// do NOT affect which tools register (that is §6.3 wiring); they affect which
// subprocess/env backs the server.
//
// Tool-level enforcement is routed through the Policy seam (BeforeToolCall /
// RecordToolResult) rather than a hardcoded orchestrationState method chain, so
// the interactive bundle (approvals/ceilings) and the scheduled bundle
// (audit gating) plug into the SAME wrapper. cutlass's additive mcpTool.Run
// behaviours — isError→error mapping, per-tool call timeout, parallel-safe
// marking, fast.io response trimming — are preserved.

// maxToolsPerRequest is OpenAI's hard ceiling on the tools array per request.
const maxToolsPerRequest = 128

// toolCallTimeout bounds a single MCP tool call so a hung stdio server can't
// block the agent loop.
const toolCallTimeout = 5 * time.Minute

// mcpAllowlist maps server name → allowed tool names. Empty/missing = allow all.
type mcpAllowlist map[string][]string

// mcpOptionalSet reports whether a server is Optional (participates only when
// opted in for the run).
type mcpOptionalSet map[string]bool

// MCPAllowlist / MCPOptionalSet are the exported aliases the DRIVERS use to
// build a RunConfig (the underlying map types are otherwise unexported).
type (
	// MCPAllowlist maps server name → allowed tool names (Gate-2). Empty = all.
	MCPAllowlist = mcpAllowlist
	// MCPOptionalSet reports which servers are Optional (Gate-1 opt-in).
	MCPOptionalSet = mcpOptionalSet
)

// toolBuildConfig parameterizes the divergences between the two modes' tool sets.
type toolBuildConfig struct {
	// includeConfirmAudit appends the scheduled-mode confirm_audit tool.
	includeConfirmAudit bool
	// loaderTools are extra always-registered tools (scheduled mcp_list/load).
	loaderTools []fantasy.AgentTool
	// remediationHints configures the fast.io inline-upload guard hint.
	remediationHints RemediationHints
	// preGatedTools are already-policy-aware tools registered verbatim (the
	// native-acp delegating MCP tools). They call BeforeToolCall +
	// RecordToolResult themselves, so they are NOT wrapped in policyGuardedTool.
	preGatedTools []fantasy.AgentTool
}

// buildFantasyTools combines native tools with discovered MCP tools into the
// single slice the fantasy agent wants, applying the Gate-1 opt-in and the
// per-server allowlist. Every tool is wrapped in a policy-guarded wrapper so
// cost/audit/repeat enforcement runs before each call.
//
// optionalServers may be nil/empty. optIn is the per-run enabled set (server
// names). policy is the seam both modes feed.
func buildFantasyTools(
	nativeTools []fantasy.AgentTool,
	mcpServerTools []mcp.ServerTool,
	broker MCPBroker,
	allow mcpAllowlist,
	policy Policy,
	optionalServers mcpOptionalSet,
	optIn map[string]bool,
	cfg toolBuildConfig,
) ([]fantasy.AgentTool, error) {
	// mcpServerTools is the tool CATALOG (discovery, as data); broker is the seam
	// each tool's CALL routes through (the in-process localMCPBroker by default, or
	// an injected out-of-process broker — issue #167). They are deliberately
	// separate: where a call runs is decoupled from where the catalog is read, so
	// the broker can own the client while the loop just advertises what it fetched.
	allTools := make([]fantasy.AgentTool, 0, len(nativeTools)+len(mcpServerTools)+len(cfg.loaderTools)+len(cfg.preGatedTools)+1)

	for _, t := range nativeTools {
		allTools = append(allTools, &policyGuardedTool{inner: t, policy: policy})
	}
	for _, t := range cfg.loaderTools {
		allTools = append(allTools, &policyGuardedTool{inner: t, policy: policy})
	}
	// Pre-gated tools (native-acp delegating MCP tools) own their policy handling
	// (BeforeToolCall + RecordToolResult), exactly like the built-in mcpTool, so
	// they register VERBATIM — wrapping them would double the gate and drop
	// RecordToolResult.
	allTools = append(allTools, cfg.preGatedTools...)

	mcpRegistered := 0
	mcpSkippedOptional := 0
	mcpSkippedAllowlist := 0
	for _, st := range mcpServerTools {
		// Gate 1: Optional servers only pass if the run opted in. Byte-identical
		// between modes.
		if optionalServers[st.ServerName] && !optIn[st.ServerName] {
			mcpSkippedOptional++
			continue
		}
		// Gate 2: per-server tool allowlist.
		if list, ok := allow[st.ServerName]; ok && len(list) > 0 && !slices.Contains(list, st.Tool.Name) {
			mcpSkippedAllowlist++
			continue
		}
		allTools = append(allTools, &mcpTool{
			serverName: st.ServerName,
			tool:       st.Tool,
			broker:     broker,
			policy:     policy,
		})
		mcpRegistered++
	}

	if cfg.includeConfirmAudit {
		allTools = append(allTools, &policyGuardedTool{inner: buildConfirmAuditPolicyTool(policy), policy: policy})
	}

	log.Printf("Fantasy tools registered: %d (%d native + %d loader + %d MCP, %d MCP skipped optional, %d MCP skipped allowlist)",
		len(allTools), len(nativeTools), len(cfg.loaderTools), mcpRegistered, mcpSkippedOptional, mcpSkippedAllowlist)

	if len(allTools) > maxToolsPerRequest {
		return nil, fmt.Errorf("registered %d tools, exceeds the %d-tool ceiling", len(allTools), maxToolsPerRequest)
	}
	return allTools, nil
}

// buildConfirmAuditPolicyTool returns the scheduled confirm_audit tool wired to
// the policy's underlying orchestration when available, else a no-op stub. The
// scheduled Policy bundle (P3) embeds an orchestrationState; we type-assert it
// so the tool can mutate audit state.
func buildConfirmAuditPolicyTool(policy Policy) fantasy.AgentTool {
	for p := policy; p != nil; {
		if op, ok := p.(interface{ orchestration() *orchestrationState }); ok {
			if orch := op.orchestration(); orch != nil {
				return buildConfirmAuditTool(orch)
			}
		}
		w, ok := p.(PolicyUnwrapper)
		if !ok {
			break
		}
		p = w.Unwrap()
	}
	// Fallback: a confirm_audit that always reports it isn't wired (keeps the
	// schema present so the model can still call it in test doubles).
	return fantasy.NewAgentTool(
		toolNameConfirmAudit,
		"Confirms that the self-audit protocol has been completed.",
		func(_ context.Context, _ confirmAuditInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("Audit acknowledged."), nil
		},
	)
}

// policyGuardedTool wraps any tool with the Policy gate: BeforeToolCall may
// block (returns the message as the tool result without executing); on
// execution, RecordToolResult records the outcome.
type policyGuardedTool struct {
	inner  fantasy.AgentTool
	policy Policy
}

func (g *policyGuardedTool) Info() fantasy.ToolInfo { return g.inner.Info() }
func (g *policyGuardedTool) ProviderOptions() fantasy.ProviderOptions {
	return g.inner.ProviderOptions()
}
func (g *policyGuardedTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	g.inner.SetProviderOptions(opts)
}

func (g *policyGuardedTool) Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	name := g.inner.Info().Name
	if g.policy != nil {
		if blocked, msg := g.policy.BeforeToolCall(name, params.ID, params.Input); blocked {
			return fantasy.NewTextErrorResponse(msg), nil
		}
	}
	resp, err := g.inner.Run(ctx, params)
	// Scrub secrets from tool output at the choke point: the redacted text is what
	// re-enters the model context next turn, what RecordToolResult/the policy sees,
	// what the stream sink records, and what the session log persists.
	if resp.Content != "" {
		resp.Content = toolRedactor().Redact(resp.Content)
	}
	if g.policy != nil {
		// Record the outcome so policies that gate on tool RESULTS observe native
		// tool calls (bash/python/task_tracker/...), not just the MCP and
		// delegating tools. Without this the scheduled task-tracker finish gate
		// (latestTaskTracker.Seen) never fired in production. A transport error or
		// an is-error response counts as a failed call.
		g.policy.RecordToolResult(name, params.Input, resp.Content, err == nil && !resp.IsError)
	}
	return resp, err
}

// sanitizeSchemaProperties deep-copies a JSON-schema "properties" map and strips
// any `pattern` entries using `\p{…}` Unicode property escapes, which OpenAI's
// function-calling validator rejects (ECMA-262 only).
func sanitizeSchemaProperties(props map[string]any) map[string]any {
	out := make(map[string]any, len(props))
	for k, v := range props {
		out[k] = sanitizeSchemaValue(v)
	}
	return out
}

// SanitizeSchemaProperties is the exported form used by the native-acp agent's
// delegating MCP tools so they sanitize their advertised schema EXACTLY as the
// in-process mcpTool does (parity: OpenAI rejects `\p{…}` patterns).
func SanitizeSchemaProperties(props map[string]any) map[string]any {
	return sanitizeSchemaProperties(props)
}

const jsonSchemaPatternKey = "pattern"

func sanitizeSchemaValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		clone := make(map[string]any, len(t))
		for k, vv := range t {
			if k == jsonSchemaPatternKey {
				if s, ok := vv.(string); ok && strings.Contains(s, `\p{`) {
					continue
				}
			}
			clone[k] = sanitizeSchemaValue(vv)
		}
		return clone
	case []any:
		clone := make([]any, len(t))
		for i, vv := range t {
			clone[i] = sanitizeSchemaValue(vv)
		}
		return clone
	default:
		return v
	}
}

// mcpTool wraps an MCP server tool as a fantasy.AgentTool (crush pattern).
// Named mcp_<server>_<tool> to avoid collisions across servers.
//
// The actual call runs through the MCPBroker seam (not a direct *mcp.Client),
// so where the connector credentials live is decoupled from where this loop runs
// (see MCPBroker). mcpTool owns the per-call FRAMING — policy gate, arg parse,
// timeout, isError→error mapping, result recording — while the broker owns the
// call itself (guard, transport, flatten, fast.io trim).
type mcpTool struct {
	serverName      string
	tool            mcp.Tool
	broker          MCPBroker
	policy          Policy
	providerOptions fantasy.ProviderOptions
}

func (m *mcpTool) Name() string {
	return fmt.Sprintf("mcp_%s_%s", m.serverName, m.tool.Name)
}

func (m *mcpTool) Info() fantasy.ToolInfo {
	parameters := make(map[string]any)
	required := make([]string, 0)

	if input, ok := m.tool.InputSchema["properties"].(map[string]any); ok {
		parameters = sanitizeSchemaProperties(input)
	}
	if req, ok := m.tool.InputSchema["required"].([]any); ok {
		for _, v := range req {
			if s, ok := v.(string); ok {
				required = append(required, s)
			}
		}
	} else if reqStr, ok := m.tool.InputSchema["required"].([]string); ok {
		required = reqStr
	}

	return fantasy.ToolInfo{
		Name:        m.Name(),
		Description: m.tool.Description,
		Parameters:  parameters,
		Required:    required,
		Parallel:    isParallelSafeTool(m.Name()),
	}
}

func (m *mcpTool) Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	toolName := m.Name()

	if m.policy != nil {
		if blocked, msg := m.policy.BeforeToolCall(toolName, params.ID, params.Input); blocked {
			return fantasy.NewTextErrorResponse(msg), nil
		}
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(params.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	// The broker owns the call itself — the fast.io inline-upload guard, the
	// transport against the credentialed client, the content flatten, and the
	// fast.io response trim. mcpTool keeps only the per-call framing.
	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	resultText, isErr, err := m.broker.CallMCP(callCtx, m.serverName, m.tool.Name, args)
	if err != nil {
		m.record(toolName, params.Input, "", false)
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Error calling %s: %v", toolName, err)), nil
	}
	// Scrub secrets from MCP output before it is recorded, returned to the model,
	// or streamed/persisted downstream.
	resultText = toolRedactor().Redact(resultText)

	// Map MCP isError to a fantasy error response so both the LLM and the log
	// know the call failed (per MCP 2025-06-18 spec, tool-level errors arrive as
	// a successful JSON-RPC response with isError=true). The fast.io guard above
	// also surfaces through this path (it returns isError=true with the hint text).
	if isErr {
		errText := resultText
		if errText == "" {
			errText = fmt.Sprintf("MCP tool %s returned isError=true with no text content", toolName)
		}
		m.record(toolName, params.Input, errText, false)
		return fantasy.NewTextErrorResponse(errText), nil
	}

	m.record(toolName, params.Input, resultText, true)
	return fantasy.NewTextResponse(resultText), nil
}

func (m *mcpTool) record(toolName, rawInput, resultText string, succeeded bool) {
	if m.policy != nil {
		m.policy.RecordToolResult(toolName, rawInput, resultText, succeeded)
	}
}

func (m *mcpTool) ProviderOptions() fantasy.ProviderOptions     { return m.providerOptions }
func (m *mcpTool) SetProviderOptions(o fantasy.ProviderOptions) { m.providerOptions = o }
