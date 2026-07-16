package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/observability"
	"github.com/ElcanoTek/fleet/internal/tools"
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
	// personaName/personaPolicy/observer carry Gate-4 (#294) into the tool build
	// so the persona allowlist is applied to the deferrable MCP set BEFORE the
	// disclosure decision (#570) — the roster-level resolvePersonaTools pass in
	// run.go cannot see a tool that deferred behind the bridges. nil
	// personaPolicy = no narrowing. observer receives the persona_tool_blocked
	// audit events for tools suppressed here; nil-safe.
	personaName   string
	personaPolicy *PersonaToolPermissions
	observer      Observer
	// panicAttribution is copied into the universal dispatch wrapper for every
	// advertised route and into deferred logical MCP tools before registration.
	panicAttribution panicAttribution
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
	allTools := make([]fantasy.AgentTool, 0, len(nativeTools)+len(mcpServerTools)+len(cfg.loaderTools)+1)

	for _, t := range nativeTools {
		allTools = append(allTools, &policyGuardedTool{inner: t, policy: policy})
	}
	for _, t := range cfg.loaderTools {
		allTools = append(allTools, &policyGuardedTool{inner: t, policy: policy})
	}
	// The MCP tools that pass gates 1+2 — these are the DEFERRABLE set (#506):
	// registered directly when the roster fits, or hidden behind the disclosure
	// bridges when it would blow the ceiling.
	mcpTools := make([]fantasy.AgentTool, 0, len(mcpServerTools))
	mcpSkippedOptional := 0
	mcpSkippedAllowlist := 0
	mcpSkippedPersona := 0
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
		// Gate 3 (per-task credential allowlist, #184) is enforced at the MCPBroker
		// seam (gateMCPBrokerWithAllowlist), not here — the broker is the single
		// seam every MCP call routes through, so the allowlist holds for every
		// caller. The tool is advertised; a denied call is refused at dispatch with
		// a governance message.
		mt := &mcpTool{
			serverName: st.ServerName,
			tool:       st.Tool,
			broker:     broker,
			policy:     policy,
		}
		// Gate 4 (persona tool allowlist, #294): applied HERE — to the logical
		// mcp_<server>_<tool> identity, before the disclosure decision below —
		// and not only over the registered roster (run.go), because above the
		// disclosure threshold (#506) these tools defer behind the bridge tools
		// and never appear in that roster, so a roster-only filter would
		// silently stop governing them (#570). Filtering before deferral keeps
		// a denied tool out of the deferred registry entirely: it is neither
		// registered directly nor discoverable/callable via
		// tool_search/tool_describe/tool_call — disclosure changes visibility,
		// not governance.
		if cfg.personaPolicy != nil && !cfg.personaPolicy.empty() {
			if suppressed, reason := personaBlocksTool(*cfg.personaPolicy, mt.Name()); suppressed {
				emitPersonaToolBlocked(cfg.observer, cfg.personaName, mt.Name(), reason)
				mcpSkippedPersona++
				continue
			}
		}
		mcpTools = append(mcpTools, mt)
	}

	confirmAudit := []fantasy.AgentTool{}
	if cfg.includeConfirmAudit {
		confirmAudit = append(confirmAudit, &policyGuardedTool{inner: buildConfirmAuditPolicyTool(policy), policy: policy})
	}

	// Progressive tool disclosure (#506): core (already in allTools) + confirm
	// audit are never deferred. If registering every MCP tool directly would
	// exceed the disclosure threshold, hide them behind the three bridge tools
	// backed by a BM25 index instead — removing the hard 128-tool ceiling and
	// cutting per-turn schema tokens. Small catalogs are unchanged (deferral
	// only triggers above the threshold).
	threshold := disclosureThreshold()
	directTotal := len(allTools) + len(mcpTools) + len(confirmAudit)
	if directTotal > threshold && len(mcpTools) > 0 {
		// Deferred MCP calls execute inside the advertised tool_call bridge. Wrap
		// each LOGICAL tool here as well as wrapping the final bridge roster below,
		// so an MCP panic is attributed and accounted to mcp_<server>_<tool>, not
		// only to the disclosure plumbing. containToolRoster is idempotent.
		reg := newDeferredToolRegistry(containToolRoster(mcpTools, cfg.panicAttribution, policy))
		bridges := reg.bridgeTools()
		for _, b := range bridges {
			guarded := &policyGuardedTool{inner: b, policy: policy}
			if _, isCallBridge := b.(*deferredToolCall); isCallBridge {
				// The call bridge returns either a Fleet-generated correction or the
				// verbatim response of a logical MCP tool that already crossed its
				// redaction/PII/guardrail boundary. Screening that response again is
				// not merely redundant: if the screen itself panicked, it would turn
				// one logical incident into a second bridge incident and replace the
				// original reference. The structural type assertion cannot be forged
				// by tool response metadata.
				guarded.outputAlreadyGoverned = true
			}
			allTools = append(allTools, guarded)
		}
		allTools = append(allTools, confirmAudit...)
		log.Printf("Fantasy tools registered: %d (%d native + %d loader + %d bridges; %d MCP tools DEFERRED behind tool_search/describe/call [#506], %d MCP skipped optional, %d MCP skipped allowlist, %d MCP skipped persona)",
			len(allTools), len(nativeTools), len(cfg.loaderTools), len(bridges), len(mcpTools), mcpSkippedOptional, mcpSkippedAllowlist, mcpSkippedPersona)
		if len(allTools) > maxToolsPerRequest {
			return nil, fmt.Errorf("registered %d core+bridge tools, exceeds the %d-tool ceiling even after deferral", len(allTools), maxToolsPerRequest)
		}
		return containToolRoster(allTools, cfg.panicAttribution, policy), nil
	}

	allTools = append(allTools, mcpTools...)
	allTools = append(allTools, confirmAudit...)

	log.Printf("Fantasy tools registered: %d (%d native + %d loader + %d MCP, %d MCP skipped optional, %d MCP skipped allowlist, %d MCP skipped persona)",
		len(allTools), len(nativeTools), len(cfg.loaderTools), len(mcpTools), mcpSkippedOptional, mcpSkippedAllowlist, mcpSkippedPersona)

	if len(allTools) > maxToolsPerRequest {
		return nil, fmt.Errorf("registered %d tools, exceeds the %d-tool ceiling", len(allTools), maxToolsPerRequest)
	}
	return containToolRoster(allTools, cfg.panicAttribution, policy), nil
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
	inner                 fantasy.AgentTool
	policy                Policy
	outputAlreadyGoverned bool
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
		setToolPanicPhase(ctx, panicPhasePolicyBefore)
		if blocked, msg := g.policy.BeforeToolCall(name, params.ID, params.Input); blocked {
			msg, _ = governToolOutput(ctx, name, msg)
			return fantasy.NewTextErrorResponse(msg), nil
		}
	}
	setToolPanicPhase(ctx, panicPhaseToolExecute)
	resp, err := g.inner.Run(ctx, params)
	policyResult := resp.Content
	// Scrub secrets from tool output at the choke point: the redacted text is what
	// re-enters the model context next turn, what RecordToolResult/the policy sees,
	// what the stream sink records, and what the session log persists.
	if resp.Content != "" && !g.outputAlreadyGoverned {
		var outputBlocked bool
		resp.Content, outputBlocked = governToolOutput(ctx, name, resp.Content)
		if outputBlocked {
			resp.IsError = true
		}
		policyResult = resp.Content
	}
	// Fantasy turns a non-nil Go error into model/transcript text AFTER Run
	// returns. Never let a tool-supplied error object cross that boundary: Error
	// may contain credentials or may itself panic. Evaluate and normalize it
	// while the universal outer recovery point can still synthesize one paired
	// incident response.
	if err != nil {
		setToolPanicPhase(ctx, panicPhaseOutputRedact)
		errText := err.Error()
		errText, _ = governToolOutput(ctx, name, errText)
		if errText == "" {
			errText = "Tool execution failed without an error message."
		}
		err = errors.New(errText)
		policyResult = errText
	}
	if g.policy != nil {
		// Record the outcome so policies that gate on tool RESULTS observe native
		// tool calls (bash/python/task_tracker/...), not just the MCP and
		// built-in MCP tools. Without this the scheduled task-tracker finish gate
		// (latestTaskTracker.Seen) never fired in production. A transport error or
		// an is-error response counts as a failed call.
		recordPolicyToolResult(ctx, g.policy, name, params.Input, policyResult, err == nil && !resp.IsError)
	}
	return resp, err
}

// governToolOutput is the one text-output choke point for native, loader, and
// direct MCP tools. It applies secret and optional PII screening, the workspace
// guardrail, and the byte ceiling before text can become model context,
// transcript/SSE data, or policy accounting. The caller's outer
// panicContainedTool attributes and contains a panic in any configurable pass.
func governToolOutput(ctx context.Context, toolName, text string) (string, bool) {
	if text == "" {
		return "", false
	}
	setToolPanicPhase(ctx, panicPhaseOutputRedact)
	text = toolRedactor().Redact(text)
	var piiBlocked bool
	text, piiBlocked = redactPII(toolName, text)
	setToolPanicPhase(ctx, panicPhaseOutputGuardrail)
	var guardrailBlocked bool
	text, guardrailBlocked = screenToolOutput(ctx, toolName, text)
	if ceil := maxToolOutputBytes(); ceil > 0 && len(text) > ceil {
		orig := len(text)
		text, _ = applyOutputCeiling(text, ceil)
		log.Printf("agentcore: truncated %s output from %d to %d bytes (FLEET_MAX_TOOL_OUTPUT_BYTES)", toolName, orig, len(text))
	}
	return text, piiBlocked || guardrailBlocked
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
		setToolPanicPhase(ctx, panicPhasePolicyBefore)
		if blocked, msg := m.policy.BeforeToolCall(toolName, params.ID, params.Input); blocked {
			msg, _ = governToolOutput(ctx, toolName, msg)
			return fantasy.NewTextErrorResponse(msg), nil
		}
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(params.Input), &args); err != nil {
		msg, _ := governToolOutput(ctx, toolName, fmt.Sprintf("invalid arguments: %v", err))
		return fantasy.NewTextErrorResponse(msg), nil
	}

	// The broker owns the call itself — the fast.io inline-upload guard, the
	// transport against the credentialed client, the content flatten, and the
	// fast.io response trim. mcpTool keeps only the per-call framing.
	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	// Sentry breadcrumb (#193): a trail of every MCP call so a captured
	// exception's Sentry event shows the agent's tool history. Tool ARGS are
	// deliberately NOT attached (they may carry connector params); the
	// BeforeSend hook is the last-line scrubber, but breadcrumbs stay lean.
	// No-op when FLEET_SENTRY_DSN is unset.
	observability.AddBreadcrumb(callCtx, "mcp", "mcp call: "+toolName, nil)
	setToolPanicPhase(ctx, panicPhaseToolExecute)
	resultText, isErr, err := m.broker.CallMCP(callCtx, m.serverName, m.tool.Name, args)
	if err != nil {
		setToolPanicPhase(ctx, panicPhaseOutputRedact)
		// Evaluate Error directly while the universal MCP wrapper can recover.
		// fmt's %v intentionally catches a panicking Error method and returns a
		// %!v(PANIC=...) string, which would smuggle the raw panic value through
		// output governance without emitting an incident.
		errText := "Error calling " + toolName + ": " + err.Error()
		errText, _ = governToolOutput(ctx, toolName, errText)
		m.record(ctx, toolName, params.Input, errText, false)
		return fantasy.NewTextErrorResponse(errText), nil
	}
	// Fast.io returns short-lived bearer URLs from download.file-url/zip-url.
	// Vault them before the generic secret scrubber sees `token=...`: redaction
	// alone makes the documented mcp_fast_io_download -> download_url chain
	// unusable, while passing the raw token through would leak direct file access
	// into provider context and logs. The model receives only an opaque handle;
	// download_url redeems it inside this process.
	if !isErr && m.serverName == mcpServerFastIO && m.tool.Name == "download" {
		resultText = tools.ProtectFastIODownloadURLs(resultText)
	}
	// Scrub/screen/cap MCP output before it is recorded, returned to the model,
	// or streamed/persisted downstream.
	var outputBlocked bool
	resultText, outputBlocked = governToolOutput(callCtx, toolName, resultText)

	// Map MCP isError to a fantasy error response so both the LLM and the log
	// know the call failed (per MCP 2025-06-18 spec, tool-level errors arrive as
	// a successful JSON-RPC response with isError=true). The fast.io guard above
	// also surfaces through this path (it returns isError=true with the hint text).
	if isErr || outputBlocked {
		errText := resultText
		if errText == "" {
			errText = fmt.Sprintf("MCP tool %s returned isError=true with no text content", toolName)
		}
		m.record(ctx, toolName, params.Input, errText, false)
		return fantasy.NewTextErrorResponse(errText), nil
	}

	m.record(ctx, toolName, params.Input, resultText, true)
	return fantasy.NewTextResponse(resultText), nil
}

func (m *mcpTool) record(ctx context.Context, toolName, rawInput, resultText string, succeeded bool) {
	recordPolicyToolResult(ctx, m.policy, toolName, rawInput, resultText, succeeded)
}

func (m *mcpTool) ProviderOptions() fantasy.ProviderOptions     { return m.providerOptions }
func (m *mcpTool) SetProviderOptions(o fantasy.ProviderOptions) { m.providerOptions = o }
