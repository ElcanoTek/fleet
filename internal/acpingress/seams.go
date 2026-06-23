package acpingress

import (
	"context"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/store"
)

// The narrow seams the IngressAgent drives. They are the SAME methods the web
// path uses; defining them as interfaces (rather than importing *agent.Manager /
// *store.Store) keeps this package decoupled and lets tests inject a fake engine
// that runs a real governed turn against a fake LLM with no DB.

// TurnEngine runs one governed interactive turn. The production implementation
// is *agent.Manager (the concrete httpapi.turnEngine) — so an ingress turn
// inherits the configured runtime flavor + full governance verbatim. The turn's
// streamed events flow through the EventSink (→ ACP session/update); the human
// approval surface rides the ApprovalStager wired onto the TurnInput.
type TurnEngine interface {
	RunTurn(ctx context.Context, in agent.TurnInput, sink agent.EventSink) (*agent.TurnResult, error)
}

// ConversationStore persists the ACP-session-bound conversation + its history,
// so an ingress turn's audit / notes / history live in the SAME tables the web
// path uses. The production implementation is *store.Store. ConversationID-keyed
// methods scope per-session state; userEmail is the bound Principal's identity.
type ConversationStore interface {
	// CreateConversation creates the fleet conversation bound to an ACP session.
	CreateConversation(ctx context.Context, userEmail, title, persona, model string, lockdown bool) (*store.Conversation, error)
	// Get returns the conversation by id, scoped to userEmail (nil,nil when absent).
	// The conversation row IS the durable ACP-session binding — the SessionId equals
	// conv.ID — so this is how a reconnect after a `fleet acp` restart rehydrates a
	// session (LoadSession / ResumeSession / a post-restart Prompt).
	Get(ctx context.Context, userEmail, convID string) (*store.Conversation, error)
	// LoadHistory returns the conversation's persisted history for replay.
	LoadHistory(ctx context.Context, convID string) ([]agent.HistoryEntry, error)
	// AppendHistory persists this turn's new history entries (atomic batch).
	AppendHistory(ctx context.Context, convID string, entries []agent.HistoryEntry) error
}

// ApprovalStore is the subset of the approvals table the ingress approval
// surface touches: create a pending row (the audit record), then claim it
// approved/rejected once the human decides over ACP, and record the staged
// tool's outcome. The production implementation is *store.Store.
type ApprovalStore interface {
	// CreateApproval stages a pending approval row (the audit record).
	CreateApproval(ctx context.Context, convID, userEmail, toolName, toolCallID, argsJSON string) (*store.Approval, error)
	// ClaimApproval atomically flips a pending approval to approved/rejected.
	// Returns false when it was already resolved (lost race / double-decide).
	ClaimApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) (bool, error)
	// SetApprovalResult records the staged tool's outcome post-execution.
	SetApprovalResult(ctx context.Context, userEmail, approvalID, resultText string) error
	// AppendHistory writes the resolution tool_result into the conversation so
	// the next turn's model sees the outcome (mirrors the web approval handler).
	AppendHistory(ctx context.Context, convID string, entries []agent.HistoryEntry) error

	// CreateMemoryProposal stages a propose_memory proposal (pending user
	// confirmation). AcceptMemoryProposal accepts it on human Allow over ACP.
	// These back the ingress propose_memory surface (the SAME store + tables the
	// web memory-confirmation path uses). *store.Store implements both.
	CreateMemoryProposal(ctx context.Context, userEmail, conversationID, content string) (*store.Memory, error)
	AcceptMemoryProposal(ctx context.Context, userEmail, id string) (*store.Memory, error)
}

// StagedToolRunner executes an approved staged tool one-shot, outside the agent
// loop, through the SAME governed primitives the web approval handler uses
// (MCP via the prefixed client, native bash via the warm sandbox pool). The
// production implementation is the package's stagedToolRunner over a Toolbox
// backed by *agent.Manager.
type StagedToolRunner interface {
	RunStagedTool(ctx context.Context, approval *store.Approval) (string, error)
}

// Toolbox exposes the live MCP client + sandbox pool the staged-tool runner
// needs to resolve an approval through the SAME container/credential boundary an
// agent-driven call would cross. *agent.Manager satisfies it (MCPClient +
// SandboxPool). It is a narrow seam so the runner — and its tests — never depend
// on the whole Manager.
type Toolbox interface {
	// MCPClient returns the shared, host-credentialed MCP client (nil in mock
	// mode). The runner routes mcp_<server>_<tool> calls through it.
	MCPClient() *mcp.Client
	// SandboxPool returns the per-turn warm sandbox pool. The runner takes one
	// to execute an approved native bash command in a container.
	SandboxPool() *sandbox.Pool
}

// Principal is the identity an ingress session binds to for audit attribution.
// `fleet acp` runs as the box user (local-process trust), so there is no
// signed-key auth as on the web path; the operator configures a stable service
// identity (e.g. an email) so the conversation + approvals rows attribute
// correctly. A blank Email falls back to DefaultPrincipalEmail.
type Principal struct {
	// Email is the audit identity for conversations + approvals this session
	// creates. It is NOT an authentication credential — launching the process
	// already implies box-user trust.
	Email string
}

// DefaultPrincipalEmail is the audit identity an ingress session uses when the
// operator did not configure one. It is an obvious, non-routable placeholder so
// audit rows are never silently attributed to a real user.
const DefaultPrincipalEmail = "acp-ingress@fleet.local"
