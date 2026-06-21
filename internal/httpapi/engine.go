package httpapi

import (
	"context"
	"errors"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// turnEngine is the interactive agent engine the HTTP layer drives. It is the
// contract between this transport layer (P6a: routing, SSE, persistence) and
// the unified agent runtime that the Mega Box binary (cmd/fleet, P6b) wires up.
//
// Background: chat's HTTP server was written against chat's *agent.Manager,
// which carried the entire interactive turn loop as methods. The fleet
// migration's P3 phase rebuilt internal/agent as a unified runtime over
// agentcore.Run, deliberately deferring "the interactive turn-loop's SSE
// streaming, store persistence, and approval staging" to "the httpapi/store
// layers (P6)" (see internal/agent/interactive.go). That left the
// per-event SSE/history bridge — the part chat's session.go::RunTurn owned —
// without a home in the agent package.
//
// turnEngine restores that boundary as an interface this package consumes.
// cmd/fleet (P6b) supplies the concrete implementation that drives
// agentcore.Run / agent.RunInteractiveTurn and forwards fantasy's streaming
// callbacks into an EventSink while accumulating the persisted history. The
// HTTP handlers in this package depend only on this interface, so the server
// compiles and its DB-backed + mock-mode test suites run with a nil engine
// (the live turn path is only exercised by skipped integration/live tests).
//
// MCPServerCatalog and ListPersonas are already provided by the fleet
// agent.Manager; RunTurn / Summarize / SuggestTitle / MCPClient / SandboxPool
// are the interactive-engine surface P6b binds.
type turnEngine interface {
	// RunTurn executes one interactive turn: it streams events through the
	// sink and returns the tail of history to persist plus per-turn usage.
	RunTurn(ctx context.Context, in TurnInput, sink agent.EventSink) (*TurnResult, error)
	// Summarize runs the summarize-and-continue compaction call.
	Summarize(ctx context.Context, in SummarizeInput) (*SummarizeResult, error)
	// SuggestTitle generates a short sidebar title for the opening exchange.
	// Returns "" on any failure (best-effort; never fails a turn).
	SuggestTitle(ctx context.Context, userMessage, assistantReply string) string
	// MCPClient exposes the shared MCP client for the out-of-band approval
	// execution path (runStagedTool).
	MCPClient() *mcp.Client
	// SandboxPool exposes the per-turn sandbox warm pool for the out-of-band
	// approved-bash execution path (runStagedBash).
	SandboxPool() *sandbox.Pool
	// MCPServerCatalog returns the Optional MCP server catalog for the
	// settings UI (already implemented by agent.Manager).
	MCPServerCatalog() []agent.OptionalServerInfo
	// ListPersonas returns the available persona names (already implemented
	// by agent.Manager).
	ListPersonas() ([]string, error)
}

// ErrModelSelectionRequired is the sentinel a turnEngine implementation returns
// from RunTurn when the chosen model failed in a way the user can fix by
// picking a different model. The HTTP layer detects it with errors.Is: on a
// match it does NOT emit a generic turn.error, because the engine has already
// emitted the structured turn.model_required event carrying the reason and the
// failed model slug. Mirrors chat's agent.ErrModelSelectionRequired.
var ErrModelSelectionRequired = errors.New("model selection required")

// TurnInput carries per-turn inputs from the HTTP layer to the engine.
// Mirrors chat's agent.TurnInput.
type TurnInput struct {
	UserMessage string
	Persona     string // persona name, e.g. "victoria"
	// Model is the OpenRouter slug to drive this turn. Required: the server
	// holds no default. A blank or unresolvable slug fails the turn up-front.
	Model   string
	History []agent.HistoryEntry

	// ImageAttachments are user-attached image files for THIS turn only.
	ImageAttachments []agent.ImageAttachment

	// ConversationID scopes per-turn filesystem state to this chat.
	ConversationID string

	// OptionalMCPServersEnabled is the conversation's opt-in list for Optional
	// MCP servers (e.g. gamma). nil/empty means "no optional servers".
	OptionalMCPServersEnabled []string

	// Memories are user-scoped long-term facts injected into the system prompt.
	Memories []string

	// ApprovalStager, when set, intercepts critical tool calls (send_email)
	// and routes them through the approvals table instead of running directly.
	ApprovalStager agent.ApprovalStager

	// MemoryProposer, when set, intercepts propose_memory tool calls and
	// creates pending memory proposals for user confirmation.
	MemoryProposer agent.MemoryProposer

	// Lockdown is set when the conversation row has lockdown=true. Forces a
	// per-turn container sandbox and constrains the resolved model slug to the
	// operator's lockdown allow-list.
	Lockdown bool
}

// TurnResult is returned after a turn completes. Mirrors chat's agent.TurnResult.
type TurnResult struct {
	FinalText           string
	NewHistory          []agent.HistoryEntry // the user msg + any assistant/tool events this turn
	PromptTokens        int
	CompletionTokens    int
	CachedTokens        int
	CacheCreationTokens int
	CostUSD             float64
	// Model is the resolved OpenRouter slug this turn actually ran against.
	Model string
	// Cancelled is true when the turn ended because the caller's ctx was
	// cancelled (Stop button, client disconnect, idle timeout). Partial
	// history and cost are still returned.
	Cancelled bool
}

// SummarizeInput carries the inputs the summarize endpoint needs. Mirrors
// chat's agent.SummarizeInput.
type SummarizeInput struct {
	// History is the full conversation history up to (and not including) any
	// new user message.
	History []agent.HistoryEntry
	// Model is the OpenRouter slug to drive the summarize call.
	Model string
	// Lockdown mirrors TurnInput.Lockdown.
	Lockdown bool
	// OnTextDelta, if non-nil, is invoked for each chunk of summary text the
	// model produces (wired to the SSE stream). Optional.
	OnTextDelta func(text string)
}

// SummarizeResult is what the summarize endpoint returns. Mirrors chat's
// agent.SummarizeResult.
type SummarizeResult struct {
	Text             string
	Model            string
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
}
