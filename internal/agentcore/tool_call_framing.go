package agentcore

import (
	"context"
	"strings"

	"charm.land/fantasy"
)

// The ONE shared tool-call governance framing (#1127).
//
// policyGuardedTool.Run (native/loader/bridge tools) and mcpTool.Run (MCP
// tools) historically each carried a full copy of the gate → journal →
// execute → govern → bound → record sequence, including duplicated
// post_tool_use appending. An ordering bug fixed in one copy would have been
// invisible in the other, so the sequence now lives here exactly once and the
// two wrappers inject only their genuine differences (toolCallFraming).
//
// Deliberate divergences the seams preserve (they are not accidental drift):
//
//   - Error returns. A native tool's failure surfaces as a non-nil Go error
//     (boundedModelToolError, so Fantasy persists the exact bounded bytes and
//     errors.Is/As keep working), while an MCP failure — transport error or
//     isError=true — is always mapped to an error RESPONSE with a nil Go
//     error (per MCP 2025-06-18, tool-level errors arrive as data the model
//     should see and recover from).
//   - MCP parses its JSON arguments BETWEEN the policy gate and the intent
//     journal, so an unparseable call is refused without journaling an intent
//     that could never dispatch (the validate seam; native tools have none).

// toolCallFraming injects a tool wrapper's per-type behavior into
// runGovernedToolCall. name/policy/hooks/journal identify the call and its
// governance seams; validate and execute are the two per-type steps.
type toolCallFraming struct {
	name    string
	policy  Policy
	hooks   *hookEngine // #788; nil = no hooks (nil-safe methods)
	journal TurnJournal // #798; nil = no durable journal (scheduled/evals)
	// validate, when non-nil, runs BETWEEN the policy gate and the intent
	// journal. ok=false refuses the call with the returned message, without
	// executing or journaling (mcpTool's argument parse).
	validate func() (refusal string, ok bool)
	// execute runs the tool call itself and applies the type-specific output
	// governance (governToolOutput and friends), returning the response
	// BEFORE post_tool_use appending and the model-output boundary — those
	// are shared. It also owns its setToolPanicPhase(panicPhaseToolExecute)
	// placement, so per-type pre-call work (mcpTool's Sentry breadcrumb)
	// keeps its original panic attribution.
	execute func(ctx context.Context) toolCallOutcome
}

// toolCallOutcome is what a wrapper's execute step hands the shared epilogue.
type toolCallOutcome struct {
	// resp is the governed (redact/PII/guardrail already applied) response,
	// not yet hook-appended or bounded.
	resp fantasy.ToolResponse
	// failed selects the failure framing everywhere it must agree at once:
	// the post_tool_use isError flag, the journaled outcome, and (negated)
	// the policy result's succeeded bit.
	failed bool
	// cause, when non-nil, is returned from Run as a boundedModelToolError
	// wrapping the FINAL bounded content — set after the shared epilogue so
	// err and resp.Content stay byte-identical (native tools only; see the
	// divergence note above).
	cause error
	// modelNote is fleet's own note for the model about this result (mcpTool's
	// date-window reminders), joined to resp.Content ahead of any
	// post_tool_use fragment. Like that fragment it reaches the model, the
	// hooks, the journal and the session log, but not the policy record:
	// see runGovernedToolCall.
	modelNote string
}

// runGovernedToolCall is the single governance framing every model-visible
// tool call crosses: pre_tool_use hook gate → Policy.BeforeToolCall gate →
// per-type validation → durable intent journal → execute → post_tool_use
// append → model-output boundary → durable outcome journal → policy result
// record. The order is load-bearing at every step — the comments below record
// why — and the caller's outer panicContainedTool attributes and contains a
// panic anywhere inside it.
func runGovernedToolCall(ctx context.Context, f toolCallFraming, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	ctx = ensureOutputGovernanceState(ctx)
	// pre_tool_use hooks (#788) run BEFORE fleet's own gates, on the
	// unmodified input. A block short-circuits without executing the tool or
	// recording a result (matching the policy-block behavior below). Existing
	// gates then evaluate after hooks, so a hook can only narrow, never
	// widen. MCP tools reach here under their real mcp_<server>_<tool> name —
	// which also covers the deferred-disclosure route, where the bridge
	// dispatches straight to the wrapped Run.
	if blocked, reason := f.hooks.preToolUse(ctx, f.name, params.ID, params.Input); blocked {
		return governedToolRefusal(ctx, f.name, params.ID, reason), nil
	}
	if f.policy != nil {
		setToolPanicPhase(ctx, panicPhasePolicyBefore)
		if blocked, msg := f.policy.BeforeToolCall(f.name, params.ID, params.Input); blocked {
			return governedToolRefusal(ctx, f.name, params.ID, msg), nil
		}
	}
	// From here on the policy has admitted the call, and may have reserved
	// state for it that only its result record releases (a send_email held
	// in flight against an identical duplicate). Each refusal below means the
	// call will never run or record a result, so it hands that reservation
	// back first; otherwise an identical retry would be refused for the rest
	// of the turn as a duplicate of a call that never happened.
	if f.validate != nil {
		if refusal, ok := f.validate(); !ok {
			releaseAdmittedCall(f.policy, f.name, params.Input)
			return governedToolRefusal(ctx, f.name, params.ID, refusal), nil
		}
	}
	// Durable tool-intent barrier (#798): the intent record commits BEFORE
	// the tool can produce a side effect, after the gates above so blocked
	// calls never journal. A failed write refuses dispatch (fail closed) — a
	// crash may then leave an un-run journaled call, which startup recovery
	// pairs with an explicit unknown-outcome result rather than losing the
	// record.
	if refusal, ok := journalToolIntent(ctx, f.journal, f.name, params.ID, params.Input); !ok {
		releaseAdmittedCall(f.policy, f.name, params.Input)
		return governedToolRefusal(ctx, f.name, params.ID, refusal), nil
	}
	out := f.execute(ctx)
	// governed is the tool's own governed payload, before fleet's notes and
	// any post_tool_use fragment are joined to it. It is what the policy
	// records (policyRecordText): the policy CLASSIFIES results — a provider
	// status_code, a {"success":false} payload, a batch's results[], a task
	// tracker snapshot — by parsing them, and text appended to a JSON payload
	// leaves nothing to parse, so {"status_code":400} would read as a clean
	// success (and a send recorded as delivered).
	governed := out.resp
	out.resp.Content = appendHookContext(out.resp.Content, out.modelNote)
	// post_tool_use hooks (#788) fire on success AND failure, and the
	// fragment joins BEFORE the model-output boundary and the durable outcome
	// record below so the journal, the session log and the model all see the
	// fragment — and so a hook fragment can never push a response past the
	// model-visible ceiling.
	out.resp.Content = appendGovernedPostHook(ctx, f.hooks, f.name, params.ID, params.Input, out.resp.Content, out.failed)
	// Bound every response, including media and Fleet-generated
	// deferred-bridge corrections, before anything durable records it. The
	// outer registration boundary repeats this as an idempotent route-proof
	// backstop. (The native error path formerly bounded twice here; the
	// single pass is a no-op for all realistic inputs, and in the one
	// adversarial re-detection corner — a >cap error text whose head/tail
	// preview cut manufactures a binary-looking run the full content lacked —
	// the single-pass journal/audit bytes are the more consistent choice:
	// they match MCP's long-standing single-bound behavior, and the
	// model-visible bytes converge at the outer boundary either way.)
	// The live limit is read once: the policy record below asks whether the
	// payload alone passes the boundary, and must ask it of the limit this
	// response was bounded with.
	outputLimit := maxToolOutputBytes()
	resp := boundModelVisibleToolResponseWithin(ctx, f.name, params.ID, out.resp, outputLimit)
	markOutputGoverned(ctx)
	// Durable outcome record (#798): the exact bounded model-visible bytes,
	// before the response re-enters the provider loop.
	journalToolOutcome(ctx, f.journal, f.name, params.ID, resp.Content, out.failed)
	// Record the outcome so policies that gate on tool RESULTS observe native
	// tool calls (bash/python/task_tracker/...), not just the MCP and
	// built-in MCP tools. Without this the scheduled task-tracker finish gate
	// (latestTaskTracker.Seen) never fired in production. A transport error
	// or an is-error response counts as a failed call. Nil-safe on policy.
	//
	// The record is the tool's own governed payload, without fleet's notes or
	// a hook fragment (policyRecordText).
	recordPolicyToolResult(ctx, f.policy, f.name, params.Input, policyRecordText(governed, out.resp, resp, out.failed, outputLimit), !out.failed)
	if out.cause != nil {
		// Fantasy persists a non-nil Go error as the model-visible tool
		// result. The typed wrapper carries the exact bounded bytes and
		// preserves errors.Is/As without ever exposing the original Error
		// implementation beyond this boundary.
		return resp, &boundedModelToolError{cause: out.cause, message: resp.Content}
	}
	return resp, nil
}

// releaseAdmittedCall hands back what policy's gates reserved for a call they
// admitted that is then refused before it runs. The reservation lives on the
// run's orchestrationState, found through any wrapping policy (Unwrap), so a
// driver that layers its own Policy over a built-in one needs no extra method.
// A nil policy ran no gate, and so reserved nothing.
func releaseAdmittedCall(policy Policy, name, input string) {
	if policy == nil {
		return
	}
	if o, ok := policyOrchestration(policy); ok {
		o.releaseAdmittedToolCall(name, input)
	}
}

// governedToolRefusal is the shared no-execute exit every pre-dispatch gate
// takes: the refusal text crosses the same governToolOutput choke point as
// real tool output (a gate message may quote model-supplied input), is capped
// by the model-output boundary, and completes output governance. Refused
// calls deliberately journal nothing and record no policy result — the tool
// never ran.
func governedToolRefusal(ctx context.Context, name, callID, msg string) fantasy.ToolResponse {
	msg, _ = governToolOutput(ctx, name, msg)
	resp := boundModelVisibleToolResponse(ctx, name, callID, fantasy.NewTextErrorResponse(msg))
	markOutputGoverned(ctx)
	return resp
}

// appendGovernedPostHook runs post_tool_use hooks (#788) and appends any
// bounded context fragment to text, passing the fragment through the
// governToolOutput choke point (secret/PII/guardrail) so hook-supplied bytes
// are governed like every other tool output. Fires on both success and
// failure paths, for native and MCP tools alike.
func appendGovernedPostHook(ctx context.Context, hooks *hookEngine, toolName, callID, rawInput, text string, isErr bool) string {
	frag := hooks.postToolUse(ctx, toolName, callID, rawInput, text, isErr)
	if frag == "" {
		return text
	}
	governed, blocked := governToolOutput(ctx, toolName, frag)
	if blocked || containsEncodedBinary(governed) {
		// A base64-heavy fragment (e.g. a JWT attestation) would trip the
		// model-output boundary's binary detector against the COMBINED
		// result, suppressing the tool's legitimate output. Drop the fragment
		// instead.
		return text
	}
	return appendHookContext(text, governed)
}

// fleetNoteTags are the tags that open the text fleet joins to a tool result:
// a post_tool_use fragment (hookEngine.postToolUse) and the date-window
// reminders (dateWindowNotes).
var fleetNoteTags = []string{"[hook:", "[fleet date-window]", "[fleet search]"}

// StartsWithFleetNote reports whether text opens with one of the tags fleet
// puts at the head of what it joins to a tool result. A reader of the session
// log uses it to tell fleet's notes after a JSON payload from any other
// bracketed text a tool wrote there itself, which is part of the result. It
// is a format check, not provenance: a tool that writes one of these tags
// after its own payload is read as payload plus notes.
func StartsWithFleetNote(text string) bool {
	for _, tag := range fleetNoteTags {
		if strings.HasPrefix(text, tag) {
			return true
		}
	}
	return false
}

// appendHookContext appends a post-tool hook fragment to a tool result.
func appendHookContext(content, frag string) string {
	if frag == "" {
		return content
	}
	if content == "" {
		return frag
	}
	return content + "\n\n" + frag
}

// policyRecordText is the text the policy records for a call: the tool's
// own governed payload (governed), never fleet's notes or a post_tool_use
// fragment (annotated is the response with those joined; bounded is the
// model-visible result).
//
// A successful call records the governed but UNBOUNDED text
// (policyResultText): a large batch results[] (a 200-deal merge runs well
// past the model-visible ceiling) would otherwise reach the policy as a
// truncation envelope that is neither parsed nor discharged. A failed call's
// outcome is already decided, so it records what the model-output boundary
// leaves of it (#793) — but of the payload alone: when the payload on its own
// passes the boundary whole (passesModelVisibleBoundary), it is recorded
// as it is, even if the annotations joined to it pushed the model-visible
// text past the limit. Otherwise the bounded bytes are recorded: a truncation
// envelope, which quotes only a preview of the text, or for a media response
// an envelope with no preview at all.
// outputLimit is the limit bounded was cut with.
func policyRecordText(governed, annotated, bounded fantasy.ToolResponse, failed bool, outputLimit int) string {
	if !failed {
		return policyResultText(governed, bounded)
	}
	if annotated.Content != governed.Content && passesModelVisibleBoundary(governed, outputLimit) {
		return governed.Content
	}
	return bounded.Content
}

// policyResultText returns the text the policy records for a successful
// call: the governed (redacted, screened) payload before the model-visible
// boundary cut it. A media response keeps the bounded text, whose payload the
// boundary replaced. Text past maxDealOutcomesBytes is cut to
// maxDealOutcomesBytes+1 bytes: still over every result parser's cap, so a
// per-record envelope that large fails closed (malformedDealOutcomes) rather
// than being parsed whole.
func policyResultText(governed, bounded fantasy.ToolResponse) string {
	if len(governed.Data) > 0 || governed.Type == "image" || governed.Type == "media" {
		return bounded.Content
	}
	if len(governed.Content) > maxDealOutcomesBytes {
		return governed.Content[:maxDealOutcomesBytes+1]
	}
	return governed.Content
}
