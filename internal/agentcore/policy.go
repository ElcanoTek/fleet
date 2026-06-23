package agentcore

// The two Policy bundles. Both are thin adapters over the shared
// orchestrationState; the divergence is which gates run before a tool call and
// when CanFinish returns true.
//
//   - InteractivePolicy: cost/token ceiling + repeat-call guard + approval /
//     memory staging; CanFinish returns true on round 0 so Run collapses to a
//     single pass (the chat 1-round special case).
//   - ScheduledPolicy: audit gating + critical-action enforcement + repeat-call
//     guard; CanFinish delegates to checkFinishEnforcement so the loop runs
//     until the confirm_audit + commitments + task tracker clear.
//
// Both satisfy `orchestration() *orchestrationState` so the loop's usage
// accounting and the confirm_audit tool share the same state.

// InteractivePolicy is the live-turn policy bundle.
type InteractivePolicy struct {
	orch *orchestrationState
}

// NewInteractivePolicy builds the interactive bundle. maxCostUSD/maxTotalTokens
// are the per-turn ceilings (0 = unlimited); approvalSink/memoryProposer may be
// nil. The loop-guard noun is set to the interactive wording.
func NewInteractivePolicy(maxCostUSD float64, maxTotalTokens int, approvalSink ApprovalStager, memoryProposer MemoryProposer) *InteractivePolicy {
	o := newOrchestrationState(nil, 0)
	o.setRepeatGuardNoun(repeatGuardNounReplyToUser)
	o.setCeilings(maxCostUSD, maxTotalTokens)
	if approvalSink != nil {
		o.setApprovalSink(approvalSink)
	}
	if memoryProposer != nil {
		o.setMemoryProposer(memoryProposer)
	}
	return &InteractivePolicy{orch: o}
}

func (p *InteractivePolicy) orchestration() *orchestrationState { return p.orch }

// SetNoteProposer wires the admin-notes proposer (propose_note) for this run.
// Available in both modes (the agent-notes wiki is global, unlike user
// memories which stay interactive-only).
func (p *InteractivePolicy) SetNoteProposer(np NoteProposer) { p.orch.setNoteProposer(np) }

// BeforeToolCall runs the interactive gate chain: ceilings → repeat-call guard →
// email safety (rate-limit/dedup/approval staging) → risky-bash approval →
// preview_email staging → suggest_advanced_model staging → memory proposal →
// note proposal. The bash/preview/suggest gates are inert when no approval sink
// is wired.
func (p *InteractivePolicy) BeforeToolCall(toolName, toolCallID, rawInput string) (bool, string) {
	if blocked, msg := p.orch.checkCeilings(); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkRepeatedCall(toolName, rawInput); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkEmailSafety(toolName, toolCallID, rawInput); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkBashSafety(toolName, toolCallID, rawInput); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkPreviewEmailSafety(toolName, toolCallID, rawInput); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkSuggestAdvancedSafety(toolName, rawInput); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkMemoryProposal(toolName, rawInput); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkNoteProposal(toolName, rawInput); blocked {
		return true, msg
	}
	return false, ""
}

func (p *InteractivePolicy) RecordToolResult(toolName, rawInput, resultText string, succeeded bool) {
	p.orch.recordToolResult(toolName, rawInput, resultText, succeeded)
}

// CanFinish always returns true at round 0 — this is the 1-round collapse that
// makes the chat single pass a special case of the unified loop. (Any later
// round would also finish, but interactive runs never reach one.)
func (p *InteractivePolicy) CanFinish(_ int) (bool, []string) {
	return true, nil
}

// ScheduledPolicy is the run-to-completion policy bundle.
type ScheduledPolicy struct {
	orch *orchestrationState
}

// NewScheduledPolicy builds the scheduled bundle over a session log. maxIterations
// is informational; the loop owns the real round cap. maxCostUSD/maxTotalTokens
// are the per-run ceilings (0 = unlimited) — enforced for unattended scheduled /
// one-shot runs exactly as the interactive policy enforces them, so a runaway
// agent is bounded by the configured budget rather than the invoice.
func NewScheduledPolicy(logSession *LogSession, maxIterations int, maxCostUSD float64, maxTotalTokens int) *ScheduledPolicy {
	o := newOrchestrationState(logSession, maxIterations)
	o.setRepeatGuardNoun(repeatGuardNounFinishTask)
	o.setCeilings(maxCostUSD, maxTotalTokens)
	return &ScheduledPolicy{orch: o}
}

func (p *ScheduledPolicy) orchestration() *orchestrationState { return p.orch }

// SetNoteProposer wires the admin-notes proposer (propose_note) for this run.
func (p *ScheduledPolicy) SetNoteProposer(np NoteProposer) { p.orch.setNoteProposer(np) }

// BeforeToolCall runs the scheduled gate chain: cost/token ceiling → repeat-call
// guard → critical-tool audit gating → note proposal. The ceiling check is FIRST
// (matching the interactive policy) so an unattended run that blows its budget
// stops calling tools and ends with what it has, rather than running unbounded.
func (p *ScheduledPolicy) BeforeToolCall(toolName, toolCallID, rawInput string) (bool, string) {
	if blocked, msg := p.orch.checkCeilings(); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkRepeatedCall(toolName, rawInput); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkCriticalTool(toolName, toolCallID, rawInput); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkNoteProposal(toolName, rawInput); blocked {
		return true, msg
	}
	return false, ""
}

func (p *ScheduledPolicy) RecordToolResult(toolName, rawInput, resultText string, succeeded bool) {
	p.orch.recordToolResult(toolName, rawInput, resultText, succeeded)
}

// CanFinish delegates to checkFinishEnforcement (audit + commitments + task
// tracker). The round arg is unused — scheduled finishing is state-driven, not
// round-driven.
func (p *ScheduledPolicy) CanFinish(_ int) (bool, []string) {
	return p.orch.checkFinishEnforcement()
}
