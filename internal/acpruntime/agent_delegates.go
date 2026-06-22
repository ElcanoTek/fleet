package acpruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"charm.land/fantasy"
	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// Agent-side delegates for the P-ACP-2b governed surfaces: MCP tool calls,
// approval/memory/note staging, and usage reporting. Each rides a `_fleet/*`
// extension method back to the host so the EFFECT (credentialed MCP call, DB
// write + SSE card, token/cost accounting) happens host-side while the DECISION
// (which tool, when to stage) is made by the SAME policy the in-process path runs,
// here inside the agent loop. This is what makes native-acp govern identically.

// delegatingMCPTool is an agent-side fantasy.AgentTool that advertises an
// mcp_<server>_<tool> surface (built from a host-supplied descriptor with NO
// credentials) and delegates every invocation over `_fleet/mcp` to the host,
// which runs it against the per-task credentialed mcp.Client. The agent never
// holds MCP credentials.
//
// It is wrapped by agentcore's policyGuardedTool path the same way the in-process
// mcpTool is — registered through buildFantasyTools so the InteractivePolicy gate
// (cost/repeat/approval) runs before each call, identical to in-process.
type delegatingMCPTool struct {
	desc            MCPToolDescriptor
	conn            *acp.AgentSideConnection
	sessionID       string
	policy          agentcore.Policy
	providerOptions fantasy.ProviderOptions
}

var _ fantasy.AgentTool = (*delegatingMCPTool)(nil)

func (t *delegatingMCPTool) name() string {
	return fmt.Sprintf("mcp_%s_%s", t.desc.Server, t.desc.Tool)
}

func (t *delegatingMCPTool) Info() fantasy.ToolInfo {
	parameters := map[string]any{}
	required := []string{}
	if props, ok := t.desc.InputSchema["properties"].(map[string]any); ok {
		// Sanitize EXACTLY as the in-process mcpTool does (strip OpenAI-rejected
		// `\p{…}` patterns), so the advertised schema is byte-identical to the
		// in-process tool surface.
		parameters = agentcore.SanitizeSchemaProperties(props)
	}
	if req, ok := t.desc.InputSchema["required"].([]any); ok {
		for _, v := range req {
			if s, ok := v.(string); ok {
				required = append(required, s)
			}
		}
	}
	return fantasy.ToolInfo{
		Name:        t.name(),
		Description: t.desc.Description,
		Parameters:  parameters,
		Required:    required,
	}
}

func (t *delegatingMCPTool) Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	name := t.name()
	// Run the SAME policy gate the in-process mcpTool runs (cost/repeat/email
	// approval), in the agent loop. Identical governance: the policy IS the
	// in-process InteractivePolicy.
	if t.policy != nil {
		if blocked, msg := t.policy.BeforeToolCall(name, params.ID, params.Input); blocked {
			return fantasy.NewTextErrorResponse(msg), nil
		}
	}

	var args map[string]any
	if params.Input != "" {
		if err := json.Unmarshal([]byte(params.Input), &args); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
		}
	}
	raw, err := t.conn.CallExtension(ctx, ExtMethodMCP, MCPRequest{
		SessionID: t.sessionID,
		Server:    t.desc.Server,
		Tool:      t.desc.Tool,
		Arguments: args,
	})
	if err != nil {
		t.record(name, params.Input, "", false)
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Error calling %s: %v", name, err)), nil
	}
	var resp MCPResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.record(name, params.Input, "", false)
		return fantasy.NewTextErrorResponse(fmt.Sprintf("decode mcp response: %v", err)), nil
	}
	if resp.Error != "" {
		t.record(name, params.Input, resp.Error, false)
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Error calling %s: %s", name, resp.Error)), nil
	}
	if resp.IsError {
		// Tool-level error (MCP isError=true): surface it the same way the
		// in-process mcpTool does so the model + log see an identical signal.
		t.record(name, params.Input, resp.Text, false)
		return fantasy.NewTextErrorResponse(resp.Text), nil
	}
	t.record(name, params.Input, resp.Text, true)
	return fantasy.NewTextResponse(resp.Text), nil
}

func (t *delegatingMCPTool) record(name, rawInput, resultText string, succeeded bool) {
	if t.policy != nil {
		t.policy.RecordToolResult(name, rawInput, resultText, succeeded)
	}
}

func (t *delegatingMCPTool) ProviderOptions() fantasy.ProviderOptions { return t.providerOptions }
func (t *delegatingMCPTool) SetProviderOptions(o fantasy.ProviderOptions) {
	t.providerOptions = o
}

// buildDelegatingMCPTools turns the host-supplied descriptors into delegating
// fantasy tools. Each carries the policy so it runs the SAME gate + RecordToolResult
// the in-process mcpTool does (so it is registered as a RunConfig.NativeTools
// member that already owns its policy — NOT re-wrapped in policyGuardedTool, which
// would double the BeforeToolCall and skip RecordToolResult).
func buildDelegatingMCPTools(conn *acp.AgentSideConnection, sessionID string, policy agentcore.Policy, descs []MCPToolDescriptor) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, 0, len(descs))
	for _, d := range descs {
		out = append(out, &delegatingMCPTool{desc: d, conn: conn, sessionID: sessionID, policy: policy})
	}
	return out
}

// delegatingStager implements agentcore.ApprovalStager + MemoryProposer +
// NoteProposer by forwarding each staging effect over `_fleet/stage` to the host.
// The agent's InteractivePolicy decides WHEN to stage (identical to in-process);
// the host performs the EFFECT (DB write + SSE card) through the real stagers.
type delegatingStager struct {
	conn      *acp.AgentSideConnection
	sessionID string
}

var (
	_ agentcore.ApprovalStager = (*delegatingStager)(nil)
	_ agentcore.MemoryProposer = (*delegatingStager)(nil)
)

func (s *delegatingStager) stage(req StageRequest) (StageResponse, error) {
	req.SessionID = s.sessionID
	raw, err := s.conn.CallExtension(context.Background(), ExtMethodStage, req)
	if err != nil {
		return StageResponse{}, fmt.Errorf("%s: %w", ExtMethodStage, err)
	}
	var resp StageResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return StageResponse{}, fmt.Errorf("decode stage response: %w", err)
	}
	if resp.Error != "" {
		return resp, fmt.Errorf("%s", resp.Error)
	}
	return resp, nil
}

func (s *delegatingStager) Stage(toolName, toolCallID, rawInput string) (string, error) {
	resp, err := s.stage(StageRequest{
		Kind: StageApproval, ToolName: toolName, ToolCallID: toolCallID, RawInput: rawInput,
	})
	if err != nil {
		return "", err
	}
	return resp.ProposalID, nil
}

func (s *delegatingStager) StageSuggestion(reason string) (string, string, error) {
	// The host owns the suppression gate (already-advanced / prior-approved /
	// cooldown). A suppressed suggestion is NOT an error: the host returns an
	// empty id + the agent-facing message verbatim, mirroring the in-process
	// StageSuggestion contract. Only a transport/internal failure is an error.
	req := StageRequest{Kind: StageSuggestion, Reason: reason}
	req.SessionID = s.sessionID
	raw, err := s.conn.CallExtension(context.Background(), ExtMethodStage, req)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", ExtMethodStage, err)
	}
	var resp StageResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", "", fmt.Errorf("decode stage response: %w", err)
	}
	if resp.Error != "" {
		return "", "", fmt.Errorf("%s", resp.Error)
	}
	return resp.ProposalID, resp.Message, nil
}

func (s *delegatingStager) Propose(content string) (string, error) {
	resp, err := s.stage(StageRequest{Kind: StageMemory, Content: content})
	if err != nil {
		return "", err
	}
	return resp.ProposalID, nil
}

// delegatingVerifier rides `_fleet/verify` to run the host-side end-of-run
// verifier. The agent's scheduled policy DECIDES to verify (when its inner
// audit/finish enforcement clears); the verifier EFFECT (an LLM re-check on the
// host fallback model, host-side creds) runs on the host. Mirrors delegatingStager.
type delegatingVerifier struct {
	conn      *acp.AgentSideConnection
	sessionID string
}

// verify ships the tool-exec summary + round and returns the missing required
// actions. A transport failure OR a host-reported VerifyResponse.Error is
// returned as an error so the caller (CanFinish) can fail OPEN, exactly as the
// in-process verifier does on error.
func (v *delegatingVerifier) verify(round int, records []ToolExecRecord) ([]string, error) {
	raw, err := v.conn.CallExtension(context.Background(), ExtMethodVerify, VerifyRequest{
		SessionID: v.sessionID,
		Round:     round,
		Records:   records,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ExtMethodVerify, err)
	}
	var resp VerifyResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode verify response: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return resp.Missing, nil
}

// toolExecAccumulator is an agentcore.Observer that builds the verifier's
// tool-exec summary from the agent's OWN authoritative run stream — the
// tool.call/tool.result events agentcore emits in this container. This is the
// causally-ordered, ground-truth view at the moment the loop decides to finish,
// so the verifier never depends on the host re-deriving the summary from a live
// (cross-process, possibly-lossy) event projection. It pairs calls with results
// the SAME way the host's buildToolExecSummary does: a call with no result counts
// as failed.
type toolExecAccumulator struct {
	mu       sync.Mutex
	callName map[string]string // tool_call id → tool name
	results  []ToolExecRecord  // one per tool.result, in arrival order
	resolved map[string]bool   // call ids that produced a result
}

func newToolExecAccumulator() *toolExecAccumulator {
	return &toolExecAccumulator{callName: map[string]string{}, resolved: map[string]bool{}}
}

func (a *toolExecAccumulator) Observe(eventType string, payload map[string]any) {
	switch eventType {
	case "tool.call":
		id, _ := payload["id"].(string)
		name, _ := payload["name"].(string)
		a.mu.Lock()
		a.callName[id] = name
		a.mu.Unlock()
	case "tool.result":
		id, _ := payload["id"].(string)
		isErr, _ := payload["is_err"].(bool)
		a.mu.Lock()
		a.results = append(a.results, ToolExecRecord{Name: a.callName[id], Succeeded: !isErr})
		a.resolved[id] = true
		a.mu.Unlock()
	}
}

// records returns the tool-exec summary: every completed tool result, plus any
// issued tool call that never produced a result (counted failed, like the host's
// buildToolExecSummary).
func (a *toolExecAccumulator) records() []ToolExecRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]ToolExecRecord(nil), a.results...)
	for id, name := range a.callName {
		if !a.resolved[id] {
			out = append(out, ToolExecRecord{Name: name, Succeeded: false})
		}
	}
	return out
}

// observerFanout fans run events to two observers — the agent's delegating
// observer (→ host) AND the tool-exec accumulator the verifier reads.
type observerFanout struct {
	a, b agentcore.Observer
}

func (f observerFanout) Observe(eventType string, payload map[string]any) {
	f.a.Observe(eventType, payload)
	f.b.Observe(eventType, payload)
}

// endOfRunVerifier runs the host-side verifier for a round + tool-exec summary.
// *delegatingVerifier is the production implementation (over `_fleet/verify`);
// the interface lets tests inject a fake without an ACP connection.
type endOfRunVerifier interface {
	verify(round int, records []ToolExecRecord) (missing []string, err error)
}

var _ endOfRunVerifier = (*delegatingVerifier)(nil)

// verifyingScheduledPolicy wraps the agent's in-loop ScheduledPolicy with the
// end-of-run verifier, mirroring the in-process scheduledPolicy.CanFinish exactly:
// defer to the inner audit/finish enforcement; once it clears, run the verifier
// ONCE; a non-empty missing list becomes a final enforcement round. The only
// difference from in-process is WHERE the verifier runs — over `_fleet/verify` on
// the host — so native-acp governs identically instead of silently skipping it.
type verifyingScheduledPolicy struct {
	inner    agentcore.Policy // the *agentcore.ScheduledPolicy; Unwrap exposes it for orchestration binding
	verifier endOfRunVerifier
	acc      *toolExecAccumulator
	verified bool
}

var (
	_ agentcore.Policy          = (*verifyingScheduledPolicy)(nil)
	_ agentcore.PolicyUnwrapper = (*verifyingScheduledPolicy)(nil)
)

func (p *verifyingScheduledPolicy) BeforeToolCall(toolName, toolCallID, rawInput string) (bool, string) {
	return p.inner.BeforeToolCall(toolName, toolCallID, rawInput)
}

func (p *verifyingScheduledPolicy) RecordToolResult(toolName, rawInput, resultText string, succeeded bool) {
	p.inner.RecordToolResult(toolName, rawInput, resultText, succeeded)
}

func (p *verifyingScheduledPolicy) CanFinish(round int) (bool, []string) {
	if ok, msgs := p.inner.CanFinish(round); !ok {
		return false, msgs
	}
	if p.verified || p.verifier == nil {
		return true, nil
	}
	// Attempt-once, set BEFORE the call: a flapping/erroring host verifier must not
	// trap the loop into repeated verify rounds (parity with the in-process path).
	p.verified = true
	var recs []ToolExecRecord
	if p.acc != nil {
		recs = p.acc.records()
	}
	missing, err := p.verifier.verify(round, recs)
	if err != nil {
		// FAIL-OPEN on any verifier failure (transport, decode, host-side error),
		// exactly as the in-process verifier logs and allows finish.
		log.Printf("verifier skipped: %v", err)
		return true, nil
	}
	if len(missing) == 0 {
		return true, nil
	}
	return false, []string{fmt.Sprintf(
		"End-of-run verification found unfinished required actions: %v. "+
			"Complete each one now, or call confirm_audit(success=false, user_visible_summary=...) to abort explicitly.",
		missing)}
}

// Unwrap exposes the inner ScheduledPolicy so agentcore can reach its
// orchestration state (confirm_audit binding + usage), identical to the
// in-process scheduledPolicy wrapper.
func (p *verifyingScheduledPolicy) Unwrap() agentcore.Policy { return p.inner }

// noteProposerAdapter adapts delegatingStager's note path to agentcore.NoteProposer
// (whose Propose has a different signature than MemoryProposer.Propose, so it
// cannot share the method set on delegatingStager directly).
type noteProposerAdapter struct{ s *delegatingStager }

var _ agentcore.NoteProposer = noteProposerAdapter{}

func (n noteProposerAdapter) Propose(slug, title, body, reason string) (string, error) {
	resp, err := n.s.stage(StageRequest{
		Kind: StageNote, Slug: slug, Title: title, Body: body, Reason: reason,
	})
	if err != nil {
		return "", err
	}
	return resp.ProposalID, nil
}
