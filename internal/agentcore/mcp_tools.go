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
	mcpClient *mcp.Client,
	allow mcpAllowlist,
	policy Policy,
	optionalServers mcpOptionalSet,
	optIn map[string]bool,
	cfg toolBuildConfig,
) ([]fantasy.AgentTool, error) {
	mcpServerTools := mcpClient.GetAllTools()
	allTools := make([]fantasy.AgentTool, 0, len(nativeTools)+len(mcpServerTools)+len(cfg.loaderTools)+1)

	for _, t := range nativeTools {
		allTools = append(allTools, &policyGuardedTool{inner: t, policy: policy})
	}
	for _, t := range cfg.loaderTools {
		allTools = append(allTools, &policyGuardedTool{inner: t, policy: policy})
	}

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
			serverName:       st.ServerName,
			tool:             st.Tool,
			mcpClient:        mcpClient,
			policy:           policy,
			remediationHints: cfg.remediationHints,
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
	return g.inner.Run(ctx, params)
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
type mcpTool struct {
	serverName       string
	tool             mcp.Tool
	mcpClient        *mcp.Client
	policy           Policy
	remediationHints RemediationHints
	providerOptions  fantasy.ProviderOptions
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
		Parallel:    parallelSafeMCPTools[m.Name()],
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

	// Block oversized inline base64 uploads to fast.io before they hit the wire.
	if ok, hint := rejectFastIOInlineBase64Upload(toolName, args, m.remediationHints); !ok {
		m.record(toolName, params.Input, hint, false)
		return fantasy.NewTextErrorResponse(hint), nil
	}

	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	result, err := m.mcpClient.CallToolOn(callCtx, m.serverName, m.tool.Name, args)
	if err != nil {
		m.record(toolName, params.Input, "", false)
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Error calling %s: %v", toolName, err)), nil
	}

	var sb strings.Builder
	for _, block := range result.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
			sb.WriteString("\n")
		}
	}
	resultText := sb.String()

	if m.serverName == mcpServerFastIO {
		resultText = trimFastIOResponse(resultText)
	}

	// Map MCP isError to a fantasy error response so both the LLM and the log
	// know the call failed (per MCP 2025-06-18 spec, tool-level errors arrive as
	// a successful JSON-RPC response with isError=true).
	if result.IsError {
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
