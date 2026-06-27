package httpapi

import (
	"context"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// turnEngine is the interactive agent engine the HTTP layer drives. It is the
// contract between this transport layer (P6a: routing, SSE, persistence) and
// the unified agent runtime that the fleet binary (cmd/fleet) wires up.
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
	// ProviderHealth returns a snapshot of per-model circuit-breaker state for
	// the LLM provider health endpoint + degraded /healthz (#267).
	ProviderHealth() []agentcore.ModelHealth
}

// ErrModelSelectionRequired is the sentinel a turnEngine implementation returns
// from RunTurn when the chosen model failed in a way the user can fix by
// picking a different model. The HTTP layer detects it with errors.Is: on a
// match it does NOT emit a generic turn.error, because the engine has already
// emitted the structured turn.model_required event carrying the reason and the
// failed model slug. Aliased to the concrete engine's sentinel so errors.Is
// matches across the package boundary.
var ErrModelSelectionRequired = agent.ErrModelSelectionRequired

// The per-turn / summarize value types live in the agent package (the concrete
// engine builds and returns them). The HTTP layer references them via these
// aliases so server.go / summarize.go keep their unqualified names and the
// turnEngine interface and *agent.Manager agree on a single set of types.
type (
	// TurnInput carries per-turn inputs from the HTTP layer to the engine.
	TurnInput = agent.TurnInput
	// TurnResult is returned after a turn completes.
	TurnResult = agent.TurnResult
	// SummarizeInput carries the inputs the summarize endpoint needs.
	SummarizeInput = agent.SummarizeInput
	// SummarizeResult is what the summarize endpoint returns.
	SummarizeResult = agent.SummarizeResult
)
