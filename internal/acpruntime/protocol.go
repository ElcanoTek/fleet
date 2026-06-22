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
)

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
