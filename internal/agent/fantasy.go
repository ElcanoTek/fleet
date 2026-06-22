// Package agent wires the charm.land/fantasy LLM driver to the MCP client,
// tool set, and orchestration state for the fleet interactive (chat) agent.
//
// This file is the reusable subset ported from chat's fantasy.go: MCP tool
// adaptation (the "crush pattern"), the optional-server gating in
// buildFantasyTools, and JSON-schema sanitization.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// ── tool building ──

// maxToolsPerRequest is OpenAI's hard ceiling on the `tools` array per chat
// completion request. Above this providers return 400 before the model sees
// the call.
const maxToolsPerRequest = 128

// fastIOServerName is the MCP server registration key for fast.io.
// Used by the tool-list probe (fastIOServerEnabled) and the response-trim
// gate to recognize the relevant server.
const fastIOServerName = "fast_io"

// fastIOUploadToolName is the prefixed name the agent harness gives
// to fast.io's `upload` MCP tool. Declared as a package constant so the
// overflow sentinel tests and the dispatcher guard share one literal.
const fastIOUploadToolName = "mcp_fast_io_upload"

// mcpAllowlist maps server name → list of allowed tool names. Empty list
// (or missing key) means "register every tool the server advertises".
type mcpAllowlist map[string][]string

// mcpOptionalSet reports whether a given server name is Optional — i.e.
// only participates in a turn when the conversation has opted in via the
// settings UI. Built once at Manager.New() from the MCPServerSpec.Optional
// flags and read on every turn.
type mcpOptionalSet map[string]bool

// fastIOServerEnabled reports whether the fast_io MCP server is wired
// up for this turn. We probe by name rather than by token presence so
// the check still works in tests (where a fake client carries no
// config) and so a future rename of the env var doesn't silently
// disable the native upload tool.
func fastIOServerEnabled(serverTools []mcp.ServerTool) bool {
	for _, st := range serverTools {
		if st.ServerName == fastIOServerName {
			return true
		}
	}
	return false
}

// buildFantasyTools combines native tools with discovered MCP tools into the
// single slice the fantasy agent wants. Three gates filter MCP tools, in
// order:
//
//  1. Optional + not-opted-in: if the server is marked Optional (e.g.
//     gamma) and the conversation has NOT opted in, the tool is dropped
//     entirely — its schema never reaches the LLM.
//  2. Allowlist: if the server's mcpAllowlist entry is non-empty and the
//     tool name isn't on it, drop it.
//
// Every tool — native AND MCP — is wrapped in a ceiling-check so
// cost/token runaways stop before the next step, not just at MCP boundaries.
//
// optionalServers may be nil (no Optional servers configured) or empty
// (Optional servers exist but none opted in for this turn). enabledOptIns
// is the per-conversation list from store.Conversation.OptionalMCPServersEnabled.
func buildFantasyTools(
	nativeTools []fantasy.AgentTool,
	mcpClient *mcp.Client,
	allow mcpAllowlist, //nolint:unparam // per-server tool allowlist (Gate 2 below) is wired infrastructure; no caller supplies a non-nil allowlist yet, but the filter is intentional and must stay live.
	orch *orchestrationState,
	optionalServers mcpOptionalSet,
	enabledOptIns []string,
) ([]fantasy.AgentTool, error) {
	mcpServerTools := mcpClient.GetAllTools()
	allTools := make([]fantasy.AgentTool, 0, len(nativeTools)+len(mcpServerTools))

	// Cheap membership lookup for the per-turn opt-in list. Built before
	// native registration so we can gate native optional tools (currently
	// just generate_image) by the same list users use for Optional MCPs.
	optInSet := make(map[string]bool, len(enabledOptIns))
	for _, n := range enabledOptIns {
		optInSet[n] = true
	}

	nativeSkippedOptional := 0
	for _, t := range nativeTools {
		if gate := nativeOptInGate(t.Info().Name); gate != "" && !optInSet[gate] {
			nativeSkippedOptional++
			continue
		}
		allTools = append(allTools, &ceilingGuardedTool{inner: t, orch: orch})
	}

	// Register fastio_upload_file when the fast_io MCP server is wired
	// up. The native tool reads files from the per-conversation
	// workspace, base64-encodes them in Go (deterministic, no
	// model-side mangling), and forwards via `mcp_fast_io_upload`
	// stream-upload. The whole point is keeping bytes out of the
	// model's context, so this lives next to the inline-base64 guard
	// in mcp_fastio_guard.go — same intent, opposite end of the wire.
	//
	// fastio_find is the read-side companion: a smart wrapper around
	// `storage action=search` that auto-promotes ELC codes, runs a
	// single bulk-details call to hydrate metadata, and returns a
	// tight table — fixing the file-discovery context bloat documented
	// in fastio_find.go's package comment.
	if fastIOServerEnabled(mcpServerTools) {
		allTools = append(allTools, &ceilingGuardedTool{
			inner: tools.NewFastIOUploadFileTool(mcpClient),
			orch:  orch,
		})
		allTools = append(allTools, &ceilingGuardedTool{
			inner: tools.NewFastIOFindTool(mcpClient),
			orch:  orch,
		})
	}

	mcpRegistered := 0
	mcpSkippedOptional := 0
	mcpSkippedAllowlist := 0
	for _, st := range mcpServerTools {
		// Gate 1: Optional servers only pass if the conversation opted in.
		if optionalServers[st.ServerName] && !optInSet[st.ServerName] {
			mcpSkippedOptional++
			continue
		}
		// Gate 2: Per-server tool allowlist.
		if list, ok := allow[st.ServerName]; ok && len(list) > 0 && !slices.Contains(list, st.Tool.Name) {
			mcpSkippedAllowlist++
			continue
		}
		// mcpTool already checks ceilings + email safety internally.
		allTools = append(allTools, &mcpTool{
			serverName: st.ServerName,
			tool:       st.Tool,
			mcpClient:  mcpClient,
			orch:       orch,
		})
		mcpRegistered++
	}

	log.Printf("Fantasy tools registered: %d (%d native − %d native skipped optional + %d MCP, %d MCP skipped optional, %d MCP skipped allowlist)",
		len(allTools), len(nativeTools), nativeSkippedOptional, mcpRegistered, mcpSkippedOptional, mcpSkippedAllowlist)

	if len(allTools) > maxToolsPerRequest {
		return nil, fmt.Errorf("registered %d tools, exceeds the %d-tool ceiling",
			len(allTools), maxToolsPerRequest)
	}

	return allTools, nil
}

// ceilingGuardedTool wraps a native tool and blocks with a clear error
// when the orchestration state reports cost/token ceilings hit. The inner
// tool runs otherwise unchanged.
type ceilingGuardedTool struct {
	inner fantasy.AgentTool
	orch  *orchestrationState
}

func (g *ceilingGuardedTool) Info() fantasy.ToolInfo { return g.inner.Info() }
func (g *ceilingGuardedTool) ProviderOptions() fantasy.ProviderOptions {
	return g.inner.ProviderOptions()
}
func (g *ceilingGuardedTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	g.inner.SetProviderOptions(opts)
}

func (g *ceilingGuardedTool) Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if blocked, msg := g.orch.checkCeilings(); blocked {
		return fantasy.NewTextErrorResponse(msg), nil
	}
	// Risky native bash commands (git push, system package-manager
	// actions) are staged for human approval. Same contract as
	// send_email: return a blocking response so the model stops
	// retrying.
	toolName := g.inner.Info().Name
	// Repeat-call loop guard: cut off degenerate identical-call loops
	// before they execute (and before they stage yet another approval).
	if blocked, msg := g.orch.checkRepeatedCall(toolName, params.Input); blocked {
		return fantasy.NewTextErrorResponse(msg), nil
	}
	if blocked, msg := g.orch.checkBashSafety(toolName, params.ID, params.Input); blocked {
		return fantasy.NewTextErrorResponse(msg), nil
	}
	// preview_email is ALWAYS staged — it has no execution path of
	// its own, the approval card is the feature.
	if blocked, msg := g.orch.checkPreviewEmailSafety(toolName, params.ID, params.Input); blocked {
		return fantasy.NewTextErrorResponse(msg), nil
	}
	// propose_memory is intercepted to create a pending proposal and
	// emit an SSE event so the UI can show a Save/Don't Save card.
	if blocked, msg := g.orch.checkMemoryProposal(toolName, params.Input); blocked {
		return fantasy.NewTextErrorResponse(msg), nil
	}
	// suggest_advanced_model is ALWAYS intercepted — same shape as
	// preview_email but the staged card prompts a model switch instead
	// of an email preview. The stager owns the per-conversation gate.
	if blocked, msg := g.orch.checkSuggestAdvancedSafety(toolName, params.Input); blocked {
		return fantasy.NewTextErrorResponse(msg), nil
	}
	return g.inner.Run(ctx, params)
}

// sanitizeSchemaProperties deep-copies a JSON-schema "properties" map and
// strips any `pattern` entries using `\p{…}` Unicode property escapes, which
// OpenAI's function-calling validator rejects (ECMA-262 only).
func sanitizeSchemaProperties(props map[string]any) map[string]any {
	out := make(map[string]any, len(props))
	for k, v := range props {
		out[k] = sanitizeSchemaValue(v)
	}
	return out
}

func sanitizeSchemaValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		clone := make(map[string]any, len(t))
		for k, vv := range t {
			if k == "pattern" {
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

// ── MCP tool (crush pattern) ──

type mcpTool struct {
	serverName      string
	tool            mcp.Tool
	mcpClient       *mcp.Client
	orch            *orchestrationState
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
	}
}

func (m *mcpTool) Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	toolName := m.Name()

	// Cost + token ceilings. Checked BEFORE email safety so that a runaway
	// loop doesn't keep staging approvals either.
	if blocked, msg := m.orch.checkCeilings(); blocked {
		return fantasy.NewTextErrorResponse(msg), nil
	}

	// Repeat-call loop guard: same contract as the native wrapper above.
	if blocked, msg := m.orch.checkRepeatedCall(toolName, params.Input); blocked {
		return fantasy.NewTextErrorResponse(msg), nil
	}

	// Email-rate-limit / dedup + human approval gate.
	if blocked, msg := m.orch.checkEmailSafety(toolName, params.ID, params.Input); blocked {
		return fantasy.NewTextErrorResponse(msg), nil
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(params.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	result, err := m.mcpClient.CallToolOn(ctx, m.serverName, m.tool.Name, args)
	if err != nil {
		m.orch.recordToolResult(toolName, params.Input, "", false)
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

	m.orch.recordToolResult(toolName, params.Input, resultText, true)
	return fantasy.NewTextResponse(resultText), nil
}

func (m *mcpTool) ProviderOptions() fantasy.ProviderOptions {
	return m.providerOptions
}

func (m *mcpTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	m.providerOptions = opts
}
