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

// SetSkillProposer wires the personal-skill proposer (propose_skill) for this
// run (docs/SKILLS.md phase 3).
func (p *InteractivePolicy) SetSkillProposer(sp SkillProposer) { p.orch.setSkillProposer(sp) }

// BeforeToolCall runs the interactive gate chain: ceilings → repeat-call guard →
// email safety (rate-limit/dedup/approval staging) → risky-bash approval →
// preview_email staging → schedule_task staging → suggest_advanced_model staging →
// bundle critical-tool approval staging → memory proposal → note proposal. The
// bash/preview/schedule/suggest/critical gates are inert when no approval sink
// is wired; the proposal gates are inert when no proposer is.
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
	if blocked, msg := p.orch.checkScheduleTaskSafety(toolName, toolCallID, rawInput); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkSuggestAdvancedSafety(toolName, rawInput); blocked {
		return true, msg
	}
	// Bundle-declared critical tools (agent_policy.critical_tools) route through
	// the same approval-card UX in interactive mode. Runs AFTER the tailored
	// gates above so each of those keeps single ownership of its flow.
	if blocked, msg := p.orch.checkCriticalToolApproval(toolName, toolCallID, rawInput); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkMemoryProposal(toolName, rawInput); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkNoteProposal(toolName, rawInput); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkSkillProposal(toolName, rawInput); blocked {
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

// Budget exposes this turn's current cost/token ceilings and accumulated spend
// — the same seam ScheduledPolicy offers (#175). Interactive chat registers
// spawn_subagent too (#1043); the tool sizes a child's sliced ceiling against
// THIS turn's remaining budget, so the parent ceiling stays the hard wall
// across descendants in both modes.
func (p *InteractivePolicy) Budget() BudgetState { return p.orch.budgetState() }

// ChargeChildUsage folds a completed child run's usage into THIS turn's
// accumulated cost/token counters (#1043), so the turn's own ceiling check,
// later sibling spawns, AND the chat cost chip all account for child spend.
func (p *InteractivePolicy) ChargeChildUsage(u RunUsage) { p.orch.chargeChildUsage(u) }

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

// NewDelegatedPolicy builds the run-to-completion bundle for a SPAWNED
// SUB-AGENT (#1043 follow-up). It is the SAME ScheduledPolicy — same gate
// chain, same ceilings, same critical-tool audit gating, same orchestration
// state the confirm_audit tool and usage accounting bind to — with exactly one
// difference: CanFinish does not demand the self-audit ritual (see
// checkFinishEnforcement). This is configuration of the one governed core, not
// a second policy path: a child that unlocks a critical action through
// confirm_audit is still held to its commitments, and the PARENT still has to
// pass its own audit over the delegated work.
func NewDelegatedPolicy(logSession *LogSession, maxIterations int, maxCostUSD float64, maxTotalTokens int) *ScheduledPolicy {
	p := NewScheduledPolicy(logSession, maxIterations, maxCostUSD, maxTotalTokens)
	p.orch.setDelegatedFinish(true)
	return p
}

func (p *ScheduledPolicy) orchestration() *orchestrationState { return p.orch }

// SetNoteProposer wires the admin-notes proposer (propose_note) for this run.
func (p *ScheduledPolicy) SetNoteProposer(np NoteProposer) { p.orch.setNoteProposer(np) }

// SetSkillProposer wires the personal-skill proposer (propose_skill) for this
// run (docs/SKILLS.md phase 3).
func (p *ScheduledPolicy) SetSkillProposer(sp SkillProposer) { p.orch.setSkillProposer(sp) }

// Budget exposes this run's current cost/token ceilings and accumulated spend
// (#175). The spawn_subagent tool reads the PARENT policy's Budget to size a
// child's sliced ceiling against the parent's REMAINING budget — the parent
// ceiling is the hard wall across all descendants. See orchestrationState.
func (p *ScheduledPolicy) Budget() BudgetState { return p.orch.budgetState() }

// ChargeChildUsage folds a completed child run's usage into THIS run's
// accumulated cost/token counters (#175), so the parent's own ceiling check and
// every later sibling spawn account for the child's spend. This is what makes
// the parent ceiling un-breachable by the collective spend of its sub-agents.
func (p *ScheduledPolicy) ChargeChildUsage(u RunUsage) { p.orch.chargeChildUsage(u) }

// BeforeToolCall runs the scheduled gate chain: cost/token ceiling → repeat-call
// guard → critical-tool audit gating → memory-proposal backstop → note proposal.
// The ceiling check is FIRST (matching the interactive policy) so an unattended
// run that blows its budget stops calling tools and ends with what it has,
// rather than running unbounded. The memory-proposal gate is an honest
// "unavailable" backstop (scheduled tasks use remember/recall, not
// propose_memory — interactive-only).
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
	// Honest backstop for propose_memory (#285): the scheduled orchestration never
	// wires a memoryProposer (user memories are interactive-only — scheduled tasks
	// use the remember/recall task-memory tools instead), so this returns
	// MEMORY_PROPOSAL_UNAVAILABLE rather than letting the no-op tool body falsely
	// report "Memory proposal created". The scheduled prompt does not advertise
	// propose_memory, so this only fires if the model calls it unprompted.
	if blocked, msg := p.orch.checkMemoryProposal(toolName, rawInput); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkNoteProposal(toolName, rawInput); blocked {
		return true, msg
	}
	if blocked, msg := p.orch.checkSkillProposal(toolName, rawInput); blocked {
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
