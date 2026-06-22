// Package acpruntime makes fleet an ACP CLIENT that drives a fleet of sandboxed
// ACP agents, and provides the agent-side runner that wraps fleet's native
// fantasy loop (agentcore.Run) AS an ACP agent.
//
// Topology (Plan v4):
//
//   - fleet = ACP client (ClientRuntime): spawns each flavor's agent as a
//     subprocess via `podman run -i <image>` (no -t — a PTY corrupts JSON-RPC
//     framing; stderr is split from the protocol stdout), advertises fs +
//     terminal capabilities at initialize, and handles the `_fleet/*` extension
//     methods. The client owns the REAL host-side governance — it runs delegated
//     tool calls in the hardened per-turn sandbox (NO Podman-in-Podman), applies
//     policy/audit/notes/creds, and streams results + Observer events back out.
//
//   - native flavor = cmd/fleet-native-agent (AgentRunner): wraps agentcore.Run.
//     The fantasy loop runs IN the sandbox; it emits session/update for streaming
//     and, for ANY execution, delegates over ACP back to the client. It has NO
//     local executor (its sandbox is a delegating forwarder) so it cannot
//     self-execute — the same shape future external agents take.
//
// The governance seam rides ACP extension methods (`_fleet/*`): bash/python tool
// execution, MCP tool calls, and the structured run events the host needs to
// govern + persist. These wire types are the shared contract between the two
// sides; both import this package so the JSON shapes can never drift.
package acpruntime

// Extension-method names (the `_`-prefixed ACP extension namespace). Both the
// client (ExtensionMethodHandler) and the agent (CallExtension) use these exact
// strings, so they live here once.
const (
	// ExtMethodTool is the agent→client request to EXECUTE a delegated native
	// tool (bash / run_python) in the host-managed sandbox. The client runs it
	// against the real host *sandbox.Sandbox and returns the combined output.
	ExtMethodTool = "_fleet/tool"

	// ExtMethodEvent is the agent→client notification carrying a structured run
	// event the host needs to govern/persist beyond the user-visible
	// session/update text (e.g. enforcement nudges, usage accounting). The
	// client forwards it to fleet's real Observer.
	ExtMethodEvent = "_fleet/event"

	// ExtMethodMCP is the agent→client request to EXECUTE a delegated MCP tool
	// call HOST-SIDE. The agent advertises the same mcp_<server>_<tool> surface
	// the in-process path advertises (descriptors travel in the RunSpec), but the
	// CALL itself rides this seam so the host runs it against the per-task
	// credentialed mcp.Client. MCP credentials NEVER enter the agent container —
	// this is the host-side credential-brokering seam (P-ACP-2b).
	ExtMethodMCP = "_fleet/mcp"

	// ExtMethodStage is the agent→client request to STAGE an approval / memory /
	// note proposal for human confirmation HOST-SIDE. The agent's InteractivePolicy
	// runs in the container (identical governance), but the staging EFFECT (DB
	// write + SSE card) belongs to the host. The agent's delegating stagers ride
	// this seam; the host routes it to the real ApprovalStager / MemoryProposer /
	// NoteProposer.
	ExtMethodStage = "_fleet/stage"
)

// MCPRequest is the agent→client `_fleet/mcp` payload: a single delegated MCP
// tool call. The agent passes the server name + bare tool name + decoded args;
// the host runs mcpClient.CallToolOn against the per-task credentialed client.
type MCPRequest struct {
	SessionID string         `json:"sessionId"`
	Server    string         `json:"server"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// MCPResponse is the client→agent `_fleet/mcp` result. Text is the flattened
// text content blocks (the same view the in-process mcpTool renders); IsError
// mirrors the MCP isError bit so the agent maps it onto a fantasy error response
// exactly as the in-process path would. Error carries a delegation/transport
// failure (the host could not reach the server) distinct from a tool-level
// isError result.
type MCPResponse struct {
	Text    string `json:"text"`
	IsError bool   `json:"isError,omitempty"`
	Error   string `json:"error,omitempty"`
}

// MCPToolDescriptor describes one MCP tool the host wants the agent to advertise.
// It carries everything the agent needs to register a delegating mcp_<server>_<tool>
// tool WITHOUT any credentials: the server + bare tool name, the description, and
// the JSON-schema input shape. Travels in the RunSpec so the agent's tool surface
// matches the in-process path's exactly.
type MCPToolDescriptor struct {
	Server      string         `json:"server"`
	Tool        string         `json:"tool"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// StageKind enumerates the host-side staging effects the agent delegates.
type StageKind string

const (
	// StageApproval stages a critical tool call (send_email / risky bash /
	// preview_email) via the host ApprovalStager.Stage.
	StageApproval StageKind = "approval"
	// StageSuggestion stages a suggest_advanced_model card via
	// ApprovalStager.StageSuggestion (carries the per-conversation gate).
	StageSuggestion StageKind = "suggestion"
	// StageMemory stages a propose_memory proposal via MemoryProposer.Propose.
	StageMemory StageKind = "memory"
	// StageNote stages a propose_note proposal via NoteProposer.Propose.
	StageNote StageKind = "note"
)

// StageRequest is the agent→client `_fleet/stage` payload: a single host-side
// staging effect. Fields are populated per Kind (approval/suggestion use
// ToolName/ToolCallID/RawInput/Reason; memory uses Content; note uses
// Slug/Title/Body/Reason).
type StageRequest struct {
	SessionID  string    `json:"sessionId"`
	Kind       StageKind `json:"kind"`
	ToolName   string    `json:"toolName,omitempty"`
	ToolCallID string    `json:"toolCallId,omitempty"`
	RawInput   string    `json:"rawInput,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Content    string    `json:"content,omitempty"`
	Slug       string    `json:"slug,omitempty"`
	Title      string    `json:"title,omitempty"`
	Body       string    `json:"body,omitempty"`
}

// StageResponse is the client→agent `_fleet/stage` result. ProposalID is the
// host-assigned approval/proposal id (empty when the host suppressed the
// suggestion). Message is the host-supplied agent-facing message (StageSuggestion
// returns one verbatim; empty otherwise). Error carries a staging failure.
type StageResponse struct {
	ProposalID string `json:"proposalId,omitempty"`
	Message    string `json:"message,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ToolKind enumerates the delegated native tools the client can execute.
type ToolKind string

const (
	// ToolBash is a bash command run in the host sandbox.
	ToolBash ToolKind = "bash"
	// ToolPython is a run_python snippet run in the host sandbox kernel.
	ToolPython ToolKind = "python"
)

// ToolRequest is the agent→client `_fleet/tool` payload: a single delegated
// native-tool execution. SessionID scopes it to the ACP session so the client
// can resolve the right host sandbox + governance context.
type ToolRequest struct {
	SessionID string   `json:"sessionId"`
	Tool      ToolKind `json:"tool"`
	// Command is the bash command line (ToolBash).
	Command string `json:"command,omitempty"`
	// Code is the python snippet (ToolPython).
	Code string `json:"code,omitempty"`
	// WorkingDir scopes a bash invocation; WorkspaceDir scopes python.
	WorkingDir   string `json:"workingDir,omitempty"`
	WorkspaceDir string `json:"workspaceDir,omitempty"`
	// TimeoutSeconds caps the execution (0 = client default).
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// ToolResponse is the client→agent `_fleet/tool` result. Output is the combined
// stdout/stderr view the tool layer renders; Error carries an execution failure
// message (the call itself succeeded as a delegation — Error is the tool's own
// failure, surfaced to the model exactly as the in-process path would).
type ToolResponse struct {
	Output   string `json:"output"`
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
	TimedOut bool   `json:"timedOut,omitempty"`
}

// EventNotification is the agent→client `_fleet/event` payload: a structured run
// event the host's Observer should receive (mirrors agentcore.Observer's
// (eventType, payload) shape). The client re-emits it onto fleet's real
// Observer so SSE / the session log see the same events the in-process path
// would emit.
type EventNotification struct {
	SessionID string         `json:"sessionId"`
	EventType string         `json:"eventType"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// EventUsage is the `_fleet/event` eventType the agent emits per LLM step with
// the step's token + cost accounting. The host both re-emits it onto the Observer
// AND accumulates it into the run's usage totals — the agent makes the LLM calls
// (in its container), so usage accrues there and is reported back here. The
// payload uses the usageKey* field names below.
const EventUsage = "usage"

// usageKey* are the EventUsage payload field names (the agent writes them, the
// host reads them). They mirror agentcore.RunUsage's fields so accumulation is a
// straight field copy.
const (
	usageKeyPromptTokens        = "prompt_tokens"
	usageKeyLastStepInputTokens = "last_step_input_tokens"
	usageKeyCompletionTokens    = "completion_tokens"
	usageKeyCachedTokens        = "cached_tokens" //nolint:gosec // G101 false positive: an event-payload field NAME for cached-token counts, not a credential.
	usageKeyCacheCreationTokens = "cache_creation_tokens"
	usageKeyCostUSD             = "cost_usd"
)

// intFromPayload reads an integer field from an event payload, tolerating the
// float64 a JSON round-trip produces (numbers decode as float64 when the payload
// arrived over the wire) and a native int (same-process tests).
func intFromPayload(payload map[string]any, key string) int {
	switch v := payload[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	default:
		return 0
	}
}

// floatFromPayload reads a float field from an event payload (cost), tolerating
// both float64 (wire / native) and int.
func floatFromPayload(payload map[string]any, key string) float64 {
	switch v := payload[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return 0
	}
}
