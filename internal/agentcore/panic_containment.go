package agentcore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/safe"
)

// ErrRunBoundaryPanic identifies a panic that Fleet contained at a governed
// run boundary. Tool panics are converted to an in-band ToolResponse and do not
// return this error; callback/policy panics that cannot be paired to a tool call
// surface as an ordinary run error wrapping this sentinel.
var ErrRunBoundaryPanic = errors.New("panic contained at agent run boundary")

const (
	panicLocationTool     = "agentcore.tool"
	panicLocationObserver = "agentcore.observer"
	panicLocationPolicy   = "agentcore.policy"
	panicLocationRun      = "agentcore.run"

	panicPhaseToolExecute     = "tool.execute"
	panicPhasePolicyBefore    = "policy.before_tool_call"
	panicPhasePolicyRecord    = "policy.record_tool_result"
	panicPhasePolicyFinish    = "policy.can_finish"
	panicPhaseOutputRedact    = "tool.output_redaction"
	panicPhaseOutputGuardrail = "tool.output_guardrail"
	panicPhaseResultFlatten   = "tool.result_flatten"
	panicPhaseRunSynchronous  = "run.synchronous"
)

// panicAttribution is the non-secret run identity copied into every recovered
// event. A tool boundary adds its logical name and call ID at dispatch time.
type panicAttribution struct {
	runMode        string
	taskID         string
	conversationID string
}

func (a panicAttribution) metadata(location, boundary string) safe.PanicMetadata {
	return safe.PanicMetadata{
		Location:       location,
		Boundary:       boundary,
		RunMode:        a.runMode,
		TaskID:         a.taskID,
		ConversationID: a.conversationID,
	}
}

// recoverSynchronousRunPanic is the final backstop around agentcore.Run itself.
// The per-tool wrapper remains necessary because Fantasy dispatches Tool.Run in
// other goroutines and must receive an in-band paired result there. This guard
// covers synchronous AgentTool methods such as Info/ProviderOptions, input
// adapters, finalize hooks, and any future Fleet callback that executes on the
// Run goroutine. No raw value or stack crosses the recovery seam.
func recoverSynchronousRunPanic(attribution panicAttribution, result *Result, err *error) {
	if recovered := recover(); recovered != nil {
		meta := attribution.metadata(panicLocationRun, panicPhaseRunSynchronous)
		event := safe.EmitPanicWithMetadata(meta, recovered, nil)
		*result = Result{}
		*err = &containedBoundaryError{incidentID: event.IncidentID, boundary: meta.Boundary}
	}
}

// containedBoundaryError is deliberately opaque: callers get the incident ID
// needed for operator correlation, never the recovered value or stack.
type containedBoundaryError struct {
	incidentID string
	boundary   string
}

func (e *containedBoundaryError) Error() string {
	return fmt.Sprintf("agent run boundary %s failed unexpectedly (reference %s)", e.boundary, e.incidentID)
}

func (e *containedBoundaryError) Unwrap() error { return ErrRunBoundaryPanic }

// toolPanicState lives only for one AgentTool.Run invocation. Fleet's own
// policy/output wrappers advance it immediately before each boundary. The
// recordAttempted bit lets the outer recovery repair failed accounting exactly
// once when execution/output panics before Policy.RecordToolResult is reached,
// without retrying a RecordToolResult call that itself panicked.
type toolPanicState struct {
	phase           string
	recordAttempted bool
}
type toolPanicStateKey struct{}

func setToolPanicPhase(ctx context.Context, phase string) {
	if state, ok := ctx.Value(toolPanicStateKey{}).(*toolPanicState); ok && state != nil {
		state.phase = phase
	}
}

func recordPolicyToolResult(ctx context.Context, policy Policy, name, input, result string, succeeded bool) {
	if policy == nil {
		return
	}
	if state, ok := ctx.Value(toolPanicStateKey{}).(*toolPanicState); ok && state != nil {
		state.phase = panicPhasePolicyRecord
		state.recordAttempted = true
	}
	policy.RecordToolResult(name, input, result, succeeded)
}

// panicContainedTool is the outermost Fleet-owned dispatch boundary. Fantasy
// invokes Run in coordinator/parallel worker goroutines, so recovery MUST occur
// here, in that same goroutine. A nil Go error keeps the panic as an ordinary
// tool failure, preserving exactly one tool_result for the original call ID.
type panicContainedTool struct {
	inner       fantasy.AgentTool
	name        string
	attribution panicAttribution
	policy      Policy
}

func containTool(inner fantasy.AgentTool, attribution panicAttribution, policy Policy) fantasy.AgentTool {
	if inner == nil {
		return nil
	}
	if _, ok := inner.(*panicContainedTool); ok {
		return inner
	}
	return &panicContainedTool{
		inner:       inner,
		name:        inner.Info().Name,
		attribution: attribution,
		policy:      policy,
	}
}

func containToolRoster(tools []fantasy.AgentTool, attribution panicAttribution, policy Policy) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, 0, len(tools))
	for _, tool := range tools {
		if tool != nil {
			out = append(out, containTool(tool, attribution, policy))
		}
	}
	return out
}

func (t *panicContainedTool) Info() fantasy.ToolInfo { return t.inner.Info() }
func (t *panicContainedTool) ProviderOptions() fantasy.ProviderOptions {
	return t.inner.ProviderOptions()
}
func (t *panicContainedTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(opts)
}

func (t *panicContainedTool) Run(ctx context.Context, call fantasy.ToolCall) (resp fantasy.ToolResponse, err error) {
	state := &toolPanicState{phase: panicPhaseToolExecute}
	ctx = context.WithValue(ctx, toolPanicStateKey{}, state)
	defer func() {
		if recovered := recover(); recovered != nil {
			meta := t.attribution.metadata(panicLocationTool, state.phase)
			meta.ToolName = t.name
			meta.ToolCallID = call.ID
			event := safe.EmitPanicWithMetadata(meta, recovered, nil)
			// A panic may occur after an external mutation. Mark the response as
			// possibly committed for downstream diagnostics; streamSink's earlier
			// tool.call event is the authoritative in-round retry gate (ADR-0035).
			resp = fantasy.WithResponseMetadata(
				fantasy.NewTextErrorResponse(containedToolPanicText(event.IncidentID)),
				map[string]any{"incident_id": event.IncidentID, "possibly_committed": true},
			)
			// Normal wrappers record after execution/output screening. A panic in
			// either phase skips that line, so repair the logical tool's failed
			// accounting here. recordAttempted prevents a RecordToolResult panic
			// from being retried. This outer wrapper also surrounds deferred MCP's
			// logical tool, so it records mcp_<server>_<tool>, not only tool_call.
			if !state.recordAttempted && panicNeedsFailedAccounting(state.phase) {
				recordRecoveredToolFailure(ctx, t.policy, t.name, call, resp.Content, t.attribution)
			}
			err = nil
		}
	}()
	return t.inner.Run(ctx, call)
}

func containedToolPanicText(incidentID string) string {
	return fmt.Sprintf(
		"Tool execution failed unexpectedly. Reference: %s. The call may have committed side effects; verify state before retrying.",
		incidentID,
	)
}

// safeToolResultText is the last defense around Fantasy result flattening.
// Governed AgentTool errors are normalized before they reach Fantasy, but a
// provider/validation result can still carry an arbitrary error implementation.
// If Error itself panics, preserve call/result pairing with the same opaque
// incident shape and full tool/run attribution instead of dropping the result
// from a Fantasy worker callback.
func safeToolResultText(tr fantasy.ToolResultContent, attribution panicAttribution) (text string, isErr bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			meta := attribution.metadata(panicLocationTool, panicPhaseResultFlatten)
			meta.ToolName = tr.ToolName
			meta.ToolCallID = tr.ToolCallID
			event := safe.EmitPanicWithMetadata(meta, recovered, nil)
			text = containedToolPanicText(event.IncidentID)
			isErr = true
		}
	}()
	return toolResultText(tr)
}

func panicNeedsFailedAccounting(phase string) bool {
	switch phase {
	case panicPhasePolicyBefore, panicPhaseToolExecute, panicPhaseOutputRedact, panicPhaseOutputGuardrail:
		return true
	default:
		return false
	}
}

// recordRecoveredToolFailure contains a second panic from the accounting hook:
// recovery code must never become a new process-fatal path. The original tool
// incident remains the model-visible reference; a broken policy hook receives
// its own operator incident at the policy.record boundary.
func recordRecoveredToolFailure(
	ctx context.Context,
	policy Policy,
	name string,
	call fantasy.ToolCall,
	result string,
	attribution panicAttribution,
) {
	if policy == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			meta := attribution.metadata(panicLocationPolicy, panicPhasePolicyRecord)
			meta.ToolName = name
			meta.ToolCallID = call.ID
			safe.EmitPanicWithMetadata(meta, recovered, nil)
		}
	}()
	recordPolicyToolResult(ctx, policy, name, call.Input, result, false)
}

// panicContainedObserver serializes observer delivery, then permanently
// disables the observer after its first panic. Serialization ensures concurrent
// Fantasy tool-result callbacks cannot race past the disable flag. The run
// checks Err only after callbacks/tool goroutines settle and converts it to an
// ordinary error, never a cross-goroutine panic.
type panicContainedObserver struct {
	inner       Observer
	attribution panicAttribution

	mu     sync.Mutex
	failed *containedBoundaryError
}

func containObserver(inner Observer, attribution panicAttribution) *panicContainedObserver {
	return &panicContainedObserver{inner: inner, attribution: attribution}
}

// Observer returns the contained seam while preserving nil as nil for callers
// whose behavior distinguishes an absent observer from a no-op observer.
func (o *panicContainedObserver) Observer() Observer {
	if o == nil || o.inner == nil {
		return nil
	}
	return o
}

func (o *panicContainedObserver) Observe(eventType string, payload map[string]any) {
	if o == nil || o.inner == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failed != nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			meta := o.attribution.metadata(panicLocationObserver, "observer."+eventType)
			if payload != nil {
				meta.ToolName, _ = payload[evtFieldName].(string)
				if meta.ToolName == "" {
					meta.ToolName, _ = payload["tool"].(string)
				}
				meta.ToolCallID, _ = payload[evtFieldID].(string)
			}
			event := safe.EmitPanicWithMetadata(meta, recovered, nil)
			o.failed = &containedBoundaryError{incidentID: event.IncidentID, boundary: meta.Boundary}
		}
	}()
	o.inner.Observe(eventType, payload)
}

func (o *panicContainedObserver) Err() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failed == nil {
		return nil
	}
	return o.failed
}

// prefer returns a contained observer failure ahead of another stream error.
// It lets Run reuse its existing stream-error branch without growing another
// observer-specific control path.
func (o *panicContainedObserver) prefer(other error) error {
	if err := o.Err(); err != nil {
		return err
	}
	return other
}

func finalizeWithPanicBoundary(
	ctx context.Context,
	hook FinalizeHook,
	in FinalizeInput,
	observer *panicContainedObserver,
) (string, error) {
	recovered, err := hook(ctx, in)
	if observerErr := observer.Err(); observerErr != nil {
		return "", observerErr
	}
	if err != nil {
		log.Printf("finalize hook error: %v", err)
		return in.FinalText, nil
	}
	if recovered != "" {
		return recovered, nil
	}
	return in.FinalText, nil
}

func finalizeToolCallCallback(sink *streamSink, attribution panicAttribution) fantasy.OnToolCallFunc {
	return func(call fantasy.ToolCallContent) (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				meta := attribution.metadata(panicLocationRun, "finalize.on_tool_call")
				meta.ToolName = call.ToolName
				meta.ToolCallID = call.ToolCallID
				safe.EmitPanicWithMetadata(meta, recovered, nil)
				err = nil
			}
		}()
		if sink != nil {
			sink.onToolCall(call.ToolCallID, call.ToolName, call.Input)
		}
		return nil
	}
}

func finalizeToolResultCallback(sink *streamSink, attribution panicAttribution) fantasy.OnToolResultFunc {
	return func(result fantasy.ToolResultContent) (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				meta := attribution.metadata(panicLocationRun, "finalize.on_tool_result")
				meta.ToolName = result.ToolName
				meta.ToolCallID = result.ToolCallID
				safe.EmitPanicWithMetadata(meta, recovered, nil)
				err = nil
			}
		}()
		if sink != nil {
			text, isErr := safeToolResultText(result, attribution)
			sink.onToolResult(result.ToolCallID, result.ToolName, text, isErr)
		}
		return nil
	}
}

func appendEnforcementMessages(
	messages []fantasy.Message,
	enforcement []string,
	observer Observer,
	boundary *panicContainedObserver,
) ([]fantasy.Message, error) {
	for _, nudge := range enforcement {
		messages = append(messages, fantasy.NewUserMessage(nudge))
		if observer != nil {
			observer.Observe("enforcement", map[string]any{"message": nudge})
		}
	}
	return messages, boundary.Err()
}

func callPolicyCanFinish(policy Policy, round int, attribution panicAttribution) (ok bool, messages []string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			meta := attribution.metadata(panicLocationPolicy, panicPhasePolicyFinish)
			event := safe.EmitPanicWithMetadata(meta, recovered, nil)
			err = &containedBoundaryError{incidentID: event.IncidentID, boundary: meta.Boundary}
		}
	}()
	ok, messages = policy.CanFinish(round)
	return ok, messages, nil
}
