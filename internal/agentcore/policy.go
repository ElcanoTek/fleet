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
	o := newOrchestrationState(nil)
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

// SetAuditProtocolRef names the bundle's self-audit protocol (workspace-relative,
// e.g. "protocols/self-audit.md") in the audit guidance texts, or "" when the
// bundle ships none so the texts never send the model after a missing file.
func (p *InteractivePolicy) SetAuditProtocolRef(ref string) { p.orch.setAuditProtocolRef(ref) }

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
	return runGateChain(p.orch, p.orch.interactiveGates(), toolName, toolCallID, rawInput)
}

// interactiveGates is the interactive gate chain, in order (see
// BeforeToolCall). Bundle-declared critical tools (agent_policy.critical_tools)
// route through the same approval-card UX in interactive mode; that gate runs
// AFTER the tailored gates so each of those keeps single ownership of its flow.
func (o *orchestrationState) interactiveGates() []toolGate {
	return []toolGate{
		{name: "ceilings", check: func(string, string, string) (bool, string) { return o.checkCeilings() }},
		{name: "repeated_call", check: func(n, _, in string) (bool, string) { return o.checkRepeatedCall(n, in) }},
		{name: "email", check: o.checkEmailSafety, admitsSend: true},
		{name: "bash", check: o.checkBashSafety},
		{name: "python", check: func(n, _, in string) (bool, string) { return o.checkPythonSafety(n, in) }},
		{name: "browser", check: o.checkBrowserSafety},
		{name: "connector_studio", check: o.checkConnectorStudioSafety},
		{name: "chat_patrol", check: o.checkChatPatrolSafety},
		{name: "preview_email", check: o.checkPreviewEmailSafety},
		{name: "schedule_task", check: o.checkScheduleTaskSafety},
		{name: "manage_tasks", check: o.checkManageTasksSafety},
		{name: "suggest_advanced_model", check: func(n, _, in string) (bool, string) { return o.checkSuggestAdvancedSafety(n, in) }},
		{name: "critical_tool_approval", check: o.checkCriticalToolApproval},
		{name: "memory_proposal", check: func(n, _, in string) (bool, string) { return o.checkMemoryProposal(n, in) }},
		{name: "note_proposal", check: func(n, _, in string) (bool, string) { return o.checkNoteProposal(n, in) }},
		{name: "skill_proposal", check: func(n, _, in string) (bool, string) { return o.checkSkillProposal(n, in) }},
	}
}

// toolGate is one step of a policy's gate chain. admitsSend marks the gate
// that lets a send_email through and holds its payload in flight
// (claimEmailSend): checkEmailSafety in chat, checkCriticalTool in a
// scheduled run.
type toolGate struct {
	name       string
	check      func(toolName, toolCallID, rawInput string) (bool, string)
	admitsSend bool
}

// runGateChain runs gates in order and returns the first refusal. The gates
// after the send gate are meant never to refuse a send_email (each matches
// only its own tools; TestGateChains_LaterGatesPassEmailThrough pins it),
// because the send gate's admission can already have recorded a durable
// receipt for the send. Should one refuse anyway, the payload the send gate
// held in flight is handed back, so it cannot refuse an identical retry for
// the rest of the turn as "already running".
func runGateChain(o *orchestrationState, gates []toolGate, toolName, toolCallID, rawInput string) (bool, string) {
	admitted := false
	for _, g := range gates {
		if blocked, msg := g.check(toolName, toolCallID, rawInput); blocked {
			if admitted {
				o.releaseAdmittedToolCall(toolName, rawInput)
			}
			return true, msg
		}
		admitted = admitted || g.admitsSend
	}
	return false, ""
}

func (p *InteractivePolicy) RecordToolResult(toolName, rawInput, resultText string, succeeded bool) {
	p.orch.recordToolResult(toolName, rawInput, resultText, succeeded)
}

// CanFinish returns true at round 0 unless the round was cut off by
// max_output_tokens (FinishReasonLength), in which case it opens up to
// maxInteractiveLengthRecoveryRounds continuation rounds so a truncated tool
// call or response can be completed in smaller steps instead of silently
// dropping the incomplete tool call and ending the turn prematurely.
func (p *InteractivePolicy) CanFinish(round int) (bool, []string) {
	return p.orch.checkInteractiveFinishEnforcement(round)
}

// ScheduledPolicy is the run-to-completion policy bundle.
type ScheduledPolicy struct {
	orch *orchestrationState
}

// NewScheduledPolicy builds the scheduled bundle over a session log (the loop
// owns the round cap). maxCostUSD/maxTotalTokens are the per-run ceilings
// (0 = unlimited) — enforced for unattended scheduled / one-shot runs exactly
// as the interactive policy enforces them, so a runaway agent is bounded by the
// configured budget rather than the invoice.
func NewScheduledPolicy(logSession *LogSession, maxCostUSD float64, maxTotalTokens int) *ScheduledPolicy {
	o := newOrchestrationState(logSession)
	o.setRepeatGuardNoun(repeatGuardNounFinishTask)
	o.setCeilings(maxCostUSD, maxTotalTokens)
	return &ScheduledPolicy{orch: o}
}

func (p *ScheduledPolicy) orchestration() *orchestrationState { return p.orch }

// SetAuditProtocolRef names the bundle's self-audit protocol (workspace-relative,
// e.g. "protocols/self-audit.md") in the finish nudge and the BLOCKED /
// rejection texts, or "" when the bundle ships none so the texts never send the
// model after a missing file.
func (p *ScheduledPolicy) SetAuditProtocolRef(ref string) { p.orch.setAuditProtocolRef(ref) }

// SetNoteProposer wires the admin-notes proposer (propose_note) for this run.
func (p *ScheduledPolicy) SetNoteProposer(np NoteProposer) { p.orch.setNoteProposer(np) }

// SetSkillProposer wires the personal-skill proposer (propose_skill) for this
// run (docs/SKILLS.md phase 3).
func (p *ScheduledPolicy) SetSkillProposer(sp SkillProposer) { p.orch.setSkillProposer(sp) }

// NewAuditedScheduledPolicyForTest builds a ScheduledPolicy whose self-audit is
// already requested and confirmed, so a test in another package can exercise
// what runs AFTER finish enforcement (the scheduled driver's verifier) without
// scripting the audit ritual. Test-only: nothing in the runtime calls it.
func NewAuditedScheduledPolicyForTest(logSession *LogSession) *ScheduledPolicy {
	p := NewScheduledPolicy(logSession, 0, 0)
	p.orch.selfAuditRequested, p.orch.selfAuditConfirmedOnce = true, true
	return p
}

// AuditAborted reports an explicit terminal abort. Drivers must preserve this
// failed result, not re-demand the mutations the abort deliberately abandoned.
func (p *ScheduledPolicy) AuditAborted() bool {
	aborted, _, _ := p.orch.auditVerdict()
	return aborted
}

// FinishRefusedForAudit reports whether the last CanFinish refused the round
// for want of a passed self-audit: the "Before finishing: audit …" request,
// or "Audit not confirmed". Both happen only while selfAuditConfirmedOnce is
// false, i.e. before the run's first accepted confirm_audit (a pass, an
// abort, or a refused abort; the flag is never reset), so the closing text of
// such a round is an answer the model gave before its audit, which the
// scheduled driver keeps as part of the run's answer (#1826). A refusal for
// anything else (output cut off, open task-tracker items, an owed critical
// action, an abort's owed summary email) reports false.
func (p *ScheduledPolicy) FinishRefusedForAudit() bool {
	p.orch.mu.Lock()
	defer p.orch.mu.Unlock()
	return p.orch.finishRefusedForAudit
}

// AuditConfirmed reports whether this run's own confirm_audit(success=true) was
// accepted (and not later turned into a terminal abort). The scheduled driver
// requires it before
// letting a verifier outage fail open (#1602): "the audit passed" must mean an
// audit actually ran, not merely that finish enforcement let the run end.
func (p *ScheduledPolicy) AuditConfirmed() bool {
	p.orch.mu.Lock()
	defer p.orch.mu.Unlock()
	return p.orch.selfAuditConfirmedOnce && !p.orch.auditTerminalFailure
}

// BeforeToolCall runs the scheduled gate chain: cost/token ceiling → repeat-call
// guard → critical-tool audit gating → memory-proposal backstop → note proposal.
// The ceiling check is FIRST (matching the interactive policy) so an unattended
// run that blows its budget stops calling tools and ends with what it has,
// rather than running unbounded. The memory-proposal gate is an honest
// "unavailable" backstop (scheduled tasks use remember/recall, not
// propose_memory — interactive-only).
func (p *ScheduledPolicy) BeforeToolCall(toolName, toolCallID, rawInput string) (bool, string) {
	return runGateChain(p.orch, p.orch.scheduledGates(), toolName, toolCallID, rawInput)
}

// scheduledGates is the scheduled gate chain, in order (see BeforeToolCall).
//
// Honest backstop for propose_memory (#285): the scheduled orchestration never
// wires a memoryProposer (user memories are interactive-only — scheduled tasks
// use the remember/recall task-memory tools instead), so its gate returns
// MEMORY_PROPOSAL_UNAVAILABLE rather than letting the no-op tool body falsely
// report "Memory proposal created". The scheduled prompt does not advertise
// propose_memory, so this only fires if the model calls it unprompted.
func (o *orchestrationState) scheduledGates() []toolGate {
	return []toolGate{
		{name: "ceilings", check: func(string, string, string) (bool, string) { return o.checkCeilings() }},
		{name: "repeated_call", check: func(n, _, in string) (bool, string) { return o.checkRepeatedCall(n, in) }},
		{name: "critical_tool", check: o.checkCriticalTool, admitsSend: true},
		{name: "memory_proposal", check: func(n, _, in string) (bool, string) { return o.checkMemoryProposal(n, in) }},
		{name: "note_proposal", check: func(n, _, in string) (bool, string) { return o.checkNoteProposal(n, in) }},
		{name: "skill_proposal", check: func(n, _, in string) (bool, string) { return o.checkSkillProposal(n, in) }},
	}
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
