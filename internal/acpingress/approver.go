package acpingress

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// ingressApprover is the ingress HUMAN-IN-THE-LOOP surface. It implements the
// SAME staging interface the web path wires onto the InteractivePolicy
// (agentcore.ApprovalStager), and resolves the human's decision over an OUTBOUND
// ACP session/request_permission instead of a web approval card.
//
// Governance fidelity — it mirrors the web path's stage-and-block model EXACTLY:
//
//   - In-loop, Stage() is FAST: it stages the approval into the SAME approvals
//     table (the audit record) and returns the standard "APPROVAL_REQUIRED, do
//     NOT retry, wait for the user" block message — byte-identical to the web
//     path. The gated tool does NOT execute in-loop. Stage() does NO network I/O
//     and holds no human-decision wait, so it never stalls the run loop (which
//     calls it under the orchestration mutex) — same as the web stager.
//   - AFTER the turn's run loop completes (the mutex released), the IngressAgent
//     calls ResolvePending: for each approval staged this turn it issues the
//     outbound request_permission, and on approve executes the staged tool ONCE
//     through the SAME governed StagedToolRunner the web approval handler uses,
//     streaming the outcome back as a tool_call_update + appending it to history.
//
// This is the web path's "stage in the turn, resolve out of band" lifecycle,
// with the editor's request_permission standing in for the web approval card +
// its decision endpoint. The ONLY thing that differs is where the human's yes/no
// comes from.
//
// Default-DENY is the contract on every non-approval terminal: timeout, ctx
// cancel, an empty options set, a deny selection, an outbound RPC error, or any
// staging/execution failure. There is no approve-all and no silent allow.
type ingressApprover struct {
	conn      *acp.AgentSideConnection
	sessionID acp.SessionId

	store          ApprovalStore
	runner         StagedToolRunner
	conversationID string
	userEmail      string

	// permTimeout caps how long a single permission request waits for the human
	// before it defaults to DENY. Zero uses DefaultPermissionTimeout.
	permTimeout time.Duration

	mu            sync.Mutex
	pending       []stagedApproval
	pendingMemory []stagedMemory
}

// stagedMemory records one propose_memory proposal the turn staged, resolved
// post-turn by asking the human over ACP (Allow → AcceptMemoryProposal).
type stagedMemory struct {
	proposalID string
	content    string
}

// stagedApproval records one approval the turn staged, so the post-turn pass can
// ask the human and resolve it. autoDismiss marks display-only stages
// (preview_email) the in-loop gate already told the agent need NO approval — the
// post-turn pass resolves them WITHOUT prompting the human.
type stagedApproval struct {
	approvalID  string
	toolName    string
	toolCallID  string
	rawInput    string
	autoDismiss bool
}

// DefaultPermissionTimeout is the default-deny window for an ingress permission
// request when the approver's timeout is unset. Matches the external-runtime
// default so the two human-in-the-loop surfaces behave consistently.
const DefaultPermissionTimeout = 5 * time.Minute

var (
	_ agent.ApprovalStager = (*ingressApprover)(nil)
	_ agent.MemoryProposer = (*ingressApprover)(nil)
)

// Stage is the agentcore.ApprovalStager entrypoint for a critical tool call
// (send_email / risky bash / preview_email). It stages the approval row (the
// audit record) and records it for post-turn resolution, then returns the
// approval id so the policy's in-loop block message threads exactly as the web
// path's does. It performs NO human wait and NO execution here — that happens in
// ResolvePending after the run loop releases the orchestration mutex.
//
// Email materialization: for email tools (send_email / preview_email /
// mcp_<server>_send_email) we inline a workspace content_file into content and
// rewrite relative attachment paths to absolute BEFORE persisting — the SAME
// host-side transform the web stager runs (shared in internal/tools), because
// the email MCP subprocess resolves paths against a different cwd. Both the
// persisted approval row and the post-turn replay (ResolvePending →
// stagedToolRunner) therefore carry resolvable args, so an ingress send_email
// that relies on a workspace content_file / relative attachment now works
// identically to the web path. A missing/oversized content_file fails the Stage
// (fail-closed), which the policy surfaces as a tool-call failure — never a
// silent send of the wrong bytes.
func (a *ingressApprover) Stage(toolName, toolCallID, rawInput string) (string, error) {
	if tools.IsEmailToolName(toolName) {
		inlined, err := tools.MaterializeContentFile(a.conversationID, rawInput)
		if err != nil {
			return "", err
		}
		rewritten, err := tools.MaterializeAttachmentPaths(a.conversationID, inlined)
		if err != nil {
			return "", err
		}
		rawInput = rewritten
	}
	approval, err := a.store.CreateApproval(context.Background(), a.conversationID, a.userEmail, toolName, toolCallID, rawInput)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.pending = append(a.pending, stagedApproval{
		approvalID:  approval.ID,
		toolName:    toolName,
		toolCallID:  toolCallID,
		rawInput:    rawInput,
		autoDismiss: isDisplayOnly(toolName),
	})
	a.mu.Unlock()
	return approval.ID, nil
}

// StageSuggestion handles suggest_advanced_model. Ingress does not surface a
// model-switch card to the editor (model selection is a fleet/web concern, not
// an ACP-host concern), so it is SUPPRESSED with an agent-facing message that
// tells the model not to retry — identical in spirit to the web path's
// suppression branches. Nothing is staged.
func (a *ingressApprover) StageSuggestion(_ string) (string, string, error) {
	return "", "SUGGESTION_SUPPRESSED: model-switch suggestions are not surfaced over the ACP ingress transport. Do not call suggest_advanced_model again — proceed with the current model.", nil
}

// Propose implements agentcore.MemoryProposer: it stages a propose_memory
// proposal (pending user confirmation) into the SAME memories table the web path
// uses and queues it for post-turn resolution over ACP, mirroring the
// stage-then-resolve lifecycle the approval surface uses. The human's Allow/Reject
// arrives over request_permission in ResolvePending (default-DENY); nothing is
// accepted in-loop. Returns the proposal id so the orchestration's MEMORY_PROPOSED
// message threads exactly as the web path's.
func (a *ingressApprover) Propose(content string) (string, error) {
	mem, err := a.store.CreateMemoryProposal(context.Background(), a.userEmail, a.conversationID, content)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.pendingMemory = append(a.pendingMemory, stagedMemory{proposalID: mem.ID, content: content})
	a.mu.Unlock()
	return mem.ID, nil
}

// Note on the staging surfaces:
//
//   - propose_memory: staged via Propose above and resolved over ACP
//     request_permission in ResolvePending (Allow → AcceptMemoryProposal; Reject /
//     timeout / cancel → left pending, source='proposed', mirroring the web path
//     which never deletes on deny). Conversation+user scoped.
//   - propose_note: wired host-side on the Manager (cmd/fleet's notesAdapter,
//     passed as ManagerOptions.NoteProposer), so propose_note over ingress stages
//     into the SAME global admin-notes queue the web path uses — the interactive
//     turn inherits it via RunTurn's TurnConfig.NoteProposer. Note proposals are
//     intentionally GLOBAL (author "agent", un-scoped), unlike memory proposals.

// ResolvePending runs AFTER the turn's run loop completes. For each approval the
// turn staged it asks the human over ACP (or auto-dismisses display-only ones),
// executes approved tools through the governed StagedToolRunner, streams the
// outcome back to the editor, and records the resolution in history + the
// approvals row. Default-DENY on every non-approval terminal. ctx is the turn
// context, so a session Cancel aborts an in-flight permission wait (default-deny).
func (a *ingressApprover) ResolvePending(ctx context.Context, sink *ingressSink) {
	a.mu.Lock()
	pending := a.pending
	pendingMemory := a.pendingMemory
	a.pending = nil
	a.pendingMemory = nil
	a.mu.Unlock()

	for _, p := range pending {
		a.resolveOne(ctx, sink, p)
	}
	for _, m := range pendingMemory {
		a.resolveMemory(ctx, m)
	}
}

// resolveMemory asks the human over ACP whether to save a staged memory proposal.
// On Allow it accepts the proposal (flips it out of the pending queue); on
// Reject / timeout / cancel it leaves the proposal pending (source='proposed'),
// mirroring the web path, which never deletes a proposal on deny. NO tool
// executes — accepting the proposal IS the effect.
func (a *ingressApprover) resolveMemory(ctx context.Context, m stagedMemory) {
	// Reuse askHuman by presenting the proposal as a request_permission; the memory
	// content is surfaced as the raw input so the editor can show what would be saved.
	rawInput, _ := json.Marshal(map[string]string{"content": m.content})
	ask := stagedApproval{
		approvalID: m.proposalID,
		toolName:   "propose_memory",
		toolCallID: "propose_memory-" + m.proposalID,
		rawInput:   string(rawInput),
	}
	if !a.askHuman(ctx, ask) {
		return // default-DENY: leave the proposal pending for the user to act on later.
	}
	// User committed; detach from ctx so a late cancel can't abandon the accept.
	if _, err := a.store.AcceptMemoryProposal(context.WithoutCancel(ctx), a.userEmail, m.proposalID); err != nil {
		log.Printf("acpingress: accept memory proposal %s: %v", m.proposalID, err)
	}
}

func (a *ingressApprover) resolveOne(ctx context.Context, sink *ingressSink, p stagedApproval) {
	// Display-only stages (preview_email): the in-loop gate already told the
	// agent NO approval is needed (Dismiss-only), so do NOT prompt the human —
	// resolve it as a dismissal, mirroring the web preview card's no-send path.
	allowed := false
	if p.autoDismiss {
		allowed = true // "approve" a preview = render/dismiss; StagedToolRunner returns the dismissal text.
	} else {
		allowed = a.askHuman(ctx, p)
	}

	if !allowed {
		if claimed, cerr := a.store.ClaimApproval(ctx, a.userEmail, p.approvalID, "rejected", "User declined over ACP."); cerr != nil {
			log.Printf("acpingress: claim rejected approval %s: %v", p.approvalID, cerr)
		} else if claimed {
			a.recordResolution(ctx, sink, p, "User declined this action.", false)
		}
		return
	}

	// Approved (or auto-dismiss). Claim BEFORE executing so a double-decide
	// cannot run the tool twice — the pending→approved flip is the only atomic gate.
	claimed, err := a.store.ClaimApproval(ctx, a.userEmail, p.approvalID, "approved", "Approved over ACP — executing…")
	if err != nil {
		log.Printf("acpingress: claim approved approval %s: %v", p.approvalID, err)
		return
	}
	if !claimed {
		return // lost the race (already resolved); do not re-fire.
	}

	// Execute the staged tool ONCE through the governed staged-tool kernel. The
	// user committed, so detach from ctx so a late cancel cannot abandon a claimed
	// approval half-executed (the kernel applies its own per-call timeouts). The
	// runner reads only ToolName + ArgsJSON, which we already hold from Stage() —
	// reconstruct the approval row inline rather than re-reading it.
	execCtx := context.WithoutCancel(ctx)
	approval := &store.Approval{
		ID:             p.approvalID,
		ConversationID: a.conversationID,
		UserEmail:      a.userEmail,
		ToolName:       p.toolName,
		ToolCallID:     p.toolCallID,
		ArgsJSON:       p.rawInput,
		Status:         "approved",
	}
	resultText, toolErr := a.runner.RunStagedTool(execCtx, approval)
	isErr := toolErr != nil
	if isErr {
		resultText = fmt.Sprintf("action failed: %v", toolErr)
	}
	if serr := a.store.SetApprovalResult(execCtx, a.userEmail, p.approvalID, resultText); serr != nil {
		log.Printf("acpingress: set approval result %s: %v", p.approvalID, serr)
	}
	a.recordResolution(execCtx, sink, p, resultText, isErr)
}

// recordResolution streams the outcome back to the editor as a tool_call_update
// (so the editor's tool card flips from the in-loop pending/blocked state to the
// real terminal status) AND appends the resolution tool_result to history under
// the original tool_call id (so the next turn's model sees the outcome). Mirrors
// the web approval handler's appendToolResultToHistory + the UI chip update.
func (a *ingressApprover) recordResolution(ctx context.Context, sink *ingressSink, p stagedApproval, text string, isErr bool) {
	callID := resolutionCallID(p.approvalID, p.toolCallID)
	if sink != nil && p.toolCallID != "" {
		sink.toolResult(p.toolCallID, isErr)
	}
	payload, _ := json.Marshal(map[string]any{
		"id":     callID,
		"name":   p.toolName,
		"text":   text,
		"is_err": isErr,
	})
	entry := agent.HistoryEntry{Role: "tool", Type: "tool_result", Content: payload}
	if err := a.store.AppendHistory(ctx, a.conversationID, []agent.HistoryEntry{entry}); err != nil {
		log.Printf("acpingress: append approval resolution: %v", err)
	}
}

// askHuman issues the OUTBOUND request_permission and returns whether the human
// allowed it. Default-DENY on timeout / cancel / no-options / deny / RPC error.
func (a *ingressApprover) askHuman(ctx context.Context, p stagedApproval) bool {
	timeout := a.permTimeout
	if timeout <= 0 {
		timeout = DefaultPermissionTimeout
	}
	decCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req := acp.RequestPermissionRequest{
		SessionId: a.sessionID,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(p.toolCallID),
			Title:      acp.Ptr(permissionTitle(p.toolName)),
			Status:     acp.Ptr(acp.ToolCallStatusPending),
			RawInput:   rawInputMap(p.rawInput),
		},
		// Exactly two options, decided on their own merits. There is no
		// allow_always / "approve all" — every critical action is its own ask.
		Options: []acp.PermissionOption{
			{Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow", OptionId: acp.PermissionOptionId("allow")},
			{Kind: acp.PermissionOptionKindRejectOnce, Name: "Reject", OptionId: acp.PermissionOptionId("reject")},
		},
		Meta: map[string]any{"fleet/approvalId": p.approvalID},
	}

	resp, err := a.conn.RequestPermission(decCtx, req)
	if err != nil {
		// Timeout / cancel / transport error → DENY (fail-closed).
		log.Printf("acpingress: request_permission for %s denied (%v)", p.toolName, err)
		return false
	}
	sel := resp.Outcome.Selected
	if sel == nil {
		// Cancelled outcome (or no selection) → DENY.
		return false
	}
	return string(sel.OptionId) == "allow"
}

// resolutionCallID prefers the agent's original tool_call id (so the editor's
// tool card updates) and falls back to the approval id for server-staged calls
// with no agent-emitted tool_call.
func resolutionCallID(approvalID, toolCallID string) string {
	if toolCallID != "" {
		return toolCallID
	}
	return approvalID
}

// isDisplayOnly reports whether a staged tool is a display-only card (no send
// path, no human yes/no): preview_email. The in-loop gate already told the agent
// such a tool needs no approval, so the post-turn pass must not prompt for one.
func isDisplayOnly(toolName string) bool {
	return toolName == "preview_email"
}

// permissionTitle returns the human-facing prompt title for a critical tool.
func permissionTitle(toolName string) string {
	switch toolName {
	case "bash":
		return "Run a shell command"
	default:
		return fmt.Sprintf("Approve %s", toolName)
	}
}

// rawInputMap decodes a tool's raw JSON input into a generic map for the ACP
// rawInput field; on any parse failure it falls back to a single {input: raw}
// field so the editor still sees something reviewable.
func rawInputMap(rawInput string) map[string]any {
	if rawInput == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(rawInput), &m); err == nil {
		return m
	}
	return map[string]any{"input": rawInput}
}
