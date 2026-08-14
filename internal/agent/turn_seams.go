package agent

import (
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// ApprovalStager is the narrow interface the interactive policy adapter uses
// to stage a critical tool call for user approval. RunTurn wires an
// implementation that persists to the approvals table and emits an SSE event
// on the live turn.
type ApprovalStager interface {
	// Stage records a pending approval for a tool call. toolCallID is the
	// agent-assigned id of the tool_call event in conversation history, so the
	// post-approval resolver can write the real result back under the same id.
	// Empty toolCallID remains allowed for native callers that do not have one.
	Stage(toolName, toolCallID, rawInput string) (approvalID string, err error)

	// StageSuggestion stages a suggest_advanced_model approval if the
	// per-conversation gate allows it. approvalID is empty when the suggestion is
	// suppressed; msg always explains the hand-off or suppression to the agent.
	StageSuggestion(reason string) (approvalID, msg string, err error)
}

// TurnMCPScope is the per-turn credential context an ApprovalStager needs to
// stage a call against the seat the turn is ACTUALLY running on (#167
// residual 2). Broker/Catalog are the turn scope's own call seam and public
// catalog — the process-wide default-seat pair would resolve a named-account
// tool name to nothing and pre-validate on the wrong credentials. Selection is
// the public {server, account} list the scope was opened with, so staging can
// record the seat a later approval must reopen.
type TurnMCPScope struct {
	Broker    agentcore.MCPBroker
	Catalog   []mcp.ServerTool
	Selection agentcore.MCPSelection
}

// MCPScopeBinder is the optional half of ApprovalStager. RunTurn calls it once
// the per-turn MCP scope is open, before any tool can stage a card. A stager
// that does not implement it keeps the previous behaviour (the manager's
// unscoped default-seat broker).
type MCPScopeBinder interface {
	BindTurnMCPScope(scope TurnMCPScope)
}

// MemoryProposer is the narrow interface the interactive policy adapter uses
// to stage a memory proposal for user confirmation.
type MemoryProposer interface {
	Propose(content, kind string) (proposalID string, err error)
}
