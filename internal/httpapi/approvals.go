// Approvals: human-in-the-loop gate for high-risk tool calls.
//
// The agent never calls mcp_sendgrid_send_email directly. Orchestration
// intercepts the call, stages it in the approvals table, and emits a
// tool.approval_required SSE event during the live turn. The assistant
// message finishes with a summary of what it *would* send, and an inline
// card in the UI offers Send / Cancel buttons.
//
// When the user clicks Send, the UI POSTs here. The server calls the
// actual MCP tool (one-shot, outside the agent loop), writes the real
// tool result into both the approvals row and the conversation history,
// and returns it to the UI. No new agent turn runs — this is purely a
// resolution of a staged action.

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/cron"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
	"github.com/ElcanoTek/fleet/internal/webpush"
)

// approvalStager implements agent.ApprovalStager. It persists a pending
// approval and emits the tool.approval_required event on the live SSE sink
// so the frontend renders its approval card immediately.
type approvalStager struct {
	ctx            context.Context
	store          chatStore
	conversationID string
	userEmail      string
	sink           agent.EventSink
	// mcpBroker / mcpCatalog are the credential seam staging uses to
	// pre-validate an email and to resolve mcp_<server>_<tool> identities. They
	// start as the manager's process-wide default-seat pair and are replaced by
	// the live turn scope through BindTurnMCPScope (#167 residual 2) — a
	// named-account turn registers its servers as "<server>_<account>", which
	// the default-seat catalog cannot resolve at all.
	mcpBroker  agentcore.MCPBroker
	mcpCatalog []mcp.ServerTool
	// mcpSeats maps a REGISTERED server name to the public {server, account}
	// selection behind it, so a staged card records the seat an approval must
	// reopen. Populated from the turn scope's own selection.
	mcpSeats map[string]store.ApprovalSeat
	// personaPolicy is the turn's persona tool policy (nil = no narrowing),
	// so a describer call never reaches a tool the persona does not offer.
	personaPolicy *agentcore.PersonaToolPermissions
	// mcpMu guards the four fields above: BindTurnMCPScope runs on the turn
	// goroutine before the loop starts, but Stage can be reached from a
	// parallel tool dispatch.
	mcpMu sync.RWMutex
	// sessionRegistry holds per-conversation pre-approvals (#300). nil disables
	// batch approval (every call stages a card, the prior behavior).
	sessionRegistry *SessionApprovalRegistry
	// globalTimeoutSeconds is the server-wide approval default-deny window
	// (config.ApprovalTimeoutSeconds / FLEET_APPROVAL_TIMEOUT_SECONDS). It is
	// the lowest-priority layer of the resolution chain (#225).
	globalTimeoutSeconds int
	// convTimeoutSeconds is this conversation's per-chat override (nil = none).
	// It sits above the global default and below per-tool manifest overrides.
	convTimeoutSeconds *int
	// convAutoApprove is this conversation's initial per-chat auto-approve setting
	// (conversations.auto_approve). When liveAutoApprove is non-nil, Stage reads
	// the live toggle so toggling Auto-approve mid-turn takes effect immediately.
	convAutoApprove bool
	liveAutoApprove func(context.Context) bool
	// autoApproveInTest auto-approves executable critical tools; handler-only
	// cards still require explicit decisions. A CI/test escape hatch,
	// off by default; see config.Config.AutoApproveInTest.
	autoApproveInTest bool
	// push, when configured, sends the conversation owner a low-detail
	// "approval needed" browser notification the moment a card is staged
	// (#292), so a backgrounded tab still learns the turn is blocked on them.
	// nil = feature off.
	push *webpush.Service
	// bg tracks the detached push send so shutdown waits for it rather than
	// dropping a notification mid-flight. nil is fine (promote_task's stager, and
	// tests that construct a stager directly): the send then runs untracked, which
	// is the pre-existing behavior and touches no store.
	bg *backgroundTracker
	// taskConnectors resolves the conversation's live connector selection when
	// a schedule_task card is staged (ADR-0068), so the scheduled task inherits
	// the connectors chat was running with. nil stages the card without a
	// snapshot (tests, legacy construction).
	taskConnectors func(context.Context) (chatTaskConnectors, error)
	// groupID is the approval group a card joins when its tool is listed in
	// agent_policy.critical_tool_group_approval (approval_group.go): the
	// staging turn's id, so the cards one turn stages share it. "" (promote
	// cards, tests) groups nothing.
	groupID string
	// recordIDs maps a notify-mode call's tool_call_id to the record row
	// RecordAction wrote for it, for a tool in critical_tool_result_cards
	// whose result then settles the record's outcome card
	// (RecordActionResult). Guarded by recordMu.
	recordMu  sync.Mutex
	recordIDs map[string]string
	// modelAllowed reports whether a lockdown conversation may use a model
	// (config.LockdownAllows). A suggestion never offers the advanced model to
	// a lockdown chat whose allow-list excludes it: accepting would pin a slug
	// the next turn refuses. nil allows every model (tests, no config).
	modelAllowed func(model string) bool
	// lockdownOnly is cfg.LockdownOnly: every chat is lockdown, including
	// ones created before it was turned on.
	lockdownOnly bool
	// inputKind identifies the source of the turn (e.g. agent.InputKindChatPatrol)
	// so unattended in-chat patrol turns auto-approve executable session-approvable
	// tools, enforce at least a 24h timeout on any remaining staged cards, and arm
	// resume-after-approval automatically.
	inputKind string
}

var _ agent.MCPScopeBinder = (*approvalStager)(nil)

// BindTurnMCPScope adopts the per-turn credential scope. Everything staging
// does against MCP — resolving the tool's server, pre-validating an email,
// recording the seat — then reflects the account the turn is really running on.
func (a *approvalStager) BindTurnMCPScope(scope agent.TurnMCPScope) {
	seats := make(map[string]store.ApprovalSeat, len(scope.Selection))
	for _, choice := range scope.Selection {
		if choice.Server == "" {
			continue
		}
		seats[agentcore.RegisteredMCPName(choice.Server, choice.Account)] = store.ApprovalSeat{
			Server:  choice.Server,
			Account: choice.Account,
		}
	}
	a.mcpMu.Lock()
	defer a.mcpMu.Unlock()
	if scope.Broker != nil {
		a.mcpBroker = scope.Broker
	}
	if scope.Catalog != nil {
		a.mcpCatalog = scope.Catalog
	}
	a.mcpSeats = seats
	a.personaPolicy = scope.PersonaPolicy
}

// mcpScope reads the staging-time credential context under the lock.
func (a *approvalStager) mcpScope() (agentcore.MCPBroker, []mcp.ServerTool, map[string]store.ApprovalSeat) {
	a.mcpMu.RLock()
	defer a.mcpMu.RUnlock()
	return a.mcpBroker, a.mcpCatalog, a.mcpSeats
}

// seatFor resolves the credential seat behind a staged tool name. A tool whose
// registered server is not in the turn's selection (the transitional
// local-client path, or the synthetic inline http-tools server) records the
// registered name on the default seat — the same seat execution would have
// used before, so nothing regresses when there is no scope to preserve.
func (a *approvalStager) seatFor(toolName string) store.ApprovalSeat {
	_, catalog, seats := a.mcpScope()
	server, _, err := resolveMCPTool(catalog, toolName)
	if err != nil {
		return store.ApprovalSeat{}
	}
	if seat, ok := seats[server]; ok {
		return seat
	}
	return store.ApprovalSeat{Server: server}
}

// previewEmailToolName mirrors the agentcore gate's constant: the display-only
// preview tool whose card has a Dismiss button and nothing to approve.
const previewEmailToolName = "preview_email"

// notifyRecordResultPrefix opens every notify-mode record's result text
// (#1153). It doubles as the marker the reload path uses to tag the row
// `recorded` (the approvals table has no mode column — the prefix is the one
// durable trace of HOW the row resolved), so RecordAction and
// isNotifyRecordResult must stay in lockstep.
const notifyRecordResultPrefix = store.NotifyRecordResultPrefix

// isNotifyRecordResult reports whether a resolved approval row is a
// notify-mode record rather than a human decision.
func isNotifyRecordResult(resultText string) bool {
	return strings.HasPrefix(resultText, notifyRecordResultPrefix)
}

// maxApprovalTimeoutSeconds bounds a per-conversation approval-timeout override
// to a sane ceiling (7d). The POST /conversations/{id}/approval-timeout handler
// rejects anything larger so a typo can't leave a card effectively un-expiring.
const maxApprovalTimeoutSeconds = 604800

// defaultApprovalTimeoutSeconds is the hardcoded fallback at the bottom of the
// resolution chain — used when neither a per-tool, per-conversation, nor a
// positive global value applies. Treating a non-positive global as "use this
// default" (rather than "deny instantly") is the safe reading of a
// mis-set/empty FLEET_APPROVAL_TIMEOUT_SECONDS (#225). 24 hours (86400s), not
// the original 300s/3600s: the card lands whenever the agent reaches it — often
// minutes into a run while the user is multitasking across several chats — so
// short timeouts mostly denied the final, wanted action. Matches the promote
// card's window and the config env default.
const defaultApprovalTimeoutSeconds = 86400

// stagedTaskApprovalTimeoutSeconds is the default-deny window for a card that
// only STAGES a task change (schedule_task, manage_tasks): a day. Such a card
// has no side effect while it is pending — nothing touches the task store until
// it is approved — so default-deny protects nothing, while denying it silently
// drops a change the user asked for.
const stagedTaskApprovalTimeoutSeconds = 86400

// stagesTaskChangeOnly reports whether toolName's card only stages a task or
// connector configuration change: tools whose bodies never execute external
// side effects while pending (handler-only).
func stagesTaskChangeOnly(toolName string) bool {
	return toolName == tools.ScheduleTaskToolName ||
		toolName == tools.ManageTasksToolName ||
		toolName == tools.EnableMCPConnectorsToolName
}

// resolveTimeoutSeconds applies the #225 resolution chain for toolName, highest
// priority first: per-tool manifest override → per-conversation override →
// global env default → hardcoded default. Always returns a positive value.
//
// A card that only stages a task change skips the global and hardcoded
// layers: it waits stagedTaskApprovalTimeoutSeconds, or a per-conversation
// override when that is longer. An explicit per-tool manifest value still
// wins — the operator named that tool.
func (a *approvalStager) isPatrolOrPatrolResume() bool {
	if a.inputKind == agent.InputKindChatPatrol {
		return true
	}
	if a.inputKind == agent.InputKindApprovalResume && a.store != nil {
		if list, err := a.store.ListChatSchedules(a.ctx, a.userEmail, a.conversationID, false); err == nil && len(list) > 0 {
			return true
		}
	}
	return false
}

func (a *approvalStager) resolveTimeoutSeconds(toolName string) int {
	t := a.resolveBaseTimeoutSeconds(toolName)
	if a.isPatrolOrPatrolResume() && t < defaultApprovalTimeoutSeconds {
		return defaultApprovalTimeoutSeconds
	}
	return t
}

func (a *approvalStager) resolveBaseTimeoutSeconds(toolName string) int {
	if t := agentcore.ApprovalTimeoutForTool(toolName); t > 0 {
		return t
	}
	if stagesTaskChangeOnly(toolName) {
		floor := stagedTaskApprovalTimeoutSeconds
		if a.globalTimeoutSeconds > floor {
			floor = a.globalTimeoutSeconds
		}
		if a.convTimeoutSeconds != nil && *a.convTimeoutSeconds > floor {
			return *a.convTimeoutSeconds
		}
		return floor
	}
	if a.convTimeoutSeconds != nil && *a.convTimeoutSeconds > 0 {
		return *a.convTimeoutSeconds
	}
	if a.globalTimeoutSeconds > 0 {
		return a.globalTimeoutSeconds
	}
	return defaultApprovalTimeoutSeconds
}

// expiryUnixFor resolves the timeout for toolName and returns the absolute
// unix-seconds deadline to persist on the approval row (always > 0 here, since
// resolveTimeoutSeconds never returns a non-positive value).
func (a *approvalStager) expiryUnixFor(toolName string) int64 {
	// preview_email never expires: the card is display-only (Dismiss is its
	// only action), so there is no action for default-deny to protect against.
	// Before this, the sweep "auto-denied" previews after the window and wrote
	// "the action was not taken" into history — misleading for the model (the
	// preview WAS displayed) and for the user, whose rendered draft flipped to
	// a timeout notice while they might literally be reading it.
	if toolName == previewEmailToolName {
		return 0
	}
	return time.Now().Unix() + int64(a.resolveTimeoutSeconds(toolName))
}

// RecordAction implements agentcore.ActionRecorder (#1153): it posts a card
// saying an action RAN, rather than one asking whether it may.
//
// The row is created and resolved in the same breath — approved, with the undo
// hint as its result text — so the record survives a page reload and sits in the
// conversation exactly where a normal card would, but with no decision attached
// and no countdown to miss. The undo line is bundle-authored: fleet does not
// know any client's reversal verb and must not invent one, and "we can always
// roll back" is only true in practice if the card says how.
//
// An error here is load-bearing. The caller falls back to a blocking approval
// when this fails, because the entire justification for running without asking
// is that the user still finds out.
func (a *approvalStager) RecordAction(toolName, toolCallID, rawInput, undoHint string) error {
	seat := a.seatFor(toolName)
	// A record is not pending, so its expiry is only a column value; use the
	// same resolution so the row shape is identical to every other approval.
	approval, err := a.store.CreateApproval(a.ctx, a.conversationID, a.userEmail, toolName, toolCallID, rawInput, a.expiryUnixFor(toolName), seat)
	if err != nil {
		return err
	}
	result := notifyRecordResultPrefix + ": this tool is declared notify-mode in the client bundle."
	if hint := strings.TrimSpace(undoHint); hint != "" {
		result += " " + hint
	}
	if err := a.store.ResolveApproval(a.ctx, a.userEmail, approval.ID, "approved", result); err != nil {
		return err
	}
	if toolCallID != "" && agentcore.ApprovalResultCard(toolName) {
		// The call runs next; its result may carry the record's outcome card
		// (RecordActionResult, docs/APPROVAL-RESULT-CARDS.md).
		a.recordMu.Lock()
		if a.recordIDs == nil {
			a.recordIDs = map[string]string{}
		}
		a.recordIDs[toolCallID] = approval.ID
		a.recordMu.Unlock()
	}
	a.emitUserScoped("tool.action_recorded", map[string]any{
		"approval_id": approval.ID,
		"tool":        toolName,
		"summary":     summarizeApprovalInput(toolName, rawInput, a.conversationID),
		"undo_hint":   strings.TrimSpace(undoHint),
		// The exact persisted record text, so the live card and the reloaded
		// card read identically (the client used to synthesize its own).
		"result_text": result,
		"mcp_server":  approval.MCPServer,
		"mcp_account": approval.MCPAccount,
	})
	return nil
}

func (a *approvalStager) isAutoApprove() bool {
	if a.liveAutoApprove != nil {
		return a.liveAutoApprove(a.ctx)
	}
	return a.convAutoApprove
}

// isSelfAddressedPatrolEmail reports whether an outbound email call during an
// unattended in-chat patrol turn is addressed exclusively to the conversation
// owner (with no cc/bcc and inline content that requires no staging-time file
// materialization), so a user who opted into auto-approve on their patrol can
// receive their own patrol digest email without a manual approval card.
// Any inline_attachments such a call carries are checked and copied when it is
// dispatched (agentcore.Deps.PrepareEmailAttachments), as for an approved card.
func (a *approvalStager) isSelfAddressedPatrolEmail(toolName, rawInput string) bool {
	if !a.isPatrolOrPatrolResume() || strings.TrimSpace(a.userEmail) == "" {
		return false
	}
	if !tools.IsEmailToolName(toolName) && !agentcore.IsOutboundEmailTool(toolName) {
		return false
	}
	if toolName == previewEmailToolName {
		return false
	}
	if err := validateEmailHasContent(toolName, rawInput); err != nil {
		return false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(rawInput), &payload); err != nil {
		return false
	}
	if cf, _ := payload["content_file"].(string); strings.TrimSpace(cf) != "" {
		return false
	}
	if att, ok := payload["attachments"]; ok && att != nil {
		return false
	}
	for _, field := range []string{"cc", "bcc", "cc_emails", "bcc_emails"} {
		switch v := payload[field].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return false
			}
		case []any:
			if len(v) > 0 {
				return false
			}
		}
	}
	toRaw, ok := payload["to_email"]
	if !ok {
		toRaw = payload["to"]
	}
	switch v := toRaw.(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(a.userEmail))
	case []any:
		if len(v) != 1 {
			return false
		}
		s, ok := v[0].(string)
		return ok && strings.EqualFold(strings.TrimSpace(s), strings.TrimSpace(a.userEmail))
	default:
		return false
	}
}

func (a *approvalStager) emitUserScoped(name string, payload map[string]any) {
	if a.userEmail != "" {
		payload["user_email"] = a.userEmail
	}
	a.sink.Emit(name, payload)
}

func (a *approvalStager) Stage(toolName, toolCallID, rawInput string) (string, error) {
	// A task prompt the create/edit seam will refuse is refused BEFORE a card
	// exists (#1601): the model learns the error now, and nobody is asked to
	// approve a call that is certain to fail after they click Approve.
	if err := prevalidateStagedTaskPrompt(toolName, rawInput); err != nil {
		return "", err
	}
	// Auto-approve-in-test (#225): a CI/test escape hatch (FLEET_AUTO_APPROVE_IN_TEST,
	// off by default) for pipelines that have no human present and run against a
	// mocked backend. Return the pre-approved sentinel so the tool runs normally,
	// after surfacing a tool.auto_resolved event so an observing stream still sees
	// the decision. Runs before staging so no approval row or card is created.
	// NEVER enable this in production — it bypasses the human-in-the-loop gate.
	if a.autoApproveInTest && !handlerOnlyApproval(toolName) {
		a.emitUserScoped("tool.auto_resolved", map[string]any{
			"tool": toolName,
			"mode": "approve",
			"via":  "auto_approve_in_test",
		})
		return agentcore.PreApprovedSentinel, nil
	}
	// Session pre-approval (#300): if the user previously chose "approve/deny all
	// <tool>" for this conversation, short-circuit the card by returning a
	// sentinel the orchestration gate interprets — pre-approved → run the tool
	// normally; pre-denied → block it. This runs before staging AND before
	// conversation-wide auto-approve so an explicit session deny policy takes
	// precedence over auto-approve.
	//
	// A tool the bundle lists in critical_tool_no_session_approval never takes
	// a session policy, even one registered before that rule existed or by a
	// path that skipped the POST refusal: every call stages its own card.
	if a.sessionRegistry != nil && !handlerOnlyApproval(toolName) && agentcore.SessionApprovalAllowed(toolName) {
		if p, ok := a.sessionRegistry.MatchForUser(a.conversationID, a.userEmail, toolName, rawInput); ok {
			a.emitUserScoped("tool.auto_resolved", map[string]any{
				"tool":    toolName,
				"mode":    p.Mode,
				"pattern": p.Pattern,
			})
			if p.Mode == "deny" {
				return agentcore.PreDeniedSentinel, nil
			}
			return agentcore.PreApprovedSentinel, nil
		}
	}
	// Per-conversation auto-approve (or an unattended in-chat patrol turn):
	// executable critical tools run without parking on an approval card.
	// Handler-only tools (schedule_task, manage_tasks, preview_email,
	// suggest_advanced_model), outbound email (send_email, send_template_email),
	// and bundle-declared critical_tool_no_session_approval tools still stage an
	// explicit card.
	if a.isAutoApprove() && !handlerOnlyApproval(toolName) && agentcore.SessionApprovalAllowed(toolName) &&
		((!tools.IsEmailToolName(toolName) && !agentcore.IsOutboundEmailTool(toolName)) || a.isSelfAddressedPatrolEmail(toolName, rawInput)) {
		a.emitUserScoped("tool.auto_resolved", map[string]any{
			"tool": toolName,
			"mode": "approve",
			"via":  "conversation_auto_approve",
		})
		return agentcore.PreApprovedSentinel, nil
	}
	// For email tools that accept content_file, inline the file bytes
	// now. run_python writes to workspace/<convID>/, but the Go server
	// and the sendgrid MCP process each have their own cwd, so a bare
	// filename the agent passes never resolves downstream — the user
	// sees the filename rendered in the preview card, and the MCP call
	// errors out with "Content file not found" when they click Send.
	// Materializing here (the one place that has convID + the file is
	// still on disk) closes both gaps at once. See the email-delivery incident logs.
	if tools.IsEmailToolName(toolName) {
		inlined, err := tools.MaterializeContentFile(a.conversationID, rawInput)
		if err != nil {
			return "", err
		}
		rawInput = inlined
		// Same cwd-mismatch problem as content_file: the sendgrid MCP
		// subprocess does os.path.abspath against ITS cwd, so a bare
		// filename like "chart.png" written by run_python into
		// workspace/<convID>/ never resolves at send time. Rewrite
		// relative attachment paths to absolute now so the args
		// replayed post-approval carry resolvable paths.
		rewritten, err := tools.MaterializeAttachmentPaths(a.conversationID, rawInput)
		if err != nil {
			return "", err
		}
		rawInput = rewritten
		// Hard-reject empty content. Without this, preview_email stages a
		// card with an empty iframe (user sees nothing and reports "I
		// can't see the preview"), and send_email stages an approval
		// that the actual SendGrid call will reject AFTER the user has
		// clicked Send — wasting their time. The agent gets a clear
		// retry signal instead. See logs/Email-Report-Analysis-and-Summary
		// for the failure mode this catches.
		if err := validateEmailHasContent(toolName, rawInput); err != nil {
			return "", err
		}
		if broker, _, _ := a.mcpScope(); broker != nil {
			if err := a.prevalidateEmail(toolName, rawInput); err != nil {
				return "", err
			}
		}
	}

	// A scheduled task inherits this conversation's connectors (ADR-0068): snap
	// them into the staged args now so the card shows exactly what the task
	// will be able to reach and the approval creates exactly that.
	if toolName == tools.ScheduleTaskToolName {
		enriched, err := a.attachTaskConnectors(rawInput)
		if err != nil {
			return "", err
		}
		rawInput = enriched
	}

	// A bundle-declared describer turns the call into a readable card
	// (approval_card.go). Bounded and best-effort: any failure leaves the
	// generic arguments card, and staging goes on either way. It runs BEFORE
	// the supersede below: that rejects the previous card irreversibly, so a
	// turn stopped during the describer's few seconds must not leave the user
	// with neither the old card nor its replacement.
	cardJSON, cardFallback := a.describeApproval(toolName, rawInput)

	// Supersede any older pending approvals for this same tool in this
	// conversation. Keeps the UI clean when the agent retries — e.g.
	// a preview_email that staged with a broken body, then re-staged
	// with the real body. Without this, both cards render and the
	// user sees the broken one linger forever. Best-effort: a failure
	// here shouldn't block the new approval.
	//
	// For schedule_task, each call proposes creating an independent scheduled
	// task (Name is optional and non-unique, and tool.approval_superseded
	// clears all pending cards for the tool on live clients), so pending
	// schedule_task cards are never auto-superseded by subsequent calls.
	if toolName != tools.ScheduleTaskToolName {
		if n, err := a.store.SupersedePendingApprovals(a.ctx, a.userEmail, a.conversationID, toolName); err != nil {
			slog.Error("supersede approvals failed", "tool", toolName, "conv_id", a.conversationID, "err", err)
		} else if n > 0 {
			// Tell the UI to mark the old card as resolved. The client
			// already handles rejected approvals — this just nudges any
			// open stream to refresh its approval state.
			a.emitUserScoped("tool.approval_superseded", map[string]any{
				"tool":  toolName,
				"count": n,
			})
		}
	}

	seat := a.seatFor(toolName)
	approval, err := a.store.CreateApproval(a.ctx, a.conversationID, a.userEmail, toolName, toolCallID, rawInput, a.expiryUnixFor(toolName), seat)
	if err != nil {
		return "", err
	}
	// A tool in critical_tool_resume: settling this card starts the turn that
	// carries on (approval_resume.go). Armed before the card is announced.
	a.armApprovalResume(approval)
	// A tool in critical_tool_group_approval: the card joins this turn's
	// approval group, so the web can render the turn's cards as one.
	a.joinApprovalGroup(approval)
	// critical_tool_narrowable_args: freeze the argument this card narrows
	// now, so the card, its reloads and the decision never read a policy
	// that changed after it was shown (docs/APPROVAL-NARROWING.md).
	if arg := narrowableArgAtStaging(toolName, rawInput); arg != "" {
		if ok, err := a.store.SetApprovalNarrowableArg(a.ctx, a.userEmail, approval.ID, arg); err != nil || !ok {
			slog.Error("approval narrowing: freeze the argument failed", "approval_id", approval.ID, "ok", ok, "err", err)
		} else {
			approval.NarrowableArg = arg
		}
	}
	if cardJSON != "" {
		if ok, err := a.store.SetApprovalCard(a.ctx, a.userEmail, approval.ID, cardJSON); err != nil || !ok {
			slog.Error("approval card: store card failed", "approval_id", approval.ID, "ok", ok, "err", err)
			cardFallback = cardFallbackStore
		} else {
			approval.CardJSON = cardJSON
		}
	}
	if cardFallback != "" {
		// Observable without reading server logs: the card a declared
		// describer should have produced fell back to the generic one.
		a.emitUserScoped("tool.approval_card_fallback", map[string]any{
			"approval_id": approval.ID,
			"tool":        toolName,
			"reason":      cardFallback,
		})
	}

	a.emitUserScoped("tool.approval_required", approvalRequiredEvent(approval, rawInput, a.conversationID))
	a.firePushNotification(toolName)
	return approval.ID, nil
}

// firePushNotification sends the "approval needed" Web Push (#292) to the
// conversation owner, fire-and-forget: a detached, time-bounded goroutine so
// the turn is never blocked on a push relay, with the error only logged. The
// payload is low-detail by design — the TOOL NAME only, never the staged
// arguments. No-op when push is unconfigured or the approval trigger flag
// (FLEET_PUSH_ON_APPROVAL_REQUEST) is off.
func (a *approvalStager) firePushNotification(toolName string) {
	if !a.push.ApprovalPushEnabled() { // nil-safe
		return
	}
	push, email, convID := a.push, a.userEmail, a.conversationID
	send := func() {
		// Independent of the turn context on purpose: the staged card outlives
		// the turn, so the notification should too.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := push.NotifyApprovalRequired(ctx, email, convID, toolName); err != nil {
			logging.From(ctx).Error("push: approval notification failed", "tool", safe.LogSafe(toolName), "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
		}
	}
	if a.bg != nil {
		a.bg.Go("httpapi.approval_push", send)
		return
	}
	go send()
}

// validateEmailHasContent rejects a preview_email / send_email call
// whose body is empty after content_file materialization. Returns an
// agent-readable error pointing at the missing field. Empty here means
// either content was never set OR content_file pointed at an empty
// file. We deliberately don't trim-whitespace the body itself — a
// whitespace-only email is probably wrong but isn't our call to make
// at the staging layer; SendGrid will accept and the operator can
// reject in the preview card. We DO trim the parsed string before
// the empty check so a single space ` ` doesn't sneak through as
// "non-empty" when the agent likely meant nothing.
//
// rawInput must already have run through materializeContentFile so
// content_file → content inlining has happened.
func validateEmailHasContent(toolName, rawInput string) error {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(rawInput), &parsed); err != nil {
		// Non-JSON: nothing useful to check. Stage and let downstream
		// handle the error — same behavior as prevalidateEmail.
		return nil //nolint:nilerr // intentional: a non-JSON body is not a validation failure here; downstream handles the malformed input.
	}
	if strings.TrimSpace(firstString(parsed, "content")) != "" {
		return nil
	}
	return fmt.Errorf(
		"%s called without an email body. Pass `content` (HTML/text string) "+
			"or `content_file` (path to a file in your workspace, e.g. "+
			"`email_draft.html` or `/opt/chat/workspace/<convID>/email_draft.html`). "+
			"Both are empty in this call",
		toolName,
	)
}

// memoryProposer implements agent.MemoryProposer. It persists a pending
// memory proposal and emits a memory.proposed SSE event so the frontend
// renders a Save/Don't Save card. The conversationID scopes the proposal
// so loadConversation can re-hydrate it on focus/refresh — without that,
// the visibilitychange handler's auto-refetch wipes the card.
type memoryProposer struct {
	ctx            context.Context
	store          chatStore
	conversationID string
	userEmail      string
	sink           agent.EventSink
	// origin records WHO is proposing (#515 provenance): "tool" for the
	// agent's propose_memory calls, "auto" for the post-turn extractor.
	origin string
}

func (m *memoryProposer) Propose(content, kind string) (string, error) {
	return m.propose(store.MemoryProposalParams{Content: content, Kind: kind, Origin: m.origin}, "")
}

// propose is the shared core. supersededContent (display-only) is the CURRENT
// text of the memory a supersede claim targets, echoed onto the SSE payload so
// the card can show "replaces: …" without a second fetch.
func (m *memoryProposer) propose(p store.MemoryProposalParams, supersededContent string) (string, error) {
	p.Origin = m.origin
	memory, err := m.store.CreateMemoryProposal(m.ctx, m.userEmail, m.conversationID, p)
	if err != nil {
		return "", err
	}
	payload := map[string]any{
		"proposal_id": memory.ID,
		"content":     memory.Content,
		"kind":        memory.Kind,
	}
	if m.userEmail != "" {
		payload["user_email"] = m.userEmail
	}
	if memory.Supersedes != "" {
		payload["supersedes_id"] = memory.Supersedes
		payload["supersedes_content"] = excerpt(supersededContent, 200)
	}
	m.sink.Emit("memory.proposed", payload)
	return memory.ID, nil
}

// excerpt clamps display text for a card payload. Counts RUNES, not bytes
// (like displayArgValue), so a multibyte excerpt is never cut mid-character
// into a replacement glyph on the card.
func excerpt(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxChars {
		return string(r[:maxChars]) + "…"
	}
	return s
}

// StageSuggestion stages a suggest_advanced_model approval the chat model
// asked for, if the per-conversation gate allows it. Suppression rules
// (stageSuggestion):
//
//   - Conversation already pinned to ADVANCED → skip; the user is already
//     there and a card would be silly.
//   - A prior suggestion is still pending → skip; a card is already up.
//   - A prior suggestion was dismissed, or was accepted and the user has since
//     moved off the advanced model, and fewer than
//     agentcore.SuggestAdvancedCooldownTurns user turns have passed since it
//     → skip (cooldown). An accepted suggestion used to suppress the
//     conversation for good, so a user who switched back to the fast model
//     for a while was never offered the advanced one again.
//
// The agent-facing message reflects the suppression reason so the model
// knows not to retry.
func (a *approvalStager) StageSuggestion(reason string) (string, string, error) {
	id, msg, _, err := a.stageSuggestion(reason, suggestionSourceModel)
	return id, msg, err
}

// StageHostSuggestion stages a card fleet decided on itself, from an
// end-of-turn struggle signal or the classifier (docs/CHAT-ESCALATION.md),
// under the same gate as a model-requested one. It reports whether a card was
// staged; a suppressed suggestion is not an error.
func (a *approvalStager) StageHostSuggestion(source, reason string) (bool, error) {
	_, _, staged, err := a.stageSuggestion(reason, source)
	return staged, err
}

func (a *approvalStager) stageSuggestion(reason, source string) (string, string, bool, error) {
	id, msg, err := a.stageSuggestionGated(reason, source)
	switch {
	case err != nil:
		metrics.RecordModelEscalation(source, "error")
	case id == "":
		metrics.RecordModelEscalation(source, "suppressed")
	default:
		metrics.RecordModelEscalation(source, "staged")
	}
	return id, msg, id != "", err
}

func (a *approvalStager) stageSuggestionGated(reason, source string) (string, string, error) {
	advancedModel := agentcore.CurrentAdvancedModel()
	conv, err := a.store.Get(a.ctx, a.userEmail, a.conversationID)
	if err != nil {
		return "", "", fmt.Errorf("lookup conversation: %w", err)
	}
	if conv != nil && conv.Model == advancedModel {
		return "", "SUGGESTION_SUPPRESSED: this conversation is already pinned to the advanced model. Do not call suggest_advanced_model again — proceed with the user's request.", nil
	}
	if conv != nil && (conv.Lockdown || a.lockdownOnly) && a.modelAllowed != nil && !a.modelAllowed(advancedModel) {
		return "", "SUGGESTION_SUPPRESSED: this is a lockdown conversation and its allowed models do not include the advanced model. Do not call suggest_advanced_model again — keep working with the current model.", nil
	}
	if a.inputKind != "" {
		return "", "SUGGESTION_SUPPRESSED: this is an unattended background turn with no live user at the keyboard. Do not call suggest_advanced_model — proceed with the current model.", nil
	}

	prior, err := a.store.LatestApprovalByTool(a.ctx, a.conversationID, tools.SuggestAdvancedModelToolName)
	if err != nil {
		return "", "", fmt.Errorf("lookup prior suggestion: %w", err)
	}
	if prior != nil {
		switch prior.Status {
		case "pending":
			return "", "SUGGESTION_SUPPRESSED: a model-switch suggestion is already pending the user's response. Do not stage another.", nil
		case "approved", "rejected":
			if strings.HasPrefix(prior.ResultText, "Superseded ") {
				break
			}
			// The cooldown runs from the user's answer, not from when the card
			// appeared: messages sent while it sat pending are not "after" it.
			// Seconds alone cannot order a message against an answer in the
			// same second, so the store counts from the answer's history note,
			// in message order. Rows resolved before resolved_at was recorded
			// fall back to created_at.
			var n int64
			if prior.ResolvedAt > 0 {
				n, err = a.store.CountUserMessagesAfterResolution(a.ctx, a.conversationID, resolutionCallID(prior), prior.ResolvedAt)
			} else {
				n, err = a.store.CountUserMessagesAfterTimestamp(a.ctx, a.conversationID, prior.CreatedAt)
			}
			if err != nil {
				return "", "", fmt.Errorf("count user turns: %w", err)
			}
			if n < int64(agentcore.SuggestAdvancedCooldownTurns) {
				what := "dismissed"
				if prior.Status == "approved" {
					what = "answered"
				}
				return "", fmt.Sprintf(
					"SUGGESTION_SUPPRESSED: the user %s a model-switch suggestion %d user turn(s) ago; cooldown is %d turn(s). Do not stage another suggestion right now — keep working with the current model.",
					what, n, agentcore.SuggestAdvancedCooldownTurns,
				), nil
			}
		}
	}

	// Persist the reason as the args payload so the approval card can
	// render it on page reload and the audit trail is intact. source records
	// who asked, for the card and the escalation metrics.
	rawInput, err := json.Marshal(map[string]any{"reason": reason, "recommend_model": advancedModel, "source": source})
	if err != nil {
		return "", "", fmt.Errorf("encode reason: %w", err)
	}

	// Supersede any older pending cards for this same tool so the UI
	// stays clean if a rare race writes two in quick succession.
	if n, err := a.store.SupersedePendingApprovals(a.ctx, a.userEmail, a.conversationID, tools.SuggestAdvancedModelToolName); err != nil {
		slog.Error("supersede suggestions failed", "conv_id", a.conversationID, "err", err)
	} else if n > 0 {
		a.emitUserScoped("tool.approval_superseded", map[string]any{
			"tool":  tools.SuggestAdvancedModelToolName,
			"count": n,
		})
	}

	// suggest_advanced_model has no agent-emitted tool_call to thread
	// back to — it's a server-staged card. Empty toolCallID is fine;
	// resolutionCallID falls back to the approval id at write time.
	// A model-switch suggestion is server-staged and calls no MCP server, so it
	// records no credential seat.
	approval, err := a.store.CreateApproval(a.ctx, a.conversationID, a.userEmail,
		tools.SuggestAdvancedModelToolName, "", string(rawInput), a.expiryUnixFor(tools.SuggestAdvancedModelToolName),
		store.ApprovalSeat{})
	if err != nil {
		return "", "", err
	}

	a.emitUserScoped("tool.approval_required", approvalRequiredEvent(approval, string(rawInput), a.conversationID))

	msg := fmt.Sprintf(
		"SUGGESTION_DISPLAYED: the user is now seeing your model-switch suggestion (suggestion_id=%s). The card has three actions — Switch & retry (default), Just switch, Dismiss — and the user picks. Do NOT call suggest_advanced_model again. Briefly summarize what you've done so far and stop iterating; the user's choice will arrive on the next turn.",
		approval.ID,
	)
	return approval.ID, msg, nil
}

// prevalidateEmail calls the MCP validate_email_content tool to catch
// structural errors or unresolved tokens before the approval row is created.
// If the MCP tool itself errors, the error is logged and staging continues;
// the real send path will still validate. This prevents broken emails from
// reaching the user's approval card.
func (a *approvalStager) prevalidateEmail(sourceToolName, rawInput string) error {
	var args map[string]any
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return nil // can't parse, let downstream handle
	}
	content := firstString(args, "content")
	subject := firstString(args, "subject")
	if content == "" {
		return nil // nothing to validate, let downstream handle
	}

	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()

	broker, catalog, _ := a.mcpScope()
	server, err := resolveEmailValidationServer(catalog, sourceToolName)
	if err != nil {
		logging.From(ctx).Error("prevalidateEmail: resolve source tool failed", "err", err)
		return nil
	}
	// The email is staged by a chat turn, so a validate_email_content the
	// bundle hides from interactive chat (agent_policy.interactive_hidden_tools)
	// is not called, exactly as a hidden approval-card describer is not
	// (approval_card.go): this host-side call must not reach what the model
	// could not. Prevalidation is best-effort, so it is skipped like an
	// unreachable validator and the card is staged unvalidated.
	if full := agentcore.MCPToolName(server, "validate_email_content"); agentcore.InteractiveHiddenTool(full) {
		logging.From(ctx).Info("prevalidateEmail: tool is hidden from interactive chat (interactive_hidden_tools); not called", "tool", full)
		return nil
	}
	text, isError, err := broker.CallMCP(ctx, server, "validate_email_content", map[string]interface{}{
		"content": content,
		"subject": subject,
	})
	if err != nil {
		logging.From(ctx).Error("prevalidateEmail: validation tool error", "err", err)
		return nil
	}
	if isError {
		logging.From(ctx).Error("prevalidateEmail: validation tool reported an error")
		return nil
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}

	var vr validationResult
	if err := json.Unmarshal([]byte(text), &vr); err != nil {
		logging.From(ctx).Error("prevalidateEmail: unmarshal validation result failed", "err", err)
		return nil
	}
	if !vr.Valid && len(vr.Errors) > 0 {
		return fmt.Errorf("email validation failed:\n  - %s", strings.Join(vr.Errors, "\n  - "))
	}
	return nil
}

// resolveEmailValidationServer keeps an MCP send's validation on its source
// server. Native preview_email/send_email calls have no server prefix, so they
// keep the historical deterministic behavior of choosing the
// lexicographically first server that advertises validate_email_content.
func resolveEmailValidationServer(catalog []mcp.ServerTool, sourceToolName string) (string, error) {
	if strings.HasPrefix(sourceToolName, "mcp_") {
		server, _, err := resolveMCPTool(catalog, sourceToolName)
		if err != nil {
			return "", err
		}
		for _, st := range catalog {
			if st.ServerName == server && st.Tool.Name == "validate_email_content" {
				return server, nil
			}
		}
	}
	best := ""
	for _, st := range catalog {
		if st.Tool.Name == "validate_email_content" && (best == "" || st.ServerName < best) {
			best = st.ServerName
		}
	}
	if best == "" {
		return "", errors.New("validate_email_content is not in the MCP catalog")
	}
	return best, nil
}

type validationResult struct {
	Valid    bool     `json:"valid"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

// frozenArgsMaxBytes bounds frozen_args as fleet sends it: the decoded
// arguments re-encoded (which is what rides the SSE event and the GET). A
// larger snapshot is complete:false so clients refuse informed consent;
// execution still uses the stored row. Bounding the re-encoded form, not the
// raw ArgsJSON, keeps what clients receive within 1 MiB however the model
// escaped its JSON, and keeps every list a card may narrow reviewable: the
// largest (500 values of 200 characters outside the BMP) is about 1.2 MiB
// when written with surrogate-pair escapes but about 400 KiB re-encoded.
const frozenArgsMaxBytes = 1 << 20

// approvalArgsMaxDecodeBytes bounds the raw ArgsJSON fleet decodes for a
// card's review (frozenApprovalArgs) and its narrowing (stagedNarrowable):
// room for the largest narrowable list in its worst supported encoding
// (about 1.2 MiB) and the call's other arguments. Larger arguments are
// neither reviewable in full nor narrowable; the card approves whole.
const approvalArgsMaxDecodeBytes = 4 << 20

var errTrailingJSON = errors.New("trailing JSON")

// decodeJSONNumbers decodes one JSON value with json.Number so integers above
// 2^53 and exponent literals survive. A second token is rejected so this is
// not more lenient than json.Unmarshal.
func decodeJSONNumbers(raw []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	var extra json.RawMessage
	switch err := dec.Decode(&extra); err {
	case io.EOF:
		return nil
	case nil:
		return errTrailingJSON
	default:
		return err
	}
}

// frozenApprovalArgs is the complete execution-argument review for every
// staged tool. ArgsJSON is the source of truth (model-authored arguments the
// handler will replay). Broker-resolved credentials are never copied in.
// complete is false when the snapshot is missing, unparseable, trailing,
// typed JSON null, not an object, over approvalArgsMaxDecodeBytes raw, or
// over frozenArgsMaxBytes re-encoded — clients fail closed.
func frozenApprovalArgs(rawInput string) map[string]any {
	incomplete := map[string]any{"complete": false}
	if len(rawInput) > approvalArgsMaxDecodeBytes {
		return incomplete
	}
	var args any
	if err := decodeJSONNumbers([]byte(rawInput), &args); err != nil {
		return incomplete
	}
	obj, ok := args.(map[string]any)
	if !ok || obj == nil {
		return incomplete
	}
	if encoded, err := json.Marshal(obj); err != nil || len(encoded) > frozenArgsMaxBytes {
		return incomplete
	}
	return map[string]any{"complete": true, "args": obj}
}

// resolveThinkingConfig computes the effective Claude extended-thinking config
// for a turn (#220): a per-conversation override wins; otherwise the global
// FLEET_DEFAULT_THINKING_BUDGET_TOKENS default applies. Returns nil (thinking
// off) when neither is set.
//
//   - override.Enabled == false → an explicit per-chat opt-out that overrides a
//     global default (returns a disabled config, NOT nil, so it suppresses the
//     default).
//   - override.Enabled == true with budget 0 → fall back to the global default
//     budget; the producer clamps to Claude's minimum, so an enabled chat always
//     thinks even when neither budget is set.
func resolveThinkingConfig(override *store.ThinkingConfig, defaultBudget int) *agentcore.ThinkingConfig {
	if override != nil {
		if !override.Enabled {
			return &agentcore.ThinkingConfig{Enabled: false}
		}
		budget := override.BudgetTokens
		if budget <= 0 {
			budget = defaultBudget
		}
		return &agentcore.ThinkingConfig{Enabled: true, BudgetTokens: budget}
	}
	return agentcore.ThinkingConfigForBudget(defaultBudget)
}

// ── HTTP handler: POST /conversations/{id}/approvals/{approvalId} ───────

// approvalRequest is the body the UI sends when the user clicks Send,
// Cancel, or — for suggest_advanced_model — Switch & retry / Just
// switch / Dismiss. The `action` field is optional and only inspected
// for tool kinds that distinguish multiple positive resolutions
// (currently just suggest_advanced_model). For send_email / preview_email /
// bash the boolean `approved` alone is authoritative.
type approvalRequest struct {
	Approved bool   `json:"approved"`
	Action   string `json:"action,omitempty"` // "switch_and_retry" | "switch_only" | "dismiss"
	// Scope (#300) extends a one-off decision to the rest of the session:
	// "" / "once" → just this call (default); "session" → all calls to this tool;
	// "pattern" → calls whose args match Pattern ("argName=glob"). Combined with
	// Approved: approve+session pre-approves, !approve+session pre-denies.
	Scope   string `json:"scope,omitempty"`
	Pattern string `json:"pattern,omitempty"`
	// Edits carries inline changes from the schedule_task approval card: the
	// user reviews the staged proposal and may adjust name/prompt/cron before
	// approving, instead of blindly accepting or starting over. nil field =
	// keep the staged value. Ignored for every other tool kind.
	Edits *scheduleTaskEdits `json:"edits,omitempty"`
	// ApprovedValues narrows the approval (docs/APPROVAL-NARROWING.md): the
	// values of the tool's declared narrowable list argument the person
	// approved, a non-empty subset of the staged ones. Absent (nil) approves
	// the call whole. Only with approved=true and scope once; anything else is
	// refused before a claim.
	ApprovedValues approvedValuesField `json:"approved_values"`
	// NarrowedArg is the argument the card showed as narrowable
	// (`narrowable.arg`), required with ApprovedValues: the narrowing is
	// refused unless it is still the argument the tool narrows.
	NarrowedArg string `json:"narrowed_arg,omitempty"`
}

// sanitizeTaskTagSlice normalizes common agent separator characters (':' and
// '_', e.g. from serialization keys like "pages:merrell-elc07361" or snake_case
// "daily_refresh") to '-' so they pass models.NormalizeAndValidateTags without
// failing after the user clicks Approve.
func sanitizeTaskTagSlice(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	r := strings.NewReplacer(":", "-", "_", "-")
	out := make([]string, len(tags))
	for i, t := range tags {
		out[i] = r.Replace(strings.TrimSpace(t))
	}
	return out
}

// prevalidateStagedTaskPrompt checks the prompt, cron, and tags of a
// schedule_task or manage_tasks call before a card is staged (#1601). The
// approved call reaches storage.EnqueueTaskAs / UpdateEditableTask, which
// refuse a malformed EXECUTION REQUIREMENTS declaration, an unparseable cron
// expression, or invalid tags; this moves those refusals ahead of the card so
// the agent can fix them immediately in the same turn. Any other tool, args
// that do not parse (the card and the executor report those), and a
// manage_tasks call that changes no prompt/cron/tags pass.
func prevalidateStagedTaskPrompt(toolName, rawInput string) error {
	if toolName != tools.ScheduleTaskToolName && toolName != tools.ManageTasksToolName {
		return nil
	}
	var args struct {
		Prompt     string   `json:"prompt"`
		Cron       string   `json:"cron"`
		Tags       []string `json:"tags"`
		AddTags    []string `json:"add_tags"`
		RemoveTags []string `json:"remove_tags"`
	}
	// Best effort: args that do not parse leave the fields empty, which
	// validates, and the card and the executor report them.
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return nil //nolint:nilerr // intentional: unparseable JSON is handled by the card and executor
	}
	if err := models.ValidateExecutionRequirements(args.Prompt); err != nil {
		return &agentcore.StageRefusedError{Reason: fmt.Sprintf("%v. Nothing was staged for approval: fix the prompt's EXECUTION REQUIREMENTS line and call %s again", err, toolName)}
	}
	if cronExpr := strings.TrimSpace(args.Cron); cronExpr != "" {
		if _, err := cron.ParseStandard(cronExpr); err != nil {
			return &agentcore.StageRefusedError{Reason: fmt.Sprintf("invalid cron expression %q: %v. Nothing was staged for approval: fix the cron expression and call %s again", cronExpr, err, toolName)}
		}
	}
	for _, tagSet := range [][]string{args.Tags, args.AddTags, args.RemoveTags} {
		if _, err := models.NormalizeAndValidateTags(sanitizeTaskTagSlice(tagSet)); err != nil {
			return &agentcore.StageRefusedError{Reason: fmt.Sprintf("invalid task tags: %v. Nothing was staged for approval: fix the tags (allowed characters: a-z, 0-9, '-', '.') and call %s again", err, toolName)}
		}
	}
	return nil
}

// Handler-only tools have no runnable tool body. A session sentinel would be
// mistaken for an approval id instead of creating a task or changing a model.
func handlerOnlyApproval(tool string) bool {
	switch tool {
	case tools.ScheduleTaskToolName, tools.ManageTasksToolName, tools.EnableMCPConnectorsToolName, "preview_email", tools.SuggestAdvancedModelToolName:
		return true
	default:
		return false
	}
}

// Handler-only terminal policies must match frozen argument strings, never
// display aliases or synthetic summary fields. These tools' arguments contain
// user-authored task/email/model data, not broker credentials. Executable tools
// continue to match their arguments exclusively in the server registry.
func handlerApprovalPatternArgs(tool, raw string) map[string]string {
	if !handlerOnlyApproval(tool) {
		return nil
	}
	var args map[string]any
	if json.Unmarshal([]byte(raw), &args) != nil {
		return nil
	}
	result := make(map[string]string)
	for k, v := range args {
		if s, ok := v.(string); ok {
			result[k] = s
		}
	}
	return result
}

// scheduleTaskEdits is the editable subset of a staged schedule_task call —
// exactly the fields the approval card displays. Applied over the staged args
// BEFORE re-validation, so an edited cron passes the same pure checks the gate
// applied at stage time.
type scheduleTaskEdits struct {
	Name   *string `json:"name,omitempty"`
	Prompt *string `json:"prompt,omitempty"`
	Cron   *string `json:"cron,omitempty"`
}

// maybeRegisterSessionPolicy records a session-scoped pre-approval/denial (#300)
// when the user picked a non-"once" scope on the approval card. In-memory only
// (lost on restart, by design). Logged as the audit record (this POST has no
// live SSE buffer to emit into — the staging turn has finished).
func (s *Server) maybeRegisterSessionPolicy(convID, user, toolName string, req approvalRequest) {
	scope := strings.TrimSpace(req.Scope)
	if scope == "" || scope == "once" || s.sessionApprovals == nil {
		return
	}
	// handleApproval already refused this scope for a no-session tool; this
	// keeps the registry clean if a future caller skips that check.
	if !agentcore.SessionApprovalAllowed(toolName) {
		return
	}
	mode := "approve"
	if !req.Approved {
		mode = "deny"
	}
	pattern := ""
	switch {
	case strings.HasPrefix(scope, "pattern:"):
		pattern = strings.TrimSpace(strings.TrimPrefix(scope, "pattern:"))
	case scope == "pattern":
		pattern = strings.TrimSpace(req.Pattern)
	}
	s.sessionApprovals.RegisterForUser(convID, user, toolName, SessionApprovalPolicy{Mode: mode, Pattern: pattern, CreatedAt: time.Now()})
	slog.Info("audit: session approval pre-granted", "user", user, "mode", mode, "tool", toolName, "conv_id", convID, "scope", scope, "pattern", pattern)
}

// approvalHandler runs the staged tool when the user approves, or marks it
// rejected. Returns the MCP result or the rejection confirmation.
//
// Requires the agent manager's MCP client so we can fire the send outside
// the agent loop. For that reason it lives on Server (which already holds
// *agent.Manager).
func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request, convID, approvalID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r.Context())

	var req approvalRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxApprovalBody)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}

	d := s.decideApproval(r.Context(), r.Context().Done(), user, convID, approvalID, req)
	if d.suggest != nil {
		// suggest_advanced_model has its own resolution shape (three actions
		// instead of approve/reject) and a side effect that's pure metadata
		// — flipping conversations.model — rather than firing an MCP tool.
		s.handleSuggestAdvancedApproval(context.WithoutCancel(r.Context()), w, r, user, d.suggest, req)
		return
	}
	d.write(w)
}

// approvalDecision is the answer to one approval decision, before it is
// written: a JSON body, or an HTTP error status with its plain text, or
// nothing at all when the client went away while a call was still running.
// decideApproval produces it so the single-card POST and the group decision
// (approval_group.go) share every check, claim and execution path.
type approvalDecision struct {
	body    map[string]any
	status  int    // an error status; 0 with a body = 200
	errText string // the error's plain text
	// suggest is set for a pending suggest_advanced_model card, whose
	// resolution has its own shape and writes its own answer
	// (handleSuggestAdvancedApproval).
	suggest *store.Approval
	// gone: the client disconnected while the call ran; nobody reads a reply.
	gone bool
}

func approvalError(status int, text string) approvalDecision {
	return approvalDecision{status: status, errText: text}
}

func approvalBody(body map[string]any) approvalDecision {
	return approvalDecision{body: body}
}

// write sends the decision the way handleApproval always answered.
func (d approvalDecision) write(w http.ResponseWriter) {
	switch {
	case d.gone:
	case d.status != 0:
		http.Error(w, d.errText, d.status)
	default:
		writeJSON(w, d.body)
	}
}

// decideApproval applies one person's decision to one approval card and
// returns the answer: the idempotent recorded state for a settled card, the
// timed-out resolution for an expired one, the rejection, or the claimed
// execution's outcome (or "executing" for a long call on a server that
// declared a budget). ctx is the request's context for reads and claims;
// every write after a successful claim runs detached from it. done fires
// when the client goes away while it waits for a running call.
func (s *Server) decideApproval(ctx context.Context, done <-chan struct{}, user, convID, approvalID string, req approvalRequest) approvalDecision {
	approval, err := s.store.GetApproval(ctx, user, approvalID)
	if err != nil {
		return approvalError(http.StatusInternalServerError, err.Error())
	}
	if approval == nil {
		return approvalError(http.StatusNotFound, "approval not found")
	}
	if approval.ConversationID != convID {
		return approvalError(http.StatusBadRequest, "approval/conversation mismatch")
	}
	if approval.Status != "pending" {
		// Idempotent: return the already-resolved state without re-firing.
		return s.resolvedApprovalDecision(ctx, user, approval.ID)
	}
	if handlerOnlyApproval(approval.ToolName) && req.Scope != "" && req.Scope != "once" {
		return approvalError(http.StatusBadRequest, "this tool requires a separate decision for each staged card; use scope once")
	}
	// The bundle can require the same for a critical tool
	// (agent_policy.critical_tool_no_session_approval). The web hides the
	// apply-all checkbox for it; this is the server's own refusal, so a
	// terminal or API client cannot pre-approve the rest of the chat either.
	// Nothing is claimed: the card stays pending for a per-call decision.
	if scope := strings.TrimSpace(req.Scope); scope != "" && scope != "once" && !agentcore.SessionApprovalAllowed(approval.ToolName) {
		return approvalError(http.StatusBadRequest, "this action needs its own decision for each call, so it cannot be approved or denied for the rest of the chat; use scope once")
	}
	// A narrowed approval (agent_policy.critical_tool_narrowable_args): the
	// person approved only some of the staged values of the declared list
	// argument. Checked whole before anything is claimed; a bad request
	// leaves the card pending (approval_narrowing.go).
	narrowing, err := requestedNarrowing(approval, req)
	if err != nil {
		return approvalError(http.StatusBadRequest, err.Error())
	}

	// A click that lands after the default-deny deadline but before the sweep's
	// next tick resolves the row as timed out RIGHT NOW instead of losing the
	// claim and echoing "pending" back — which the card rendered as a silent
	// reset the user could click forever (#225 follow-up). Same claim primitive
	// as the sweep, so a concurrent sweep tick can't double-resolve; if we lose
	// that race the resolvedApprovalDecision fallback below reports whatever
	// state won. Default-deny stays authoritative either way: nothing executes.
	// Every write that follows a successful claim runs on execCtx, detached
	// from the request: once the row is durably resolved, the matching
	// tool_result breadcrumb MUST land too, or the next turn's model reads a
	// dangling APPROVAL_REQUIRED and may re-stage the very action the user
	// just declined. A tab closed between the two writes used to do exactly
	// that on the reject and expired paths (the approve path was already
	// detached; the asymmetry was the bug).
	execCtx := context.WithoutCancel(ctx)

	if approval.ExpiresAt > 0 && approval.ExpiresAt <= time.Now().Unix() {
		resultText := timeoutResultTextFor(approval.ToolName)
		claimed, err := s.store.ClaimExpiredApproval(ctx, user, approvalID, "rejected", resultText)
		if err != nil {
			return approvalError(http.StatusInternalServerError, err.Error())
		}
		if claimed {
			appendToolResultToHistory(execCtx, s.store, convID, approval.ToolName,
				resolutionCallID(approval), resultText, approval.UserEmail)
			s.noteApprovalSettled(convID, approval)
			return approvalBody(s.resumeReplyFlag(execCtx, map[string]any{"status": "rejected", "result_text": resultText}, approval))
		}
		return s.resolvedApprovalDecision(ctx, user, approvalID)
	}

	// suggest_advanced_model resolves through its own handler, branched
	// before the generic Send path so it never routes through runStagedTool.
	if approval.ToolName == tools.SuggestAdvancedModelToolName {
		return approvalDecision{suggest: approval}
	}

	if !req.Approved {
		claimMsg, historyMsg := rejectionMessages(approval.ToolName)
		claimed, err := s.store.ClaimApproval(ctx, user, approvalID, "rejected", claimMsg)
		if err != nil {
			return approvalError(http.StatusInternalServerError, err.Error())
		}
		if !claimed {
			return s.resolvedApprovalDecision(ctx, user, approvalID)
		}
		// Also write a tool_result into the conversation so the NEXT turn
		// sees the rejection and the model knows not to retry. Use the
		// original tool_call id so the chip in the UI updates instead
		// of orphaning a second result row keyed off the approval id.
		appendToolResultToHistory(execCtx, s.store, convID, approval.ToolName,
			resolutionCallID(approval), historyMsg, approval.UserEmail)
		s.maybeRegisterSessionPolicy(convID, user, approval.ToolName, req)
		s.noteApprovalSettled(convID, approval)
		return approvalBody(s.resumeReplyFlag(execCtx, map[string]any{"status": "rejected"}, approval))
	}

	// Admit the execution before claiming: once a shutdown drain has begun,
	// a new approval is refused while still pending (nothing is claimed or
	// run), so the drain can never miss an execution that started after it
	// looked.
	if !s.approvalRuns.enter() {
		return approvalError(http.StatusServiceUnavailable, "fleet is shutting down; the action was not run. Try again in a moment.")
	}
	admitted := true
	defer func() {
		if admitted {
			s.approvalRuns.leave()
		}
	}()

	// User clicked Send. Claim the approval BEFORE firing the tool:
	// the pending→approved flip is the only atomic gate, so executing
	// first would let two concurrent requests (double-click, mobile
	// retry) both send the email and only collide on the status write
	// afterwards. Losing the claim means someone else is running it —
	// return their resolved state instead of re-firing.
	// A tool in critical_tool_stop: its stop signal exists BEFORE the claim
	// makes the row executing, so a Stop that lands at any point after that
	// finds it (approval_stop.go). Dropped once the outcome is recorded, or
	// right away when this request does not win the claim.
	stoppable := approvalStoppable(approval.ToolName)
	if stoppable {
		s.armApprovalStop(approvalID)
	}
	s.executingApprovals.register(execCtx, convID, user, approvalID)
	claimed, err := s.claimApproved(ctx, user, convID, approval, narrowing)
	if err != nil || !claimed {
		s.executingApprovals.unregister(approvalID)
		if stoppable {
			s.disarmApprovalStop(approvalID)
		}
	}
	if err != nil {
		return approvalError(http.StatusInternalServerError, err.Error())
	}
	if !claimed {
		return s.resolvedApprovalDecision(ctx, user, approvalID)
	}

	// The user already committed to the action, so it runs on the detached
	// execCtx — a tab close mid-flight must not abandon a claimed approval
	// half-executed. The execution, the outcome write and the session policy
	// are one unit (executeClaimedApproval) on its own goroutine, which owns
	// the s.approvalRuns admission from here on, so a graceful shutdown waits
	// for it within the grace.
	// The request waits for it, as it always has, unless the call runs on a
	// server that declared an approved-call budget and is still running at
	// approvalEarlyReplyAfter: then the request answers "executing" (the
	// same shape a concurrent loser or a "Check result" re-POST gets while
	// the sentinel stands) and the execution finishes detached and records
	// its outcome exactly as it would have. A long deal write must not hold
	// the request (and the web proxy in front of it) open for its whole
	// budget.
	reply := make(chan map[string]any, 1)
	admitted = false // the goroutine leaves the gate
	go func() {
		defer s.approvalRuns.leave()
		defer s.disarmApprovalStop(approvalID)
		defer s.executingApprovals.unregister(approvalID)
		defer safe.Recover("httpapi.approval_execute", func(any) {
			// A panic before the outcome write leaves the sentinel standing:
			// the action may have run, so the honest answer is "unknown",
			// never success or failure, and never a re-run.
			s.approvalPersistenceFailures.Store(approvalID, true)
			// The narrowing was recorded with the claim, so the card keeps it.
			reply <- withApprovalNarrowed(map[string]any{
				"status": "approved", "execution_unknown": true,
				"result_text": approvalOutcomeUnrecordedText,
			}, approval)
		})
		reply <- s.executeClaimedApproval(execCtx, user, convID, approvalID, approval, req)
	}()
	wait, early0 := s.approvalEarlyReplyWait(approval)
	if !early0 {
		return approvalBody(<-reply)
	}
	early := time.NewTimer(wait)
	defer early.Stop()
	select {
	case out := <-reply:
		return approvalBody(out)
	case <-early.C:
		running := map[string]any{
			"status":      "approved",
			"executing":   true,
			"result_text": approvalExecutingSentinel,
		}
		// A narrowed approval says what was approved while it still runs, so
		// the running card keeps its "Approved k of n" and left-out rows;
		// critical_tool_stop: the running card offers Stop.
		return approvalBody(s.resumeReplyFlag(execCtx, withApprovalStop(withApprovalNarrowed(running, approval), approval), approval))
	case <-done:
		// The client is gone; nobody reads a reply. The execution carries on
		// detached and records its outcome for the next "Check result".
		return approvalDecision{gone: true}
	}
}

// approvalEarlyReplyAfter is how long an approval POST for a call on a server
// with a declared approved-call budget waits for the outcome before it
// answers "executing" and lets the call finish detached. It sits below every
// proxy and client timeout in front of the API (the web tier and
// browsers allow minutes), and below the 60 s budget a server gets without a
// declaration. Var, not const: tests shorten it.
var approvalEarlyReplyAfter = 50 * time.Second

// approvalOutcomeUnrecordedText is the reply when the action was attempted but
// its outcome could not be durably recorded.
const approvalOutcomeUnrecordedText = "The action was attempted but its outcome could not be recorded. Verify the external result before taking further action."

// approvalMayAnswerEarly reports whether an approved card's POST may answer
// "executing" before its call returns: only an MCP call on a server that
// opted into a longer budget (approved_call_timeout_seconds) does. Every
// other approval (email on an undeclared server, bash, schedule_task,
// manage_tasks, preview_email, mock mode) keeps the request open until the
// outcome is recorded, exactly as before. The server is the card's recorded
// seat, or, for a legacy row with none, the catalog entry that would run it.
func (s *Server) approvalMayAnswerEarly(approval *store.Approval) bool {
	if s.cfg != nil && s.cfg.MockMode {
		return false
	}
	if !strings.HasPrefix(approval.ToolName, "mcp_") || s.agent == nil {
		return false
	}
	server := ""
	if approval.MCPServer != "" {
		server = agentcore.RegisteredMCPName(approval.MCPServer, approval.MCPAccount)
	} else if name, _, err := resolveMCPTool(s.agent.MCPCatalog(), approval.ToolName); err == nil {
		server = name
	}
	if server == "" {
		return false
	}
	_, declared := agentcore.ApprovedCallBudget(server, approval.ArgsJSON)
	return declared
}

// approvalProgressEarlyReplyAfter is how long the POST waits before it
// answers "executing" for a call of a tool in critical_tool_progress
// (docs/APPROVAL-PROGRESS.md): the card only shows the call's progress once it
// knows the call is running, so a call that reports progress answers early
// almost at once, whether or not its server declared a budget. Var, not
// const: tests change it.
var approvalProgressEarlyReplyAfter = 2 * time.Second

// approvalEarlyReplyWait is how long an approved card's POST waits for the
// call before it answers "executing", and whether it may at all: an MCP call
// of a tool that reports progress answers after
// approvalProgressEarlyReplyAfter; any other call on a server that declared a
// budget after approvalEarlyReplyAfter (approvalMayAnswerEarly); everything
// else waits for its outcome, as before.
func (s *Server) approvalEarlyReplyWait(approval *store.Approval) (time.Duration, bool) {
	if s.cfg != nil && s.cfg.MockMode {
		return 0, false
	}
	if strings.HasPrefix(approval.ToolName, "mcp_") && (approvalReportsProgress(approval.ToolName) || approvalStoppable(approval.ToolName)) {
		// A call that reports progress, or that the person may stop, shows
		// its running card at once: that card is where both happen.
		return min(approvalProgressEarlyReplyAfter, approvalEarlyReplyAfter), true
	}
	if s.approvalMayAnswerEarly(approval) {
		return approvalEarlyReplyAfter, true
	}
	return 0, false
}

// executeClaimedApproval runs a claimed approval, records its outcome
// (SetApprovalResult, which also commits the history breadcrumb) and
// registers any session policy the click asked for. It returns the POST body
// for that outcome. It runs on the detached execCtx and never reads the
// request, so it completes the same way whether or not anyone is still
// waiting for its reply. Mock mode short-circuits so Playwright can exercise
// the approval UX without real SendGrid credentials.
func (s *Server) runClaimedApprovalAction(execCtx context.Context, user, convID string, approval *store.Approval, req approvalRequest) (string, error) {
	switch {
	case approval.ToolName == tools.EnableMCPConnectorsToolName:
		return s.runStagedEnableMCPConnectors(execCtx, user, convID, approval)
	case s.cfg != nil && s.cfg.MockMode && approval.ToolName == tools.ManageTasksToolName:
		return "Scheduled tasks updated (mock mode — nothing persisted).", nil
	case s.cfg != nil && s.cfg.MockMode && approval.ToolName == tools.ScheduleTaskToolName:
		// Playwright/mock mode has no real sched store wired; report a canned
		// success so the approval UX is exercisable without a database.
		return "Scheduled task created (mock mode — no task persisted).", nil
	case s.cfg != nil && s.cfg.MockMode:
		return `{"status_code":202,"message":"mock send ok"}`, nil
	case approval.ToolName == tools.ScheduleTaskToolName:
		// schedule_task creates an orchestrator task in-process via the injected
		// seam (#239) — not an MCP send — so it gets its own one-shot path.
		return s.runStagedScheduleTask(execCtx, approval, req.Edits)
	case approval.ToolName == tools.ManageTasksToolName:
		// manage_tasks is the same shape for tasks that already exist (#1152).
		return s.runStagedManageTasks(execCtx, approval)
	default:
		return s.runStagedTool(execCtx, approval)
	}
}

func (s *Server) attachApprovedOptionalMCPServers(execCtx context.Context, reply map[string]any, user, convID string, approval *store.Approval, toolErr error, isErr bool) {
	if approval.ToolName != tools.EnableMCPConnectorsToolName || toolErr != nil || isErr {
		return
	}
	if conv, gerr := s.getOwnedOrMultiplayerConversation(execCtx, user, convID); gerr == nil && conv != nil {
		reply["enabled_optional"] = s.effectiveOptionalMCPServersForCaller(execCtx, user, conv)
	}
}

func (s *Server) executeClaimedApproval(execCtx context.Context, user, convID, approvalID string, approval *store.Approval, req approvalRequest) map[string]any {
	actionCtx := s.executingApprovals.register(execCtx, convID, user, approvalID)
	text, toolErr := s.runClaimedApprovalAction(actionCtx, user, convID, approval, req)
	s.executingApprovals.unregister(approvalID)
	// The call has returned, so nothing listens to its stop signal any more:
	// close the stop window now (approval_stop.go). A Stop accepted before
	// this point is recorded AND in the stop line below; one after it is
	// refused, so the row's stop mark and the history never disagree.
	stopped := s.finalizeApprovalStop(approvalID)
	resultText := text
	if toolErr != nil {
		// The approval stays approved so the user knows the action was
		// attempted; surface the failure in result_text so the UI can
		// show it.
		resultText = fmt.Sprintf("%s failed: %v", actionVerb(approval.ToolName), toolErr)
	}
	isErr := toolErr != nil
	// Fleet's own labelled lines after the tool's text (a result card is read
	// from the start of the result): for a narrowed approval, exactly which
	// staged values were approved and which were left out. They are governed
	// with the result but never truncated: the tool's text is bounded to what
	// the output limit leaves after them.
	var narrowed *approvalNarrowing
	if n, nerr := parseApprovalNarrowing(approval.NarrowedJSON); approval.NarrowedJSON != "" && nerr == nil {
		narrowed = n
	}
	// The person pressed Stop: fleet's line says so, in the words that fit.
	// A stop that landed before the call was sent ran nothing at all.
	var trailers []string
	if stopped {
		if errors.Is(toolErr, mcp.ErrStoppedBeforeSend) {
			trailers = append(trailers, approvalStoppedBeforeSendNote)
		} else {
			trailers = append(trailers, approvalStopNote)
		}
	}
	var governed string
	resultText, isErr, governed = governApprovalResultWithTrailer(execCtx, approval, resultText, isErr, approvalTrailer(narrowed, trailers...))
	// A tool in critical_tool_result_cards: the outcome card its own result
	// carries, stored on the still-executing row just before the outcome, so
	// the settled card and its reply carry both. Only for a call that ran and
	// passed governance; anything else keeps today's card.
	var resultCard json.RawMessage
	if toolErr == nil && !isErr {
		resultCard = s.recordApprovalResultCard(execCtx, user, approval, governed)
	}
	if err := s.store.SetApprovalResult(execCtx, user, approvalID, resultText, isErr); err != nil {
		slog.Error("SetApprovalResult failed", "err", err)
		s.approvalPersistenceFailures.Store(approvalID, true)
		return withApprovalNarrowed(map[string]any{
			"status": "approved", "execution_unknown": true,
			"result_text": approvalOutcomeUnrecordedText,
		}, approval)
	}
	// SetApprovalResult commits the history breadcrumb in the same transaction.
	// When the underlying execution itself failed (toolErr != nil && !stopped,
	// excluding post-execution output governance blocks whose side effect
	// already ran, user-stopped executions, and MCP transport disconnects where
	// the remote outcome is unknown), durably arm the row in Postgres so the
	// conversation automatically resumes and the agent can diagnose the error
	// and recover gracefully instead of stopping dead.
	outcomeUnknown := toolErr != nil && strings.Contains(strings.ToUpper(toolErr.Error()), "OUTCOME IS UNKNOWN")
	execFailed := toolErr != nil && !stopped && !outcomeUnknown
	if execFailed && approval.ResumeState == "" && (s.cfg == nil || !s.cfg.MockMode) {
		if st, ok := s.store.(*store.Store); ok && s.agent != nil {
			if armed, aerr := st.ArmFailedApprovalResume(execCtx, user, approvalID); aerr == nil && armed {
				approval.ResumeState = store.ApprovalResumeArmed
			}
		}
	}
	s.maybeRegisterSessionPolicy(convID, user, approval.ToolName, req)
	// A tool in critical_tool_progress: tell the owner it finished, in case
	// they walked away from the running card (approval_progress.go).
	s.notifyApprovalFinished(user, convID, approval, isErr)
	// The outcome is in the conversation now, so a resume turn can read it —
	// this is also where a long call answered "executing" earlier lands.
	s.noteApprovalSettled(convID, approval)
	reply := map[string]any{
		"status":      "approved",
		"result_text": resultText,
		"is_err":      isErr,
	}
	if resultCard != nil {
		reply["result_card"] = resultCard
	}
	s.attachApprovedOptionalMCPServers(execCtx, reply, user, convID, approval, toolErr, isErr)
	withApprovalNarrowed(reply, approval)
	if stopped {
		reply["stoppable"], reply["stopped"] = true, true
	}
	return s.resumeReplyFlag(execCtx, reply, approval)
}

// governApprovalResultWithTrailer is governApprovalResult for a result that
// carries fleet's own labelled lines (trailer): they are governed too, and
// never cut by the result's size bound (agentcore
// GovernAndBoundModelVisibleToolTextWithTrailer).
func governApprovalResultWithTrailer(ctx context.Context, approval *store.Approval, text string, isErr bool, trailer string) (bounded string, err bool, governed string) {
	if trailer == "" {
		return governApprovalResult(ctx, approval, text, isErr)
	}
	return agentcore.GovernAndBoundModelVisibleToolTextWithTrailer(ctx, approval.ToolName, resolutionCallID(approval), text, isErr, trailer)
}

// governApprovalResult applies the loop's post-execution boundary to an
// approved call's result. It returns the bounded, model-visible text and its
// error flag, plus the governed text before the size bound, which only the
// result card reads (approval_result_card.go).
func governApprovalResult(ctx context.Context, approval *store.Approval, text string, isErr bool) (bounded string, err bool, governed string) {
	if approval == nil {
		return agentcore.GovernAndBoundModelVisibleToolTextFull(ctx, "approval", "", text, isErr)
	}
	return agentcore.GovernAndBoundModelVisibleToolTextFull(ctx, approval.ToolName, resolutionCallID(approval), text, isErr)
}

// approvalExecutingSentinel is the result_text ClaimApproval writes before
// the staged tool runs. An idempotent retry that sees this text and a NULL
// is_err must report executing:true — not success — so a concurrent loser
// cannot stamp "Email sent ✓" while the winner is still in flight. Named
// so clients and tests share one string; do not parse arbitrary tool output
// for error.
const approvalExecutingSentinel = store.ApprovalExecutingSentinel

// approvalOutcomeFlags is the execution-outcome contract for an already-
// resolved approval row. status remains consent; these flags tell the
// client whether the attempt succeeded, failed, is still running, or was
// never recorded (legacy rows). The TUI matches executing and
// execution_unknown by these exact keys.
func approvalOutcomeFlags(a *store.Approval) map[string]any {
	if a == nil || a.Status != "approved" {
		return nil
	}
	if a.IsErr.Valid {
		return map[string]any{"is_err": a.IsErr.Bool}
	}
	if a.ToolName == tools.SuggestAdvancedModelToolName {
		// Even before is_err existed, approval and the model pin committed in
		// one transaction. An approved suggestion proves successful execution.
		return map[string]any{"is_err": false}
	}
	if a.ToolName == previewEmailToolName && a.ResultText == "Preview dismissed by user. No email was sent." {
		return map[string]any{"is_err": false}
	}
	if a.ResultText == approvalExecutingSentinel {
		return map[string]any{"executing": true}
	}
	if isNotifyRecordResult(a.ResultText) {
		// Pre-migration notify records have no IsErr but the prefix proves
		// RecordAction observed a successful run. Do not relabel them unknown.
		return map[string]any{"is_err": false}
	}
	return map[string]any{"execution_unknown": true}
}

func (s *Server) approvalOutcomeFlags(a *store.Approval) map[string]any {
	if _, failed := s.approvalPersistenceFailures.Load(a.ID); failed && !a.IsErr.Valid && a.ResultText == approvalExecutingSentinel {
		return map[string]any{"execution_unknown": true}
	}
	return approvalOutcomeFlags(a)
}

// approvalClientState is the idempotent POST body: consent plus the
// outcome flags. Used by the already-resolved early return and by
// writeResolvedApprovalState so a replay cannot drop is_err.
func (s *Server) approvalClientState(a *store.Approval) map[string]any {
	out := map[string]any{
		"status":      a.Status,
		"result_text": a.ResultText,
	}
	for k, v := range s.approvalOutcomeFlags(a) {
		out[k] = v
	}
	if out["execution_unknown"] == true && a.ResultText == approvalExecutingSentinel {
		out["result_text"] = "Execution outcome could not be recorded. Verify the external result before taking further action."
	}
	// The outcome card the call's own result carried, for a tool in
	// critical_tool_result_cards whose success fleet recorded. Absent for
	// every other row, so its payload is unchanged. So is what a narrowed
	// approval approved (approval_narrowing.go).
	return withApprovalStop(withApprovalNarrowed(s.withApprovalResultCard(out, a), a), a)
}

// writeResolvedApprovalState answers a request that lost the claim race
// (or arrived after resolution) with the current state of the approval,
// mirroring the idempotent already-resolved response above.
func (s *Server) writeResolvedApprovalState(w http.ResponseWriter, r *http.Request, user, approvalID string) {
	s.resolvedApprovalDecision(r.Context(), user, approvalID).write(w)
}

// resolvedApprovalDecision is writeResolvedApprovalState before it is written.
func (s *Server) resolvedApprovalDecision(ctx context.Context, user, approvalID string) approvalDecision {
	latest, err := s.store.GetApproval(ctx, user, approvalID)
	if err != nil || latest == nil {
		return approvalError(http.StatusConflict, "approval already resolved")
	}
	state := s.resumeReplyFlag(ctx, s.approvalClientState(latest), latest)
	if latest.ToolName == "suggest_advanced_model" && latest.Status == "approved" {
		conv, err := s.store.Get(ctx, user, latest.ConversationID)
		if err != nil || conv == nil {
			return approvalError(http.StatusInternalServerError, "could not load conversation model")
		}
		state["model"] = conv.Model
	}
	if latest.ToolName == tools.EnableMCPConnectorsToolName && latest.Status == "approved" {
		if conv, err := s.getOwnedOrMultiplayerConversation(ctx, user, latest.ConversationID); err == nil && conv != nil {
			state["enabled_optional"] = s.effectiveOptionalMCPServersForCaller(ctx, user, conv)
		}
	}
	return approvalBody(state)
}

// handleSuggestAdvancedApproval resolves a suggest_advanced_model card.
// Three valid actions:
//   - "switch_and_retry" / "switch_only": mark approved, pin the
//     conversation to the advanced model. The UI distinguishes the two
//     by whether it re-submits the prior user turn after the response;
//     the server only cares about the model flip.
//   - "dismiss" (or req.Approved == false): mark rejected. The
//     conversation's model stays as-is.
//
// The response carries `model` (the new effective slug) so the UI can
// update its local conversation state without a separate refetch.
//
// Claim discipline matches the generic path: the pending→resolved flip is
// claimed FIRST, and only the winner writes the history breadcrumb and (on
// accept) flips the model. Before, the accept path flipped the model and
// appended history and only then tried to resolve, so two concurrent clicks
// (a double-click, two tabs) both passed the in-memory "still pending" check
// and landed two tool_result rows — and the losing dismiss click got a 500
// ("already resolved") instead of the idempotent resolved-state echo.
func (s *Server) handleSuggestAdvancedApproval(execCtx context.Context, w http.ResponseWriter, r *http.Request, user string, approval *store.Approval, req approvalRequest) {
	switchAction := req.Action == "switch_and_retry" || req.Action == "switch_only" ||
		(req.Action == "" && req.Approved)
	if !switchAction {
		// Treat anything else as a dismissal. Record the rejection so
		// the cooldown gate in StageSuggestion sees it.
		claimed, err := s.store.ClaimApproval(r.Context(), user, approval.ID, "rejected",
			"User dismissed the model-switch suggestion.")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !claimed {
			s.writeResolvedApprovalState(w, r, user, approval.ID)
			return
		}
		metrics.RecordModelEscalation(suggestionSource(approval.ArgsJSON), "dismissed")
		appendToolResultToHistory(execCtx, s.store, approval.ConversationID, approval.ToolName, resolutionCallID(approval),
			"User dismissed the model-switch suggestion. Continue working with the current model.", approval.UserEmail)
		// Not a resume tool, but a deferred resume may be waiting on it.
		s.noteApprovalSettled(approval.ConversationID, approval)
		writeJSON(w, map[string]any{
			"status": "rejected",
			"action": "dismiss",
		})
		return
	}

	// User accepted. The claim and the model pin are ONE transaction
	// (store.ClaimApprovalAndSetModel): the resolution row's text says the
	// conversation is pinned, so it must not be written unless the pin landed.
	// Before, a claim that won and then failed to pin (a transient DB error)
	// returned 500 with the approval already recorded as approved — and the
	// retry saw a resolved row, echoed it, and never pinned. Now a failed pin
	// rolls the claim back, the card stays pending, and the retry works.
	advancedModel := suggestedModel(approval.ArgsJSON)
	if advancedModel == "" {
		http.Error(w, "suggestion has no frozen model target; dismiss it and request a new suggestion", http.StatusConflict)
		return
	}
	action := req.Action
	if action == "" {
		action = "switch_only"
	}
	// The action is recorded in the result text so the approvals table tells
	// a switch-and-retry from a plain switch (docs/CHAT-ESCALATION.md).
	resultText := fmt.Sprintf("User accepted the suggestion (%s). Conversation pinned to %s.", action, advancedModel)
	claimed, err := s.store.ClaimApprovalAndSetModel(r.Context(), user, approval.ID, resultText, approval.ConversationID, advancedModel)
	if err != nil {
		logging.From(r.Context()).Error("ClaimApprovalAndSetModel failed (suggest_advanced approval)", "err", err)
		http.Error(w, "could not pin conversation model: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !claimed {
		s.writeResolvedApprovalState(w, r, user, approval.ID)
		return
	}
	metrics.RecordModelEscalation(suggestionSource(approval.ArgsJSON), "accepted_"+action)
	appendToolResultToHistory(execCtx, s.store, approval.ConversationID, approval.ToolName, resolutionCallID(approval),
		resultText, approval.UserEmail)
	s.noteApprovalSettled(approval.ConversationID, approval)

	writeJSON(w, map[string]any{
		"status":      "approved",
		"action":      action,
		"model":       advancedModel,
		"result_text": resultText,
	})
}

// resolutionCallID returns the id under which a post-approval
// tool_result row should be written. Prefer the original tool_call id
// the orchestration layer captured at stage time so the chip the UI
// already rendered (keyed off that id) gets its resultText updated
// from "APPROVAL_REQUIRED..." to the real outcome on reload. Older
// approval rows predating the migration won't have one — fall back
// to the approval id, matching pre-fix behavior.
func resolutionCallID(a *store.Approval) string {
	if a != nil && a.ToolCallID != "" {
		return a.ToolCallID
	}
	if a != nil {
		return a.ID
	}
	return ""
}

// approvalTimeoutResultText is the canned result recorded when an approval is
// auto-denied for exceeding its default-deny window (#225). Mirrors the manual
// "User declined" path so the next turn's model sees the action was not taken.
// The UI matches on the "Approval timed out" prefix to offer its ask-again
// affordance — keep the prefix stable if the wording changes.
const approvalTimeoutResultText = "Approval timed out — auto-denied. The action was not taken."

// previewExpiredResultText is the honest wording for a LEGACY pending
// preview_email row swept past a deadline (new previews stage with no expiry).
// A preview is display-only, so "auto-denied / the action was not taken" would
// tell the model an action was refused when none ever existed.
const previewExpiredResultText = "Preview closed. It was display-only; nothing was sent."

// timeoutResultTextFor picks the sweep's result wording for a tool.
func timeoutResultTextFor(toolName string) string {
	if toolName == previewEmailToolName {
		return previewExpiredResultText
	}
	return approvalTimeoutResultText
}

// SweepExpiredApprovals enforces the default-DENY-on-timeout contract for the
// web approval path (#225): every pending approval whose expires_at deadline has
// passed is atomically claimed as rejected and a tool_result is appended to the
// conversation so the next turn's model knows the action was not taken. It
// mirrors the manual-reject path in handleApproval exactly, minus the live SSE
// emit (the staging turn is long over; the card resolves to its rejected state
// on the next reload, and the client-side countdown has already greyed it out).
//
// Claiming via ClaimExpiredApproval is the race gate against a concurrent
// sweep tick. User clicks after the deadline no longer win: ClaimApproval
// refuses expired rows (#1109), so default-deny is authoritative at click
// time. A user click *before* the deadline still wins because
// ClaimApproval requires expires_at > now, and this sweep's claim then
// returns not-claimed. Returns the number of approvals auto-denied (for
// logging). Run on a periodic ticker by cmd/fleet; safe to call
// concurrently with user resolutions.
func (s *Server) SweepExpiredApprovals(ctx context.Context) (int, error) {
	expired, err := s.store.ListExpiredApprovals(ctx, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	if len(expired) == store.MaxExpiredApprovalsPerSweep {
		// The batch is capped (store.MaxExpiredApprovalsPerSweep). More may be
		// pending past their deadline; the next tick picks up the remainder. Log
		// rather than silently bounding coverage.
		logging.From(ctx).Info("approval expiry sweep: batch hit the row cap; remainder handled next tick", "cap", store.MaxExpiredApprovalsPerSweep)
	}
	denied := 0
	for i := range expired {
		// Bail cleanly if our context is cancelled or the per-tick deadline is
		// near: claiming a row we can't then write a history result for would
		// leave it rejected (default-deny still holds — nothing executes) but
		// without the tool_result breadcrumb the next turn reads. Stopping here
		// keeps each processed row's claim+append atomic-in-practice; the
		// unprocessed rows stay pending and are swept on the next tick.
		if err := ctx.Err(); err != nil {
			logging.From(ctx).Error("approval expiry sweep: stopping early; remainder handled next tick", "denied", denied, "expired", len(expired), "err", err)
			break
		}
		a := expired[i]
		resultText := timeoutResultTextFor(a.ToolName)
		claimed, err := s.store.ClaimExpiredApproval(ctx, a.UserEmail, a.ID, "rejected", resultText)
		if err != nil {
			logging.From(ctx).Error("approval expiry sweep: claim failed", "approval_id", a.ID, "err", err)
			continue
		}
		if !claimed {
			// A user action (or a concurrent sweep) already resolved it.
			continue
		}
		appendToolResultToHistory(ctx, s.store, a.ConversationID, a.ToolName,
			resolutionCallID(&a), resultText, a.UserEmail)
		// A timed-out card is settled too: an opted-in one resumes (the model
		// learns the action was not taken), and any other may release a
		// resume that was waiting on it.
		s.noteApprovalSettled(a.ConversationID, &a)
		denied++
	}
	return denied, nil
}

// ── helper on Manager: expose the MCP client to the approval path ──
// (added via a small method on Manager in agent/agent.go)
