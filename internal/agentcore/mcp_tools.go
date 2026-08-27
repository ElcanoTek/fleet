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
	"github.com/ElcanoTek/fleet/internal/observability"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// MCP tool wrapping + the ONE buildFantasyTools skeleton both modes feed
// (merged from chat + cutlass fantasy.go).
//
// The Gate-1 opt-in (skip unless the Optional key governing the server is in
// optIn — see the keying rule below) is chat's gate and is byte-identical for
// both modes per the migration ledger: the scheduled producer derives optIn
// from its task's MCPSelection server names, the interactive producer from the
// conversation's opt-in list. Accounts do NOT affect which tools register (that
// is §6.3 wiring); they affect which subprocess/env backs the server — but they
// DO change the name the server registers under, which is why Gate-1 resolves
// its key through longestServerKey instead of an exact map lookup (#1272).
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
// block the agent loop. Var, not const: tests shrink it to prove post-call
// governance survives an expired callCtx.
var toolCallTimeout = 5 * time.Minute

// ── THE ONE SERVER-NAME KEYING RULE (#1272) ──────────────────────────────────
//
// Every server-keyed MCP gate faces the same gap: the gate map is keyed by
// MANIFEST (spec) server name, while the catalog the loop walks — and the
// system prompt's roster of tool names — carries the REGISTERED name, which for
// a named-account seat is "<server>_<account>" (resolveMCPVariant). One rule
// closes that gap for every layer:
//
//	A key K governs a name N iff N == K, or N begins with K+"_".
//	When several keys qualify, the LONGEST one wins.
//
// Longest-wins is what makes the answer deterministic over a Go map range (two
// equal-length prefixes of one name at the same offset are the same string) —
// the property docs/PROMPT-CACHE-CONTRACT.md rests on, because the
// system-prompt roster applies this same rule and a coin-flip winner would
// silently bust the cacheable prefix (#1125). It also attributes a variant
// seat's tools to the variant's OWN key when the bundle declares one, and falls
// back to its base server's key when it does not — which for the Optional set
// means fail-closed: an Optional base gates its variant seats too.
//
// Callers, all of which MUST route through here rather than an exact lookup:
// mcpAllowlist.toolsFor (Gate-2), optionalServerFor / OptionalServerFor
// (Gate-1), and — via OptionalServerForToolName — internal/agent's
// system-prompt roster filter. (The per-task credential allowlist's
// registered-name projection, permittedRegisteredNames, and the persona
// filter's mcp:<server>/* prefix are the same treatment over their own
// shapes.)
//
// wholeName admits the "N == K" branch. It is true for a REGISTERED server
// name, which can legitimately BE a declared server; it is false for a
// `mcp_`-stripped roster name, whose trailing `_<tool>` segment means a
// whole-name hit would be a server+tool coincidence and not the server (the
// roster name `mcp_jira_search` comes from server "jira" tool "search", and
// must not resolve to a server literally named "jira_search" — that server's
// own roster names all carry a further `_<tool>` segment). governs, when
// non-nil, filters out keys whose value does not participate (the Optional set
// stores `false` for a declared-but-not-Optional server).
func longestServerKey[V any](name string, keyed map[string]V, wholeName bool, governs func(V) bool) (string, bool) {
	best, found := "", false
	for key, val := range keyed {
		if governs != nil && !governs(val) {
			continue
		}
		if wholeName && key == name {
			return key, true
		}
		if (!found || len(key) > len(best)) && strings.HasPrefix(name, key+"_") {
			best, found = key, true
		}
	}
	return best, found
}

// mcpAllowlist maps server name → allowed tool names. Empty/missing = allow all.
type mcpAllowlist map[string][]string

// toolsFor returns the allowlist entry governing a REGISTERED server name,
// resolved by the one keying rule above: an exact manifest key wins, and a
// named-account seat "<server>_<account>" otherwise falls back to the longest
// manifest key it extends across an underscore. nil = no entry (allow all).
func (al mcpAllowlist) toolsFor(registered string) []string {
	if key, ok := longestServerKey(registered, al, true, nil); ok {
		return al[key]
	}
	return nil
}

// mcpOptionalSet reports whether a server is Optional (participates only when
// opted in for the run).
type mcpOptionalSet map[string]bool

// optionalTrue is the mcpOptionalSet participation filter for longestServerKey:
// a key mapped to false is declared but not Optional, so it governs nothing.
func optionalTrue(optional bool) bool { return optional }

// optionalServerFor returns the Optional key whose per-run opt-in toggle
// governs a REGISTERED server name, or "" when no Optional key governs it. It
// is the one keying rule above applied to the Gate-1 set: a variant seat is
// governed by its own key when the bundle declares one, and by its BASE
// server's key when it does not — so `jira_prod` (registered from
// {server: jira, account: prod}) is opt-in gated whenever `jira` is Optional,
// instead of slipping past an exact map lookup and registering unconditionally
// while the system prompt hid it (#1272).
func optionalServerFor(registered string, optional mcpOptionalSet) string {
	key, _ := longestServerKey(registered, optional, true, optionalTrue)
	return key
}

// MCPAllowlist / MCPOptionalSet are the exported aliases the DRIVERS use to
// build a RunConfig (the underlying map types are otherwise unexported).
type (
	// MCPAllowlist maps server name → allowed tool names (Gate-2). Empty = all.
	MCPAllowlist = mcpAllowlist
	// MCPOptionalSet reports which servers are Optional (Gate-1 opt-in).
	MCPOptionalSet = mcpOptionalSet
)

// OptionalServerFor is the driver-visible form of the Gate-1 keying rule: given
// a REGISTERED server name it returns the Optional key whose per-run opt-in
// toggle governs it ("" = not Optional). Exported so a driver cannot invent a
// second rule.
func OptionalServerFor(registered string, optional MCPOptionalSet) string {
	return optionalServerFor(registered, optional)
}

// OptionalServerForToolName resolves the Optional key governing a prefixed
// `mcp_<server>_<tool>` roster name — the system-prompt roster's view of the
// same decision Gate-1 makes over the registered server name, so what the model
// SEES and what actually REGISTERS cannot disagree for a variant seat (#1272).
// The `mcp_` prefix is stripped and the remainder resolved by the shared rule
// with the whole-name branch disabled (see longestServerKey).
func OptionalServerForToolName(toolName string, optional MCPOptionalSet) string {
	rest, ok := strings.CutPrefix(toolName, "mcp_")
	if !ok {
		return ""
	}
	key, _ := longestServerKey(rest, optional, false, optionalTrue)
	return key
}

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
	// hooks is the per-run lifecycle-hook engine (#788), nil when no hooks are
	// configured. Set on native/loader/confirm_audit/direct+deferred MCP tools;
	// deliberately NOT on the disclosure bridge wrappers (the deferred tool
	// fires the hook under its real name, so a bridge-level hook would
	// double-fire a "*" matcher).
	hooks *hookEngine
	// journal is the durable turn journal seam (#798), nil when the driver has
	// none (scheduled/evals). Same placement contract as hooks: on the real
	// tools, never the disclosure bridge wrappers, so one call journals once.
	journal TurnJournal
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
		allTools = append(allTools, &policyGuardedTool{inner: t, policy: policy, hooks: cfg.hooks, journal: cfg.journal})
	}
	for _, t := range cfg.loaderTools {
		allTools = append(allTools, &policyGuardedTool{inner: t, policy: policy, hooks: cfg.hooks, journal: cfg.journal})
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
		// between modes. The opt-in key is resolved through the ONE keying rule
		// (optionalServerFor) rather than an exact lookup on the registered
		// name, so a named-account variant seat is gated by its own Optional key
		// when the bundle declares one and by its BASE server's key when it does
		// not. The exact lookup missed the second case entirely: the seat
		// registered (and was callable) while the system-prompt roster — which
		// has always resolved the base key by prefix — hid it from the model
		// (#1272).
		if optionalKey := optionalServerFor(st.ServerName, optionalServers); optionalKey != "" && !optIn[optionalKey] {
			mcpSkippedOptional++
			continue
		}
		// Gate 2: per-server tool allowlist.
		if list := allow.toolsFor(st.ServerName); len(list) > 0 && !slices.Contains(list, st.Tool.Name) {
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
			hooks:      cfg.hooks,
			journal:    cfg.journal,
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
		// Install the final model-visible boundary before the tool enters the
		// deferred registry. That makes direct and tool_call dispatch identical;
		// wrapping only the advertised bridge would leave the hidden tool itself
		// as a future bypass route.
		mcpTools = append(mcpTools, withModelOutputBoundary(mt))
	}

	confirmAudit := []fantasy.AgentTool{}
	if cfg.includeConfirmAudit {
		confirmAudit = append(confirmAudit, &policyGuardedTool{inner: buildConfirmAuditPolicyTool(policy), policy: policy, hooks: cfg.hooks, journal: cfg.journal})
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
		// The model-output boundary sits inside the universal panic boundary: a
		// panic in a tool, policy hook, output screen, artifact stager, or bridge
		// dispatch becomes one paired in-band result, while every ordinary result
		// crosses the hard byte cap before Fantasy can retain it.
		return containToolRoster(BoundModelOutputTools(allTools), cfg.panicAttribution, policy), nil
	}

	allTools = append(allTools, mcpTools...)
	allTools = append(allTools, confirmAudit...)

	log.Printf("Fantasy tools registered: %d (%d native + %d loader + %d MCP, %d MCP skipped optional, %d MCP skipped allowlist, %d MCP skipped persona)",
		len(allTools), len(nativeTools), len(cfg.loaderTools), len(mcpTools), mcpSkippedOptional, mcpSkippedAllowlist, mcpSkippedPersona)

	if len(allTools) > maxToolsPerRequest {
		return nil, fmt.Errorf("registered %d tools, exceeds the %d-tool ceiling", len(allTools), maxToolsPerRequest)
	}
	return containToolRoster(BoundModelOutputTools(allTools), cfg.panicAttribution, policy), nil
}

// buildConfirmAuditPolicyTool returns the scheduled confirm_audit tool wired to
// the policy's underlying orchestration when available, else a no-op stub. The
// scheduled Policy bundle (P3) embeds an orchestrationState; policyOrchestration
// (the one Policy→orchestrationState walk, shared with the resilience layer's
// usage accounting, #1125) unwraps to it so the tool can mutate audit state.
func buildConfirmAuditPolicyTool(policy Policy) fantasy.AgentTool {
	if orch, ok := policyOrchestration(policy); ok {
		return buildConfirmAuditTool(orch)
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
	hooks                 *hookEngine // #788; nil = no hooks (nil-safe methods)
	journal               TurnJournal // #798; nil = no durable journal (scheduled/evals)
}

func (g *policyGuardedTool) Info() fantasy.ToolInfo { return g.inner.Info() }
func (g *policyGuardedTool) ProviderOptions() fantasy.ProviderOptions {
	return g.inner.ProviderOptions()
}
func (g *policyGuardedTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	g.inner.SetProviderOptions(opts)
}

// Run defers the shared gate→journal→execute→govern→bound→record sequence to
// runGovernedToolCall (tool_call_framing.go) and injects only the native
// execute step.
func (g *policyGuardedTool) Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	name := g.inner.Info().Name
	return runGovernedToolCall(ctx, toolCallFraming{
		name:    name,
		policy:  g.policy,
		hooks:   g.hooks,
		journal: g.journal,
		execute: func(ctx context.Context) toolCallOutcome { return g.call(ctx, name, params) },
	}, params)
}

// call runs the wrapped tool plus the native-specific output handling —
// everything between the shared gates and the shared epilogue.
func (g *policyGuardedTool) call(ctx context.Context, name string, params fantasy.ToolCall) toolCallOutcome {
	setToolPanicPhase(ctx, panicPhaseToolExecute)
	resp, err := g.inner.Run(ctx, params)
	if err != nil {
		// Fantasy persists a non-nil Go error as the model-visible tool result.
		// Evaluate it under the outer panic boundary, then govern and cap it here
		// so policy audit, the stream sink, and provider replay all receive the
		// exact same bytes. The shared epilogue wraps cause after bounding, so
		// err and resp.Content stay byte-identical (boundedModelToolError).
		cause := err
		setToolPanicPhase(ctx, panicPhaseOutputRedact)
		errText := cause.Error()
		errText, _ = governToolOutput(ctx, name, errText)
		if errText == "" {
			errText = "Tool execution failed without an error message."
		}
		return toolCallOutcome{resp: fantasy.NewTextErrorResponse(errText), failed: true, cause: cause}
	}
	if resp.Content != "" && !g.outputAlreadyGoverned {
		var outputBlocked bool
		resp.Content, outputBlocked = governToolOutput(ctx, name, resp.Content)
		if outputBlocked {
			resp.IsError = true
		}
	}
	return toolCallOutcome{resp: resp, failed: resp.IsError}
}

// governToolOutput is the one text-governance choke point for native, loader,
// and direct MCP tools. It applies secret and optional PII screening plus the
// workspace guardrail. The separate model-output boundary then renders and caps
// the governed response before it can become model context, transcript/SSE data,
// or policy accounting. The caller's outer panicContainedTool attributes and
// contains a panic in any configurable pass.
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
// (see MCPBroker). The per-call FRAMING — gates, journal, hook append, bounding,
// result recording — is the shared runGovernedToolCall (tool_call_framing.go);
// mcpTool injects only the arg parse, the timeout, and the isError→error
// mapping, while the broker owns the call itself (guard, transport, flatten,
// fast.io trim).
type mcpTool struct {
	serverName      string
	tool            mcp.Tool
	broker          MCPBroker
	policy          Policy
	providerOptions fantasy.ProviderOptions
	hooks           *hookEngine // #788; fires under the real mcp_<server>_<tool> name (also on the deferred route)
	journal         TurnJournal // #798; nil = no durable journal (scheduled/evals)
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

// Run defers the shared gate→journal→execute→govern→bound→record sequence to
// runGovernedToolCall (tool_call_framing.go) and injects the two MCP-specific
// steps: the argument parse (validate) and the brokered call (execute).
func (m *mcpTool) Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	toolName := m.Name()
	var args map[string]any
	return runGovernedToolCall(ctx, toolCallFraming{
		name:    toolName,
		policy:  m.policy,
		hooks:   m.hooks,
		journal: m.journal,
		validate: func() (string, bool) {
			if err := json.Unmarshal([]byte(params.Input), &args); err != nil {
				return fmt.Sprintf("invalid arguments: %v", err), false
			}
			return "", true
		},
		execute: func(ctx context.Context) toolCallOutcome { return m.call(ctx, toolName, params, args) },
	}, params)
}

// call runs the brokered MCP call plus the MCP-specific output handling —
// everything between the shared gates and the shared epilogue. Every failure
// (transport error, isError=true, guardrail block) becomes an error RESPONSE
// with no Go error, so the model can see it and recover (see the divergence
// note in tool_call_framing.go).
func (m *mcpTool) call(ctx context.Context, toolName string, params fantasy.ToolCall, args map[string]any) toolCallOutcome {
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
	// Everything after the call runs on ctx, never callCtx: when CallMCP fails
	// on its own timeout, callCtx is expired by definition, and a dead context
	// would fail-close the guardrail (rewriting a timeout into a fake guardrail
	// block), skip post_tool_use hooks, and abort artifact staging inside
	// boundModelVisibleToolResponse. callCtx scopes the broker call only.
	if err != nil {
		setToolPanicPhase(ctx, panicPhaseOutputRedact)
		// Transport errors are untrusted connector output. Evaluate Error directly
		// while the universal MCP wrapper can recover; fmt's %v would swallow a
		// panicking Error method into a raw %!v(PANIC=...) diagnostic.
		errText := "Error calling " + toolName + ": " + err.Error()
		errText, _ = governToolOutput(ctx, toolName, errText)
		return toolCallOutcome{resp: fantasy.NewTextErrorResponse(errText), failed: true}
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
	resultText, outputBlocked = governToolOutput(ctx, toolName, resultText)

	// Engine-authored date-window / empty-search reminders (#1026). Applied
	// after output governance so the reminder cannot be used to smuggle
	// connector bytes past the scrubber, and before post_tool_use / the
	// model-output boundary so it is capped like every other result.
	if !outputBlocked {
		resultText = AnnotateDateWindow(toolName, params.Input, resultText, runtimeNow())
	}

	// Map MCP isError to a fantasy error response so both the LLM and the log
	// know the call failed (per MCP 2025-06-18 spec, tool-level errors arrive as
	// a successful JSON-RPC response with isError=true). The fast.io guard above
	// also surfaces through this path (it returns isError=true with the hint text).
	if isErr || outputBlocked {
		if resultText == "" {
			resultText = fmt.Sprintf("MCP tool %s returned isError=true with no text content", toolName)
		}
		return toolCallOutcome{resp: fantasy.NewTextErrorResponse(resultText), failed: true}
	}

	return toolCallOutcome{resp: fantasy.NewTextResponse(resultText), failed: false}
}

func (m *mcpTool) ProviderOptions() fantasy.ProviderOptions     { return m.providerOptions }
func (m *mcpTool) SetProviderOptions(o fantasy.ProviderOptions) { m.providerOptions = o }
