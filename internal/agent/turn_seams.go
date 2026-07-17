package agent

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

// MemoryProposer is the narrow interface the interactive policy adapter uses
// to stage a memory proposal for user confirmation.
type MemoryProposer interface {
	Propose(content, kind string) (proposalID string, err error)
}
