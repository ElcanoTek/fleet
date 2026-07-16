package agentcore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"runtime/debug"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/safe"
)

// Tool-dispatch panic containment (#795).
//
// fantasy v0.35's Agent.Stream buffers streamed tool calls and executes them in
// a coordinator goroutine — plus additional WaitGroup goroutines for parallel
// tools — and calls AgentTool.Run with NO recover. Those goroutines are
// unsupervised: a panic in any tool (native, loader, pre-gated, direct-MCP,
// deferred-MCP), in a policy gate, in a redaction/guardrail pass, or in an
// Observer callback would escape and terminate the entire fleet process — the
// one thing internal/safe exists to prevent, but which safe.Recover in the
// runner/httpapi caller goroutines CANNOT catch (recover is goroutine-local).
//
// This wraps every tool fleet hands fantasy in an OUTERMOST panic-contained
// wrapper. A panic becomes exactly one in-band tool_result error (err == nil,
// so fantasy pairs one result to the call ID and the round continues instead of
// aborting the whole stream), is recorded via safe.EmitPanic (PanicCounts +
// Sentry + panic_events) with tool/mode/incident attribution, and is
// conservatively treated as a possibly-committed execution (the model is told
// not to blindly repeat it; the existing ADR-0035 toolEvents gate already
// suppresses in-round provider re-drive once a tool ran). The panic value and
// stack never reach the model — only a stable incident id.
//
// The wrapper is OUTERMOST on purpose: it sits outside policyGuardedTool /
// guardrailOnlyTool / mcpTool, so a panic in a policy gate or guardrail is
// contained too. #788's lifecycle hooks (when they land) run INSIDE this
// wrapper, never outside it.

// panicContainedTool wraps an AgentTool so a panic in its Run (or anything it
// calls) is converted to an in-band error instead of escaping the goroutine.
type panicContainedTool struct {
	inner  fantasy.AgentTool
	name   string // cached at construction so the recovery path never re-enters inner.Info()
	policy Policy
	mode   string // run mode as DATA (never branched on — TestSeamPurity)
	label  string
}

func (p *panicContainedTool) Info() fantasy.ToolInfo { return p.inner.Info() }
func (p *panicContainedTool) ProviderOptions() fantasy.ProviderOptions {
	return p.inner.ProviderOptions()
}
func (p *panicContainedTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	p.inner.SetProviderOptions(opts)
}

func (p *panicContainedTool) Run(ctx context.Context, params fantasy.ToolCall) (resp fantasy.ToolResponse, err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		incident := newIncidentID()
		boundary := "tool"
		val := r
		var stack []byte
		if bp, ok := r.(boundaryPanic); ok {
			boundary, val, stack = bp.boundary, bp.val, bp.stack
		} else {
			stack = debug.Stack()
		}
		safe.EmitPanic(
			"agentcore.tool_dispatch."+boundary,
			fmt.Sprintf("tool=%s call_id=%s mode=%s label=%s incident=%s: %v", p.name, params.ID, p.mode, p.label, incident, val),
			stack,
		)
		// Best-effort accounting so the transcript stays paired; RecordToolResult
		// itself may be the panicker, so guard it with its own recover.
		if p.policy != nil {
			func() {
				defer func() { _ = recover() }()
				// ok=false: a panicked call is a FAILED call for policy accounting.
				p.policy.RecordToolResult(p.name, params.Input, "tool panicked (incident "+incident+")", false)
			}()
		}
		resp = fantasy.NewTextErrorResponse(fmt.Sprintf(
			"tool %s failed with an internal error (reference %s). Treat this call as possibly executed — do NOT blindly repeat side-effecting work; verify state first if the tool may have mutated something.",
			p.name, incident))
		err = nil // in-band: fantasy must pair one result to the call and continue
	}()
	return p.inner.Run(ctx, params)
}

// newIncidentID returns a short random correlation id for a contained panic.
// crypto/rand (not math/rand) keeps it clear of the gosec G404 lint and needs
// no seeding; a read failure degrades to a fixed marker rather than panicking
// inside the recovery path.
func newIncidentID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// boundaryPanic annotates a panic with the seam it originated in (policy gate,
// guardrail, …) and captures the stack AT that boundary — a re-panic from a
// defer otherwise loses the original frames. atBoundary wraps a seam call so
// the outer wrapper attributes the incident to the right location.
type boundaryPanic struct {
	boundary string
	val      any
	stack    []byte
}

// atBoundary runs fn, and on panic re-panics with a boundaryPanic carrying the
// boundary label + the stack captured here. The panicContainedTool defer
// unwraps it for attribution.
func atBoundary(boundary string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			panic(boundaryPanic{boundary: boundary, val: r, stack: debug.Stack()})
		}
	}()
	fn()
}

// containToolPanics wraps each tool in the outermost panic-containment wrapper.
func containToolPanics(ts []fantasy.AgentTool, policy Policy, mode, label string) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, 0, len(ts))
	for _, t := range ts {
		out = append(out, &panicContainedTool{inner: t, name: t.Info().Name, policy: policy, mode: mode, label: label})
	}
	return out
}

// ContainToolPanics wraps tools registered OUTSIDE buildFantasyTools (e.g. the
// interactive leaked-tool-call retry agent, which builds a fresh fantasy.Agent
// over the raw native tools). Uses no policy and a generic mode label.
func ContainToolPanics(ts []fantasy.AgentTool) []fantasy.AgentTool {
	return containToolPanics(ts, nil, "interactive-aux", "")
}
