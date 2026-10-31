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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/cron"
	"github.com/ElcanoTek/fleet/internal/croncount"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sandbox"
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
	if agentcore.ApprovalTimeoutForTool(toolName) <= 0 && a.isPatrolOrPatrolResume() && t < defaultApprovalTimeoutSeconds {
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
	a.sink.Emit("tool.action_recorded", map[string]any{
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
		a.sink.Emit("tool.auto_resolved", map[string]any{
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
		if p, ok := a.sessionRegistry.Match(a.conversationID, toolName, rawInput); ok {
			a.sink.Emit("tool.auto_resolved", map[string]any{
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
	if a.isAutoApprove() && !handlerOnlyApproval(toolName) && !tools.IsEmailToolName(toolName) && !agentcore.IsOutboundEmailTool(toolName) && agentcore.SessionApprovalAllowed(toolName) {
		a.sink.Emit("tool.auto_resolved", map[string]any{
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
	// For schedule_task, only supersede a pending card when it targets the
	// same task name (or when all pending schedule_task cards share this
	// call's name), so staging multiple distinct scheduled tasks in one turn
	// (e.g. six partner meeting-prep briefs) leaves every card pending.
	if n, err := a.supersedePendingApprovals(toolName, rawInput); err != nil {
		log.Printf("supersede approvals (tool=%s conv=%s): %v", toolName, a.conversationID, err)
	} else if n > 0 {
		// Tell the UI to mark the old card as resolved. The client
		// already handles rejected approvals — this just nudges any
		// open stream to refresh its approval state.
		a.sink.Emit("tool.approval_superseded", map[string]any{
			"tool":  toolName,
			"count": n,
		})
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
			log.Printf("approval narrowing: freeze the argument for %s: ok=%t err=%v", approval.ID, ok, err)
		} else {
			approval.NarrowableArg = arg
		}
	}
	if cardJSON != "" {
		if ok, err := a.store.SetApprovalCard(a.ctx, a.userEmail, approval.ID, cardJSON); err != nil || !ok {
			log.Printf("approval card: store card for %s: ok=%t err=%v", approval.ID, ok, err)
			cardFallback = cardFallbackStore
		} else {
			approval.CardJSON = cardJSON
		}
	}
	if cardFallback != "" {
		// Observable without reading server logs: the card a declared
		// describer should have produced fell back to the generic one.
		a.sink.Emit("tool.approval_card_fallback", map[string]any{
			"approval_id": approval.ID,
			"tool":        toolName,
			"reason":      cardFallback,
		})
	}

	a.sink.Emit("tool.approval_required", approvalRequiredEvent(approval, rawInput, a.conversationID))
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
			log.Printf("push: approval notification (tool=%s conv=%s): %s", logSafe(toolName), logSafe(convID), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF.
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

	prior, err := a.store.LatestApprovalByTool(a.ctx, a.conversationID, tools.SuggestAdvancedModelToolName)
	if err != nil {
		return "", "", fmt.Errorf("lookup prior suggestion: %w", err)
	}
	if prior != nil {
		switch prior.Status {
		case "pending":
			return "", "SUGGESTION_SUPPRESSED: a model-switch suggestion is already pending the user's response. Do not stage another.", nil
		case "approved", "rejected":
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
	if n, err := a.store.SupersedePendingApprovals(a.ctx, a.conversationID, tools.SuggestAdvancedModelToolName); err != nil {
		log.Printf("supersede suggestions (conv=%s): %v", a.conversationID, err)
	} else if n > 0 {
		a.sink.Emit("tool.approval_superseded", map[string]any{
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

	a.sink.Emit("tool.approval_required", approvalRequiredEvent(approval, string(rawInput), a.conversationID))

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
		log.Printf("prevalidateEmail: resolve source tool: %v", err)
		return nil
	}
	// The email is staged by a chat turn, so a validate_email_content the
	// bundle hides from interactive chat (agent_policy.interactive_hidden_tools)
	// is not called, exactly as a hidden approval-card describer is not
	// (approval_card.go): this host-side call must not reach what the model
	// could not. Prevalidation is best-effort, so it is skipped like an
	// unreachable validator and the card is staged unvalidated.
	if full := agentcore.MCPToolName(server, "validate_email_content"); agentcore.InteractiveHiddenTool(full) {
		log.Printf("prevalidateEmail: %q is hidden from interactive chat (interactive_hidden_tools); not called", full)
		return nil
	}
	text, isError, err := broker.CallMCP(ctx, server, "validate_email_content", map[string]interface{}{
		"content": content,
		"subject": subject,
	})
	if err != nil {
		log.Printf("prevalidateEmail: validation tool error: %v", err)
		return nil
	}
	if isError {
		log.Printf("prevalidateEmail: validation tool reported an error")
		return nil
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}

	var vr validationResult
	if err := json.Unmarshal([]byte(text), &vr); err != nil {
		log.Printf("prevalidateEmail: unmarshal validation result: %v", err)
		return nil
	}
	if !vr.Valid && len(vr.Errors) > 0 {
		return fmt.Errorf("email validation failed:\n  - %s", strings.Join(vr.Errors, "\n  - "))
	}
	return nil
}

// resolveEmailValidationServer keeps an MCP send's validation on its source
// server. Native preview_email/send_email calls have no server prefix, so they
// preserve mcp.Client.CallTool's historical deterministic behavior by choosing
// the lexicographically first server that advertises validate_email_content.
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

// approvalClientFields is the review payload shared by live SSE events and
// GET /conversations/{id} pending cards so a terminal cannot see a truncated
// summary for one tool class and a full snapshot for another.
func approvalClientFields(toolName, rawInput, convID string) map[string]any {
	frozen := frozenApprovalArgs(rawInput)
	if toolName == tools.SuggestAdvancedModelToolName && suggestedModel(rawInput) == "" {
		// Legacy suggestions did not freeze their target: require a new card.
		frozen = map[string]any{"complete": false}
	}
	out := map[string]any{
		"summary":      summarizeApprovalInput(toolName, rawInput, convID),
		"pattern_args": handlerApprovalPatternArgs(toolName, rawInput),
		"frozen_args":  frozen,
		// True when the bundle lists the tool in
		// critical_tool_no_session_approval: the card must not offer
		// "apply my choice to all calls", and the POST refuses that scope.
		"no_session_approval": !agentcore.SessionApprovalAllowed(toolName),
	}
	if approvalReportsProgress(toolName) {
		// critical_tool_progress: once approved and running, the card polls
		// for the call's progress and outcome (docs/APPROVAL-PROGRESS.md).
		// Only present when true, so every other card's payload is unchanged.
		out["progress_updates"] = true
	}
	if approvalStoppable(toolName) {
		// critical_tool_stop: once approved and running, the card offers
		// Stop (docs/APPROVAL-STOP.md). Only present when true.
		out["stoppable"] = true
	}
	if resumesAfterApproval(toolName) {
		// critical_tool_resume: once this card is settled fleet starts a turn
		// on its own (docs/RESUME-AFTER-APPROVAL.md), so the client follows the
		// conversation for it. Only present when true, so every other card's
		// payload is unchanged.
		out["resume_after_approval"] = true
	}
	return out
}

// resumesAfterApproval reports whether a settled card for toolName starts a
// resume turn under the running policy (handler-only cards never do, except
// enable_mcp_connectors which always resumes so the agent can use the newly
// enabled connectors immediately).
func resumesAfterApproval(toolName string) bool {
	if toolName == tools.EnableMCPConnectorsToolName {
		return true
	}
	return agentcore.ResumeAfterApproval(toolName) && !handlerOnlyApproval(toolName)
}

func approvalRequiredEvent(approval *store.Approval, rawInput, convID string) map[string]any {
	ev := approvalClientFields(approval.ToolName, rawInput, convID)
	ev["approval_id"] = approval.ID
	ev["tool"] = approval.ToolName
	ev["expires_at"] = approval.ExpiresAt
	ev["mcp_server"] = approval.MCPServer
	ev["mcp_account"] = approval.MCPAccount
	return withApprovalNarrowable(withApprovalGroup(withApprovalCard(ev, approval), approval), approval)
}

// summarizeApprovalInput dispatches on tool name to build a display
// payload for the approval card. convID is used by the email
// summarizer to resolve workspace-relative inline attachment paths
// when expanding `cid:` image references to data: URLs for the
// preview iframe — pass "" when no per-conversation context is
// available (the substitution becomes a no-op).
func summarizeApprovalInput(toolName, rawInput, convID string) map[string]any {
	if toolName == "bash" {
		return summarizeBashInput(toolName, rawInput)
	}
	if toolName == tools.SuggestAdvancedModelToolName {
		return summarizeSuggestAdvancedInput(toolName, rawInput)
	}
	if toolName == tools.ScheduleTaskToolName {
		return summarizeScheduleTaskInput(toolName, rawInput)
	}
	if toolName == tools.ManageTasksToolName {
		return summarizeManageTasksInput(toolName, rawInput)
	}
	if toolName == tools.EnableMCPConnectorsToolName {
		return summarizeEnableMCPConnectorsInput(toolName, rawInput)
	}
	// preview_email uses the exact same summary shape as send_email
	// so ApprovalCard's existing email-preview render path just works;
	// the only difference is the `tool` field, which drives the UI
	// branch that swaps Send for Dismiss.
	if tools.IsEmailToolName(toolName) {
		return summarizeSendEmailInput(toolName, rawInput, convID)
	}
	// Everything else — bundle-declared critical tools like a pages deploy or
	// a deal write — gets a tool-shaped summary. These used to fall through to
	// the email summary and render on the email card ("Send this email?",
	// "Email sent ✓" — for a page publish), which is exactly the wrong copy on
	// the one card class whose entire job is telling the user what is about to
	// happen.
	return summarizeGenericToolInput(toolName, rawInput)
}

// genericArgValueMax bounds each rendered argument value; the full args stay on
// the approval row for execution, this is display only.
const genericArgValueMax = 300

// summarizeGenericToolInput builds the card payload for a critical tool fleet
// has no tailored card for: the tool name plus its top-level arguments as
// display strings (sorted keys — JSON maps have no order), with nested values
// compacted and everything truncated to card-friendly sizes. Fleet cannot know
// what a bundle tool's arguments mean, so it shows them verbatim rather than
// guessing at a friendlier shape — the honest floor for a human review step.
func summarizeGenericToolInput(toolName, rawInput string) map[string]any {
	var args map[string]any
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil || args == nil {
		return map[string]any{"tool": toolName, "raw": rawInput}
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, map[string]string{"key": k, "value": displayArgValue(args[k])})
	}
	return map[string]any{
		"tool": toolName,
		"args": rows,
	}
}

// displayArgValue renders one argument value for the generic card: scalars as
// themselves, nested structures as compact JSON, all truncated by RUNES so a
// multibyte payload can't be cut mid-character.
func displayArgValue(v any) string {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case nil:
		s = "null"
	default:
		b, err := json.Marshal(t)
		if err != nil {
			s = fmt.Sprintf("%v", t)
		} else {
			s = string(b)
		}
	}
	if r := []rune(s); len(r) > genericArgValueMax {
		s = string(r[:genericArgValueMax]) + "…"
	}
	return s
}

// summarizeSuggestAdvancedInput exposes the agent's reason and the
// recommended model slug so the UI card can render both without
// re-parsing the args. The server freezes the configured tier when staging;
// a later admin change must not change what this card authorizes.
func summarizeSuggestAdvancedInput(toolName, rawInput string) map[string]any {
	var args struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal([]byte(rawInput), &args)
	return map[string]any{
		"tool":            toolName,
		"reason":          args.Reason,
		"recommend_model": suggestedModel(rawInput),
		"source":          suggestionSource(rawInput),
	}
}

// Who staged a suggest_advanced_model card (docs/CHAT-ESCALATION.md): the
// chat model through the tool, a free end-of-turn struggle signal, or the
// decision-model classifier. Cards staged before the field existed read as
// the model's.
const (
	suggestionSourceModel      = "model"
	suggestionSourceSignal     = "signal"
	suggestionSourceClassifier = "classifier"
)

func suggestionSource(rawInput string) string {
	var args struct {
		Source string `json:"source"`
	}
	_ = json.Unmarshal([]byte(rawInput), &args)
	switch args.Source {
	case suggestionSourceSignal, suggestionSourceClassifier:
		return args.Source
	}
	return suggestionSourceModel
}

func suggestedModel(rawInput string) string {
	var args struct {
		Model string `json:"recommend_model"`
	}
	if json.Unmarshal([]byte(rawInput), &args) != nil {
		return ""
	}
	return strings.TrimSpace(args.Model)
}

// summarizeScheduleTaskInput builds the schedule_task approval-card payload
// (#239): the task name, a truncated prompt preview, the schedule (cron or
// one-time run_at), whether it recurs, and an estimated runs-per-month for
// recurring tasks so the user can gauge frequency before approving. Pure display
// — the underlying approval row keeps the full args for the create call.
func summarizeScheduleTaskInput(toolName, rawInput string) map[string]any {
	var p scheduleTaskStagedArgs
	if err := json.Unmarshal([]byte(rawInput), &p); err != nil {
		return map[string]any{"tool": toolName, "raw": rawInput}
	}
	const previewMax = 200
	preview := strings.TrimSpace(p.Prompt)
	// Truncate by RUNES, not bytes, so a multibyte UTF-8 prompt isn't cut
	// mid-rune (which would emit a replacement char in the card).
	if r := []rune(preview); len(r) > previewMax {
		preview = string(r[:previewMax]) + "…"
	}
	out := map[string]any{
		"tool":           toolName,
		"name":           strings.TrimSpace(p.Name),
		"prompt_preview": preview,
		// Full prompt so the card's edit mode starts from the real text (the
		// preview is rune-truncated; editing a truncated prompt would clobber
		// the staged one on approve).
		"prompt":        strings.TrimSpace(p.Prompt),
		"model":         strings.TrimSpace(p.Model),
		"allow_network": p.AllowNetwork,
		"tags":          sanitizeTaskTagSlice(p.Tags),
		// Connectors the task inherits from this conversation (ADR-0068), the
		// always-on servers every run binds anyway, and the flag the card turns
		// into a warning: approving as-is schedules a task that can reach no
		// connector at all.
		"connectors":           connectorLabels(p.Connectors),
		"always_on_connectors": append([]string{}, p.AlwaysOnConnectors...),
		"no_connectors":        len(connectorLabels(p.Connectors)) == 0 && len(p.AlwaysOnConnectors) == 0,
	}
	cron := strings.TrimSpace(p.Cron)
	switch {
	case cron != "":
		out["recurring"] = true
		out["cron"] = cron
		if n, ok := croncount.EstimateRunsPerMonth(cron); ok {
			out["runs_per_month"] = n
		}
	case strings.TrimSpace(p.RunAt) != "":
		out["recurring"] = false
		if t, ok := p.RunAtTime(); ok {
			out["run_at"] = t.Format(time.RFC3339)
		} else {
			out["run_at"] = strings.TrimSpace(p.RunAt)
		}
	default:
		out["recurring"] = false
		out["run_immediately"] = true
	}
	return out
}

// summarizeManageTasksInput builds the manage_tasks approval-card payload
// (#1152): what is about to happen, to which tasks, and — for an update — the
// change set as a list of "field → new value" lines.
//
// The card renders the SELECTOR, not a resolved list of matched tasks. Resolving
// it here would need the sched store at staging time, and the number it produced
// would be a snapshot that the executor's own resolution could disagree with by
// the time the human clicked Approve. Instead the boundaries are enforced where
// they are true: the tool refuses a filtered STOP outright, the executor caps an
// update's blast radius, and the post-approval report names every task it
// touched. What the card must never be is vague about the ACTION — so "stop"
// carries its own irreversibility line.
func summarizeManageTasksInput(toolName, rawInput string) map[string]any {
	var p tools.ManageTasksParams
	if err := json.Unmarshal([]byte(rawInput), &p); err != nil {
		return map[string]any{"tool": toolName, "raw": rawInput}
	}
	p.AddTags = sanitizeTaskTagSlice(p.AddTags)
	p.RemoveTags = sanitizeTaskTagSlice(p.RemoveTags)
	action := strings.TrimSpace(p.Action)
	ids := p.CleanTaskIDs()
	out := map[string]any{
		"tool":      toolName,
		"action":    action,
		"task_ids":  ids,
		"match":     p.MatchSummary(),
		"changes":   p.ChangeSummary(),
		"max_tasks": tools.MaxManagedTasks,
	}
	if action == tools.ManageTasksActionStop {
		out["destructive"] = true
		out["consequence"] = "A recurring job stops recurring and will not run again. A run in progress halts at its next checkpoint and keeps its partial transcript."
	}
	return out
}

// runStagedManageTasks applies an approved manage_tasks call. Everything it can
// check without storage is re-checked here rather than trusted from staging —
// the args have been sitting in a row while a human read a card, and Validate is
// where the stop-needs-explicit-ids and update-must-change-something rules live.
func (s *Server) runStagedManageTasks(ctx context.Context, approval *store.Approval) (string, error) {
	if s.manageTasks == nil {
		return "", errors.New("managing scheduled tasks from chat is not configured on this server")
	}
	var p tools.ManageTasksParams
	if err := json.Unmarshal([]byte(approval.ArgsJSON), &p); err != nil {
		return "", fmt.Errorf("parse manage_tasks args: %w", err)
	}
	if err := p.Validate(); err != nil {
		return "", err
	}
	req := TaskMutationRequest{
		Action:        strings.TrimSpace(p.Action),
		TaskIDs:       p.CleanTaskIDs(),
		Prompt:        strings.TrimSpace(p.Prompt),
		Cron:          strings.TrimSpace(p.Cron),
		Model:         strings.TrimSpace(p.Model),
		MaxIterations: p.MaxIterations,
		AddTags:       sanitizeTaskTagSlice(p.AddTags),
		RemoveTags:    sanitizeTaskTagSlice(p.RemoveTags),
		RequestedBy:   approval.UserEmail,
	}
	if !p.Match.IsEmpty() {
		req.Match = &TaskMutationMatch{
			Query: strings.TrimSpace(p.Match.Query),
			Tag:   strings.TrimSpace(p.Match.Tag),
			Model: strings.TrimSpace(p.Match.Model),
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	res, err := s.manageTasks(ctx, req)
	if err != nil {
		return "", err
	}
	return formatTaskMutationReport(req.Action, res, s.orchestratorTaskLink()), nil
}

// formatTaskMutationReport names every task the call touched. A bulk edit that
// reports only a count is a bulk edit nobody can check — and the operator asking
// for this was trying to fix jobs that had drifted, so "which ones" is the whole
// question.
func formatTaskMutationReport(action string, res *TaskMutationResult, link string) string {
	verb := "Updated"
	if action == tools.ManageTasksActionStop {
		verb = "Stopped"
	}
	var b strings.Builder
	if res == nil || len(res.Changed) == 0 {
		b.WriteString("Nothing changed: the selector matched no task you can edit.\n")
	} else {
		fmt.Fprintf(&b, "%s %d task(s):\n", verb, len(res.Changed))
		for _, e := range res.Changed {
			fmt.Fprintf(&b, "  • %s (%s)", e.Label, e.ID)
			if e.Detail != "" {
				fmt.Fprintf(&b, " — %s", e.Detail)
			}
			b.WriteString("\n")
		}
	}
	if res != nil && len(res.Skipped) > 0 {
		fmt.Fprintf(&b, "Skipped %d:\n", len(res.Skipped))
		for _, e := range res.Skipped {
			fmt.Fprintf(&b, "  • %s (%s) — %s\n", e.Label, e.ID, e.Detail)
		}
	}
	if link != "" {
		fmt.Fprintf(&b, "Review them in the Operations Center: %s", link)
	}
	return b.String()
}

// summarizeBashInput extracts the command, working directory, and
// timeout from a staged bash invocation so the UI can render a plain
// preview.
func summarizeBashInput(toolName, rawInput string) map[string]any {
	var args struct {
		Command        string `json:"command"`
		WorkingDir     string `json:"working_dir"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	_ = json.Unmarshal([]byte(rawInput), &args)
	// Truncate by RUNES, not bytes, so a multibyte command (a non-ASCII
	// path, an emoji in an echo) isn't cut mid-rune into a replacement char.
	preview := args.Command
	if r := []rune(preview); len(r) > 600 {
		preview = string(r[:600]) + "…[truncated]"
	}
	return map[string]any{
		"tool":            toolName,
		"command":         args.Command,
		"preview":         preview,
		"working_dir":     args.WorkingDir,
		"timeout_seconds": args.TimeoutSeconds,
	}
}

// summarizeSendEmailInput pulls the display-relevant fields out of a
// send_email payload. Carries:
//   - `preview`: a truncated (600 char) plain-string snippet used by older
//     UI paths and as a lightweight fallback.
//   - `content`: the FULL content, so the approval card can render an
//     HTML iframe preview exactly matching what SendGrid will receive.
//   - `content_type`: "text/html" vs "text/plain" so the UI picks the
//     right renderer without sniffing the body.
//   - `attachments` / `inline_attachments`: the frozen path/cid metadata
//     from ArgsJSON, so a terminal review can show every file that will
//     actually be sent. File bytes stay out of the summary; execution still
//     replays the original ArgsJSON.
//
// The full content is capped at 1 MiB — SendGrid tolerates much more, but
// anything beyond that in an approval payload is almost certainly a bug
// and blowing up a browser state blob to multi-megabytes is a bigger UX
// regression than hiding the preview behind a "too large" notice.
func summarizeSendEmailInput(toolName, rawInput, convID string) map[string]any {
	var args map[string]any
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return map[string]any{"tool": toolName, "raw": rawInput}
	}
	// Stage() inlines content_file into content before we get here, so
	// reading content alone is enough.
	full := firstString(args, "content")
	// Substitute `src="cid:foo"` refs with data: URLs sourced from the
	// matching inline_attachments entries. The preview iframe has no
	// SMTP envelope so cid: URLs would otherwise render as broken
	// images; the user sees a chart-shaped void in the draft and
	// reports "I clicked send blindly". The actual approval.args_json
	// keeps the original cid: + inline_attachments so the real
	// SendGrid call still attaches the images and the recipient's
	// inbox renders them via the normal MIME path.
	if convID != "" {
		full = expandCidImagesToDataURLs(full, args, convID)
	}
	// Rune-counted like the other previews so a non-ASCII email body isn't
	// cut mid-character; the 1 MiB content cap is a BYTE budget, so it is
	// cut with capBytes (a byte slice snapped back to a rune boundary).
	preview := full
	if r := []rune(preview); len(r) > 600 {
		preview = string(r[:600]) + "…[truncated]"
	}
	const maxContentBytes = 1 << 20 // 1 MiB
	contentOverflow := false
	if len(full) > maxContentBytes {
		full = capBytes(full, maxContentBytes)
		contentOverflow = true
	}
	contentType := firstString(args, "content_type")
	if contentType == "" {
		// Match sendgrid_server.py's sniff: presence of <html/<body/<table
		// or a doctype → assume HTML, otherwise plain. Cheap and close
		// enough for preview routing; the real detection runs server-side
		// before send.
		contentType = sniffContentType(full)
	}
	out := map[string]any{
		"tool":             toolName,
		"to":               args["to_email"],
		"cc":               args["cc_emails"],
		"bcc":              args["bcc_emails"],
		"subject":          args["subject"],
		"from":             firstString(args, "from_email"),
		"preview":          preview,
		"content":          full,
		"content_type":     contentType,
		"content_overflow": contentOverflow,
	}
	if v, ok := args["attachments"]; ok {
		out["attachments"] = v
	}
	if v, ok := args["inline_attachments"]; ok {
		out["inline_attachments"] = v
	}
	return out
}

// cidPattern matches one `cid:<id>` reference. The id runs to the first
// character that cannot appear inside an unquoted/quoted attribute value,
// and the trailing \b makes the match end on a word character — so a
// reference wrapped in markup punctuation (`url(cid:logo)`) still yields
// the bare id. Compiled once at package level: it used to be rebuilt per
// attachment inside the substitution loop, which re-parsed the pattern and
// re-scanned the whole document for every inline image.
var cidPattern = regexp.MustCompile(`(?i)cid:([^\s"'<>]+)\b`)

// expandCidImagesToDataURLs rewrites `src="cid:<id>"` references in
// the supplied HTML into `src="data:image/png;base64,..."` data URLs
// sourced from the matching inline_attachments entry. Used only for
// the preview pane — the underlying approval row keeps the original
// cid: form so SendGrid's MIME assembler still attaches the images
// for the real send.
//
// Path validation mirrors the workspace file API: relative paths
// resolve under the per-conversation workspace root, EvalSymlinks +
// has-prefix check rejects escapes. We refuse to read anything
// larger than 4 MiB (the same per-attachment cap the email pipeline
// enforces elsewhere) so a misbehaving agent can't blow up the
// approval row's summary payload.
func expandCidImagesToDataURLs(html string, args map[string]any, convID string) string {
	atts, ok := args["inline_attachments"].([]any)
	if !ok || len(atts) == 0 || html == "" {
		return html
	}
	wsDir, err := filepath.Abs(tools.WorkspaceDirForConversation(convID))
	if err != nil {
		return html
	}
	// Every read goes through an os.Root on the workspace dir: it refuses any
	// path that resolves outside it, whatever the agent supplied.
	root, err := os.OpenRoot(wsDir)
	if err != nil {
		return html
	}
	defer func() { _ = root.Close() }()
	const maxAttachmentBytes = 4 << 20

	cidMap := make(map[string]string)

	for _, raw := range atts {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		cid := strings.TrimSpace(firstString(entry, "cid", "content_id"))
		path := strings.TrimSpace(firstString(entry, "path", "file"))
		if cid == "" || path == "" {
			continue
		}
		// Resolve to an absolute path safely. Refuse anything that
		// escapes the workspace dir so a hostile agent can't get
		// /etc/passwd inlined into the preview card.
		var full string
		if filepath.IsAbs(path) {
			full = path
		} else {
			full = filepath.Join(wsDir, filepath.FromSlash(path))
		}
		resolved, err := filepath.EvalSymlinks(full)
		if err != nil {
			continue
		}
		resolvedAbs, err := filepath.Abs(resolved)
		if err != nil {
			continue
		}
		// Confine the symlink-resolved path to the workspace dir. filepath.Rel +
		// filepath.IsLocal is the containment check CodeQL's path-injection query
		// recognizes: Rel's nil-error result sanitizes resolvedAbs, and IsLocal
		// rejects a "../" escape that Rel would otherwise return with a nil error.
		// This keeps a hostile agent from getting /etc/passwd inlined into the
		// preview card; the root.Stat / root.ReadFile below are confined again by
		// the os.Root.
		rel, relErr := filepath.Rel(wsDir, resolvedAbs)
		if relErr != nil || !filepath.IsLocal(rel) {
			continue
		}
		info, err := root.Stat(rel)
		if err != nil || info.IsDir() || info.Size() > maxAttachmentBytes {
			continue
		}
		data, err := root.ReadFile(rel)
		if err != nil {
			continue
		}
		mimeType := mime.TypeByExtension(filepath.Ext(resolvedAbs))
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		dataURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)

		// Store the data URL mapped by the case-insensitive cid.
		cidMap[strings.ToLower(cid)] = dataURL
	}

	if len(cidMap) > 0 {
		// ONE pass over the document replaces every `cid:<id>` reference
		// (single or double quotes, case-insensitive scheme) by looking the
		// captured id up in the map, instead of one compile-and-rescan per
		// attachment. A reference with no matching attachment is returned
		// verbatim, so unresolvable cids survive exactly as before. The single
		// pass also means a substituted data URL is never itself rescanned.
		html = cidPattern.ReplaceAllStringFunc(html, func(match string) string {
			cidVal := match[len("cid:"):] // the pattern guarantees the prefix
			if dataURL, ok := cidMap[strings.ToLower(cidVal)]; ok {
				return dataURL
			}
			return match
		})
	}
	return html
}

// sniffContentType returns "text/html" if the body looks like HTML, else
// "text/plain". Checked case-insensitively against a few structural tags
// that almost every styled email carries. Mirrors the agent-side default
// "always content_type=text/html" convention while still letting plain
// drafts preview as text.
func sniffContentType(body string) string {
	if body == "" {
		return "text/plain"
	}
	lc := strings.ToLower(body)
	markers := []string{"<!doctype", "<html", "<body", "<table", "<div"}
	for _, m := range markers {
		if strings.Contains(lc, m) {
			return "text/html"
		}
	}
	return "text/plain"
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
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
	s.sessionApprovals.Register(convID, toolName, SessionApprovalPolicy{Mode: mode, Pattern: pattern, CreatedAt: time.Now()})
	//nolint:gosec // G706: every interpolated value is rendered with %q, which escapes any CR/LF, so a request-supplied scope/pattern cannot forge a log line.
	log.Printf("audit: %q pre-%s %q for conversation %q (scope=%q pattern=%q)", user, mode, toolName, convID, scope, pattern)
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
				resolutionCallID(approval), resultText)
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
			resolutionCallID(approval), historyMsg)
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
	claimed, err := s.claimApproved(ctx, user, convID, approval, narrowing)
	if err != nil || !claimed {
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
	if conv, gerr := s.store.Get(execCtx, user, convID); gerr == nil && conv != nil {
		reply["enabled_optional"] = conv.OptionalMCPServersEnabled
	}
}

func (s *Server) executeClaimedApproval(execCtx context.Context, user, convID, approvalID string, approval *store.Approval, req approvalRequest) map[string]any {
	text, toolErr := s.runClaimedApprovalAction(execCtx, user, convID, approval, req)
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
		log.Printf("SetApprovalResult: %v", err)
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

// DrainApprovalRuns stops admitting approved executions and blocks until
// every one already admitted has recorded its outcome, or ctx (the shutdown
// grace) fires; it reports which. From its first call on, a new approve POST
// is refused before its claim, so the card stays pending. An execution cut
// off by the process exit leaves its sentinel standing, and the next boot's
// RecoverStrandedApprovals records it as outcome unknown.
func (s *Server) DrainApprovalRuns(ctx context.Context) bool {
	return s.approvalRuns.drain(ctx)
}

// approvalRunGate admits and counts approved executions for the shutdown
// drain. A sync.WaitGroup cannot do this job: an Add that races a Wait which
// has already seen zero breaks its contract, and the approve route stays
// reachable while the server drains. Admission and drain share one mutex, so
// once drain has started no execution can begin. The zero value is ready.
type approvalRunGate struct {
	mu       sync.Mutex
	active   int
	draining bool
	idle     chan struct{} // closed when draining and active reaches zero
}

// enter admits one execution, or refuses it once a drain has begun.
// Increment active before checking the filesystem drain sentinel so any updater
// that sets the sentinel after enter() succeeds is guaranteed to observe
// active > 0 on its subsequent /healthz probe.
func (g *approvalRunGate) enter() bool {
	g.mu.Lock()
	if g.draining {
		g.mu.Unlock()
		return false
	}
	g.active++
	g.mu.Unlock()
	if UpdateDrainActive() {
		g.leave()
		return false
	}
	return true
}

// leave ends one admitted execution.
func (g *approvalRunGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.draining && g.active == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
}

func (g *approvalRunGate) activeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
}

// drain refuses further admissions and waits for the admitted ones, or ctx.
func (g *approvalRunGate) drain(ctx context.Context) bool {
	g.mu.Lock()
	g.draining = true
	if g.active == 0 {
		g.mu.Unlock()
		return true
	}
	if g.idle == nil {
		g.idle = make(chan struct{})
	}
	idle := g.idle
	g.mu.Unlock()
	select {
	case <-idle:
		return true
	case <-ctx.Done():
		return false
	}
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
		if conv, err := s.store.Get(ctx, user, latest.ConversationID); err == nil && conv != nil {
			state["enabled_optional"] = conv.OptionalMCPServersEnabled
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
			"User dismissed the model-switch suggestion. Continue working with the current model.")
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
		log.Printf("ClaimApprovalAndSetModel (suggest_advanced approval): %v", err)
		http.Error(w, "could not pin conversation model: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !claimed {
		s.writeResolvedApprovalState(w, r, user, approval.ID)
		return
	}
	metrics.RecordModelEscalation(suggestionSource(approval.ArgsJSON), "accepted_"+action)
	appendToolResultToHistory(execCtx, s.store, approval.ConversationID, approval.ToolName, resolutionCallID(approval),
		resultText)
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
		log.Printf("approval expiry sweep: batch hit the %d-row cap; remainder handled next tick", store.MaxExpiredApprovalsPerSweep)
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
			log.Printf("approval expiry sweep: stopping after %d/%d (%v); remainder handled next tick", denied, len(expired), err)
			break
		}
		a := expired[i]
		resultText := timeoutResultTextFor(a.ToolName)
		claimed, err := s.store.ClaimExpiredApproval(ctx, a.UserEmail, a.ID, "rejected", resultText)
		if err != nil {
			log.Printf("approval expiry sweep: claim %s: %v", a.ID, err)
			continue
		}
		if !claimed {
			// A user action (or a concurrent sweep) already resolved it.
			continue
		}
		appendToolResultToHistory(ctx, s.store, a.ConversationID, a.ToolName,
			resolutionCallID(&a), resultText)
		// A timed-out card is settled too: an opted-in one resumes (the model
		// learns the action was not taken), and any other may release a
		// resume that was waiting on it.
		s.noteApprovalSettled(a.ConversationID, &a)
		denied++
	}
	return denied, nil
}

// runStagedTool executes the staged tool one-shot outside the agent
// loop. Supports MCP tools (send_email) and the native bash tool (risky
// shell commands gated by orchestration.checkBashSafety).
func (s *Server) runStagedTool(ctx context.Context, approval *store.Approval) (text string, err error) {
	mgr := s.agent
	if mgr == nil {
		return "", errors.New("agent manager unavailable")
	}
	// Native bash: run inline via the tools package. Same safety checks
	// apply (hard-blocked patterns still rejected) since runBash is the
	// single entry point.
	if approval.ToolName == "bash" {
		return s.runStagedBash(ctx, approval)
	}
	// preview_email is preview-only by design: there is no send path.
	// When the user clicks Dismiss, we just mark it resolved with a
	// canned acknowledgment so the history reflects "user saw it".
	if approval.ToolName == previewEmailToolName {
		return "Preview dismissed by user. No email was sent.", nil
	}
	// Our internal naming convention is mcp_<server>_<tool>. Route by the
	// full prefixed name so the call lands on the server that staged it —
	// bare names collide across servers (sendgrid and mailbux both
	// export send_email).
	if !strings.HasPrefix(approval.ToolName, "mcp_") {
		return "", fmt.Errorf("unsupported tool for approval: %s", approval.ToolName)
	}
	// A tool in critical_tool_stop: a Stop accepted before the call was
	// dispatched (while its scope opened, say) means nothing was sent and no
	// cancellation either, whatever else failed first. Such a failure is
	// reported as stopped before send, so fleet's line says nothing ran
	// rather than that the server was asked to stop. This runs last, after
	// the stop window has closed (finalize is idempotent), so no Stop can land
	// between this check and the outcome.
	sent := false
	if approvalStoppable(approval.ToolName) {
		defer func() {
			if !s.finalizeApprovalStop(approval.ID) || sent || err == nil || errors.Is(err, errStoppedBeforeSend) {
				return
			}
			err = fmt.Errorf("%w (preparing it had failed too: %w)", errStoppedBeforeSend, err)
		}()
	}
	// The staged arguments, with the narrowed argument replaced when the
	// person approved only some of its values (approval_narrowing.go). A
	// narrowing record that does not match the staged call refuses the run.
	args, narrowing, err := approvalExecutionArgs(approval)
	if err != nil {
		return "", err
	}
	// Reopen the seat the card was staged under (#167 residual 2). The turn
	// scope that staged it is long gone, so this is a fresh short-lived scope
	// carrying the same public {server, account} selection. A revoked account
	// fails the open and therefore the approval — it never falls back to the
	// default bundle seat, which would send as the wrong client.
	//
	// Opening the scope (spawning the seat's server) has its own fixed
	// allowance, so a slow spawn never eats the call's budget; the close
	// after the call has its own too (approvalScopeCloseTimeout).
	openCtx, cancelOpen := context.WithTimeout(ctx, approvalScopeOpenTimeout)
	broker, catalog, release, err := s.approvalMCPScope(openCtx, approval)
	cancelOpen()
	if err != nil {
		return "", err
	}
	defer release()
	// Close the stop window the moment this returns, before the scope is
	// torn down (deferred calls run last-in first-out): release may block up
	// to approvalScopeCloseTimeout, and a Stop accepted then would be marked
	// and written into the outcome although no transport was listening any
	// more. executeClaimedApproval's own finalize reads the same answer.
	if approvalStoppable(approval.ToolName) {
		defer s.finalizeApprovalStop(approval.ID)
	}

	server, tool, err := resolveMCPTool(catalog, approval.ToolName)
	if err != nil {
		return "", err
	}
	// The call's budget: the flat 60 s (sized for an email send) unless the
	// server declares approved_call_timeout_seconds, in which case it is that,
	// scaled for a deal_ids batch (agentcore.ApprovedCallBudget). The budget
	// rides the context (mcp.WithCallTimeout) so it crosses the out-of-process
	// broker as callTimeoutMs and the credential owner applies it too, as the
	// agent loop's MCP calls do. The approval runs on a fresh per-approval
	// scope, so no other call queues ahead of it; the outer deadline is the
	// budget itself.
	budgetArgs := approval.ArgsJSON
	if narrowing != nil {
		// Budget the call that runs: a narrowed deal_ids batch is smaller.
		if b, merr := json.Marshal(args); merr == nil {
			budgetArgs = string(b)
		}
	}
	budget, _ := agentcore.ApprovedCallBudget(server, budgetArgs)
	callCtx, cancel := context.WithTimeout(mcp.WithCallTimeout(ctx, budget), budget)
	defer cancel()
	if narrowing != nil {
		// Tell the server which values were staged, so it can check its
		// other arguments against what the person reviewed.
		callCtx = mcp.WithRequestMeta(callCtx, narrowingRequestMeta(narrowing))
	}
	// Workspace-file references (ADR-0066/0088) are read now, as the agent
	// loop reads them just before its own dispatch (agentcore mcpTool.call):
	// the card and the stored arguments keep the reference, and the bytes
	// sent are the version its whole-file hash names. The windows stay
	// charged until the dispatch has let go of args (mcp.WithArgumentHold).
	resolved, err := s.resolveStagedFileReferences(callCtx, approval, catalogInputSchema(catalog, server, tool), args)
	if err != nil {
		return "", err
	}
	callCtx, hold := mcp.WithArgumentHold(callCtx, resolved)
	defer hold.Done()
	// Reading may have run past the call's budget. Never dispatch then: an
	// out-of-process call sends the request before it notices ctx.
	if err := callCtx.Err(); err != nil {
		return "", fmt.Errorf("MCP call not sent: %w", err)
	}
	// A tool in critical_tool_stop: the person may stop the call while it
	// runs (approval_stop.go). A Stop pressed before it was sent runs nothing.
	if approvalStoppable(approval.ToolName) {
		if h := s.approvalStopFor(approval.ID); h != nil {
			if h.stopped() {
				return "", errStoppedBeforeSend
			}
			callCtx = mcp.WithStop(callCtx, h.ch)
		}
	}
	// A tool in critical_tool_progress asks its server for progress while it
	// runs (approval_progress.go); the executing card shows it. Stopped when
	// the call returns, before its outcome is recorded.
	if approvalReportsProgress(approval.ToolName) {
		var stopProgress func()
		callCtx, stopProgress = s.startApprovalProgress(callCtx, approval)
		defer stopProgress()
	}
	sent = true
	text, isError, err := broker.CallMCP(callCtx, server, tool, args)
	if err != nil {
		return "", err
	}
	if isError {
		return "", fmt.Errorf("MCP tool reported an error: %s", strings.TrimSpace(text))
	}
	return strings.TrimSpace(text), nil
}

// approvalScopeCloseTimeout bounds the post-execution scope close so a hung
// credential owner cannot hold the approval HTTP request open.
const approvalScopeCloseTimeout = 5 * time.Second

// approvalScopeOpenTimeout bounds reopening the staged seat's scope (spawning
// its server) before an approved MCP call. It is separate from the call's
// budget, so a long budget does not also stretch a hung spawn, and a slow
// spawn does not shorten the call.
const approvalScopeOpenTimeout = 30 * time.Second

// approvalMCPScope resolves the call seam an approved MCP tool executes on.
// With a recorded seat and a scope-capable engine it opens a per-approval
// scope; otherwise it falls back to the engine's shared broker — the
// pre-#167-residual-2 behaviour, which is still correct for legacy rows (no
// seat was ever recorded) and for the transitional local-client mode.
func (s *Server) approvalMCPScope(ctx context.Context, approval *store.Approval) (agentcore.MCPBroker, []mcp.ServerTool, func(), error) {
	shared := func() (agentcore.MCPBroker, []mcp.ServerTool, func(), error) {
		broker := s.agent.MCPBroker()
		if broker == nil {
			return nil, nil, nil, errors.New("MCP broker not initialized (mock mode?)")
		}
		return broker, s.agent.MCPCatalog(), func() {}, nil
	}
	if approval.MCPServer == "" {
		return shared()
	}
	// Hosted connections (#988): a seat recorded against one of the user's
	// remote connections reopens through the remote overlay, pinned to the
	// exact {connection, account} — never the bundle scope, which does not
	// know the name and would fail the approval as an unknown server.
	if s.isRemoteConnectionName(ctx, approval.UserEmail, approval.MCPServer) {
		overlay, err := s.agent.OpenApprovalRemoteMCPScope(ctx, approval.UserEmail, approval.MCPServer, approval.MCPAccount)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("open approval MCP scope for %s: %w", approvalSeatLabel(approval), err)
		}
		if overlay == nil {
			return shared()
		}
		return overlay.CallBroker(), overlay.Catalog, overlay.Close, nil
	}
	workspace := tools.WorkspaceDirForConversation(approval.ConversationID)
	// The approving user: ClaimApproval matches the row's user_email against
	// the authenticated caller, so approval.UserEmail IS the person who just
	// clicked approve — and the one whose turn staged the card (ADR-0087).
	scope, err := s.agent.OpenApprovalMCPScope(ctx, agentcore.MCPSelection{{
		Server:  approval.MCPServer,
		Account: approval.MCPAccount,
	}}, workspace, agentcore.NewMCPUserIdentity(approval.UserEmail))
	if err != nil {
		// Fail closed: a seat that no longer resolves (account revoked, server
		// removed from the bundle) must surface, not silently downgrade to the
		// default seat.
		return nil, nil, nil, fmt.Errorf("open approval MCP scope for %s: %w", approvalSeatLabel(approval), err)
	}
	if scope == nil {
		return shared()
	}
	release := func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), approvalScopeCloseTimeout)
		defer cancel()
		if err := scope.Close(closeCtx); err != nil {
			//nolint:gosec // G706: approval.ID is a server-minted uuid read back from
			// our own row, and it is rendered with %q so any CR/LF would be escaped.
			log.Printf("approval %q: close MCP scope: %v", approval.ID, err)
		}
	}
	return scope.Broker, scope.Catalog, release, nil
}

// isRemoteConnectionName reports whether server names one of the user's
// hosted connections (own or shared with them) rather than a bundle server.
// A bundle server of the same name wins — the overlay never mounts a remote
// name the base catalog already provides.
func (s *Server) isRemoteConnectionName(ctx context.Context, email, server string) bool {
	if s.remoteMCP == nil || !s.remoteMCP.Enabled() || server == "" {
		return false
	}
	for _, st := range s.agent.MCPCatalog() {
		if st.ServerName == server {
			return false
		}
	}
	for _, info := range s.agent.MCPServerCatalog() {
		if info.Name == server {
			return false
		}
	}
	if own, err := s.remoteMCP.ListServers(ctx, email); err == nil {
		for _, srv := range own {
			if srv.Name == server {
				return true
			}
		}
	}
	if shared, err := s.remoteMCP.SharedWithMe(ctx, email); err == nil {
		for _, srv := range shared {
			if srv.Name == server {
				return true
			}
		}
	}
	return false
}

// approvalSeatLabel renders the public seat for an operator-facing error.
func approvalSeatLabel(approval *store.Approval) string {
	if approval.MCPAccount == "" {
		return approval.MCPServer + " (default seat)"
	}
	return approval.MCPServer + " (account " + approval.MCPAccount + ")"
}

// resolveMCPTool maps the model-visible mcp_<server>_<tool> identity back to a
// catalog entry. Complete-name comparison avoids speculative underscore splits;
// if two catalog identities flatten to the same name, the longest server match
// preserves mcp.Client.CallToolPrefixed's established routing rule.
func resolveMCPTool(catalog []mcp.ServerTool, fullName string) (string, string, error) {
	if !strings.HasPrefix(fullName, "mcp_") {
		return "", "", fmt.Errorf("not an MCP tool name: %s", fullName)
	}
	var best mcp.ServerTool
	for _, st := range catalog {
		if "mcp_"+st.ServerName+"_"+st.Tool.Name == fullName && len(st.ServerName) > len(best.ServerName) {
			best = st
		}
	}
	if best.ServerName != "" {
		return best.ServerName, best.Tool.Name, nil
	}
	return "", "", fmt.Errorf("no cataloged MCP tool matches %s", fullName)
}

// catalogInputSchema returns the input schema of server's tool in catalog:
// its base64 properties are the arguments a workspace-file reference may
// stand in for.
func catalogInputSchema(catalog []mcp.ServerTool, server, tool string) map[string]any {
	for _, st := range catalog {
		if st.ServerName == server && st.Tool.Name == tool {
			return st.Tool.InputSchema
		}
	}
	return nil
}

// resolveStagedFileReferences replaces each workspace-file reference in an
// approved MCP call's args with the base64 of its window, read from the
// workspace of the conversation that staged it. The staging turn's sandbox is
// gone by now, so this takes one under the same posture approved bash runs
// under, binds the conversation workspace as its file root (as a turn does,
// agent.Manager.configureTurnWorkspace) and reads through
// tools.WorkspaceFileReader: confinement, the whole-file hash check, the caps
// and the memory budget are the agent loop's
// (agentcore.ResolveWorkspaceFileReferences). The sandbox is released once
// the windows are read. A call with no reference takes no sandbox.
func (s *Server) resolveStagedFileReferences(ctx context.Context, approval *store.Approval, schema, args map[string]any) (func(), error) {
	if !agentcore.HasWorkspaceFileReferences(schema, args) {
		return func() {}, nil
	}
	// A reference names a file in the staging conversation's own workspace.
	// An id that is not exactly one path element would resolve elsewhere
	// (the shared root, or a nested directory).
	convID := approval.ConversationID
	if convID == "." || !filepath.IsLocal(convID) || filepath.Base(convID) != convID {
		return nil, errors.New("workspace file reference: the approval has no conversation workspace")
	}
	sb, cleanup, err := s.takeApprovalSandbox(ctx, approval)
	if err != nil {
		return nil, fmt.Errorf("workspace file reference: %w", err)
	}
	defer cleanup()
	if sb == nil {
		return nil, errors.New("workspace file reference: no sandbox to read it from")
	}
	if err := sb.BindFileOpRoot(ctx, tools.WorkspaceDirForConversation(convID)); err != nil {
		return nil, fmt.Errorf("workspace file reference: bind conversation workspace: %w", err)
	}
	return agentcore.ResolveWorkspaceFileReferences(tools.WithConversationID(ctx, convID), schema, tools.WorkspaceFileReader(sb), args)
}

// runStagedBash executes an approved bash invocation directly. We call
// the tools package rather than going through the fantasy agent loop so
// the approval is a truly one-shot execution; no new LLM turn runs
// here. Hard-blocked safety rules still apply inside runBash.
func (s *Server) runStagedBash(ctx context.Context, approval *store.Approval) (string, error) {
	var params tools.BashParams
	if err := json.Unmarshal([]byte(approval.ArgsJSON), &params); err != nil {
		return "", fmt.Errorf("parse bash args: %w", err)
	}
	// Bound the approval handler; the configured per-call timeout still
	// applies inside runBash, but we also want the HTTP request to
	// return in reasonable time.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	sb, cleanup, err := s.takeApprovalSandbox(ctx, approval)
	if err != nil {
		return "", err
	}
	defer cleanup()
	return tools.RunBashForApproval(ctx, sb, params)
}

// takeApprovalSandbox acquires the sandbox an approved action runs in: the
// command of an approved bash call, or the read of an approved MCP call's
// workspace-file references.
func (s *Server) takeApprovalSandbox(ctx context.Context, approval *store.Approval) (*sandbox.Sandbox, func(), error) {
	// Honor the conversation's lockdown seal (#562). An approval is a
	// deferred tool call from a specific conversation, so it must run under
	// the same network posture the turn that staged it would have used —
	// otherwise a prompt-injected agent could stage an exfiltrating command
	// and have the human's Approve click silently lift the --network=none
	// seal the operator relies on. The fleet-wide FLEET_DEFAULT_NETWORK_MODE
	// is the second input and is applied inside takeStagedBashSandbox.
	lockdown, err := s.stagedBashLockdown(ctx, approval)
	if err != nil {
		return nil, func() {}, err
	}
	if lockdown && !s.cfg.LockdownAvailable() {
		// Mirror takeTurnSandbox's fail-closed check: never downgrade a
		// lockdown conversation to a network-enabled container just because
		// the sealed backend is misconfigured.
		return nil, func() {}, fmt.Errorf("conversation is in lockdown mode but the server has no sandbox image configured")
	}

	// Pull a sandbox so the approval runs through the same container
	// boundary an agent-driven bash call would.
	// Production agent.New always wires a pool; a nil pool here is a
	// programmer error and we surface it rather than silently dropping
	// to host execution (which would let approved-but-malicious bash
	// reach the chat-server's filesystem and credentials).
	pool := s.agent.SandboxPool()
	if pool == nil {
		return nil, func() {}, fmt.Errorf("approval handler: no sandbox pool wired (chat-server boot misconfigured?)")
	}
	return takeStagedBashSandbox(ctx, pool, lockdown)
}

// stagedBashLockdown resolves whether an approved bash command must run
// network-sealed: the conversation's own Lockdown flag (set at creation,
// immutable) OR'd with the server-wide CHAT_LOCKDOWN_ONLY switch — the same
// disjunction postChat and POST /conversations apply (server.go). The OR with
// cfg.LockdownOnly matters for conversations created before an operator
// flipped the global switch on. It FAILS CLOSED: if the conversation can't be
// resolved we refuse to run rather than guess the posture open.
func (s *Server) stagedBashLockdown(ctx context.Context, approval *store.Approval) (bool, error) {
	if s.cfg.LockdownOnly {
		return true, nil
	}
	conv, err := s.store.Get(ctx, approval.UserEmail, approval.ConversationID)
	if err != nil {
		return false, fmt.Errorf("resolve conversation lockdown state: %w", err)
	}
	if conv == nil {
		return false, fmt.Errorf("resolve conversation lockdown state: conversation %s not found", approval.ConversationID)
	}
	return conv.Lockdown, nil
}

// stagedBashTaker is the subset of *sandbox.Pool the approved-bash path uses
// to acquire its execution sandbox. It is an interface so the take-decision
// (sealed lockdown vs. allowlisted vs. warm pool) is unit-testable without
// spinning a real podman container — the same seam pattern as scheduledrun's
// sandboxTaker and agent.Manager's turnSandboxTaker.
type stagedBashTaker interface {
	// Take returns a warm, network-ENABLED sandbox (the interactive default).
	Take(ctx context.Context) (*sandbox.Sandbox, func(), error)
	// TakeContainer cold-starts a fresh sandbox with egress SEALED
	// (--network=none) — the lockdown boundary.
	TakeContainer(ctx context.Context) (*sandbox.Sandbox, func(), error)
	// TakeContainerWithEgress cold-starts a fresh sandbox whose HTTP(S)
	// clients are routed through the host egress proxy under allowlist.
	TakeContainerWithEgress(ctx context.Context, ov sandbox.ResourceOverride, allowlist []string) (*sandbox.Sandbox, func(), error)
	// EgressDefault reports the fleet-wide FLEET_DEFAULT_NETWORK_MODE and its
	// allowlist. The pool stores these as advisory data and does not act on
	// them itself — every take path must consult them (ADR-0012, ADR-0031).
	EgressDefault() (mode string, allowlist []string)
}

// takeStagedBashSandbox acquires the container an approved bash command runs
// in, mirroring the interactive turn path's selection
// (agent.takeTurnSandboxFrom) so an approval executes under the same network
// posture the turn that staged it would have used. Two independent inputs
// decide the posture:
//
//   - The CONVERSATION's lockdown seal (#562) — strictest and checked first.
//     It FAILS CLOSED: any container error, including ErrContainerUnavailable
//     on an image-less pool, is surfaced and never downgraded to the
//     network-enabled warm take. Running approved bash with egress on in a
//     lockdown chat is exactly the exfiltration hole #562 closed.
//   - The FLEET-WIDE FLEET_DEFAULT_NETWORK_MODE (ADR-0012, extended to the
//     chat path by ADR-0031). Before this was wired up, an approved command
//     from a non-lockdown conversation took an unconditional warm, fully-open
//     container — so a fleet-wide `lockdown` left approvals as an open-egress
//     hole in an otherwise sealed deployment, and `allowlisted` let them skip
//     the proxy entirely. These branches mirror takeTurnSandboxFrom's
//     fleet-wide branches, including their degrade to the host take on
//     ErrContainerUnavailable: that signal means there is no container
//     backend at all (a test/dev ModeHost pool — a release build has no host
//     executor to fall back to, #159), so there is nothing to seal. Two
//     deliberate deviations from the turn path, both stricter or
//     inapplicable: the per-conversation seal above never degrades, and there
//     is no persistent-REPL borrow to preempt — approved bash has always
//     taken a fresh container, never the conversation's long-lived kernel.
func takeStagedBashSandbox(ctx context.Context, pool stagedBashTaker, lockdown bool) (*sandbox.Sandbox, func(), error) {
	if lockdown {
		sb, cleanup, err := pool.TakeContainer(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("take lockdown sandbox: %w", err)
		}
		return sb, cleanup, nil
	}

	switch mode, allowlist := pool.EgressDefault(); mode {
	case sandbox.NetworkModeLockdown:
		sb, cleanup, err := pool.TakeContainer(ctx)
		if errors.Is(err, sandbox.ErrContainerUnavailable) {
			return pool.Take(ctx)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("take lockdown sandbox: %w", err)
		}
		return sb, cleanup, nil
	case sandbox.NetworkModeAllowlisted:
		sb, cleanup, err := pool.TakeContainerWithEgress(ctx, sandbox.ResourceOverride{}, allowlist)
		if errors.Is(err, sandbox.ErrContainerUnavailable) {
			return pool.Take(ctx)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("take allowlisted sandbox: %w", err)
		}
		return sb, cleanup, nil
	}

	sb, cleanup, err := pool.Take(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("take sandbox: %w", err)
	}
	return sb, cleanup, nil
}

// runStagedScheduleTask creates the orchestrator task an approved schedule_task
// call described (#239). It parses the staged args, re-validates them (the same
// pure checks the gate applied at stage time — defence in depth against a tampered
// row), maps them to the sched-agnostic seam, and formats a user/model-facing
// confirmation pointing at the Operations Center. The injected seam reuses the
// SAME constructor + persistence (models.NewTask + AddTask) and cron validation
// that POST /tasks and the create_task tool use — this handler does not fork a
// second create path. (It does NOT re-run the HTTP handler's field-limit
// validation — max_iterations bounds, prompt-min-length — matching the
// create_task seam; the human approval plus the box-wide cost/iteration ceilings
// bound a misconfigured task.) A nil seam (feature unconfigured) is surfaced as a
// clear error rather than silently succeeding.
// scheduledTaskModel resolves the model a chat-created scheduled task runs on:
// the agent's explicit slug, else the live task tier
// (agentcore.CurrentTaskModel — the admin task_model setting, else
// FLEET_TASK_MODEL, else the advanced tier).
//
// It deliberately does NOT inherit the conversation's model. Chat is where
// people pick fast models to play with, and a scheduled task is a long
// unattended run: inheriting put tasks scheduled from a quick chat on the
// everyday tier, the model least suited to them (docs/TASK-MODEL-TIER.md).
// Inheritance was the #1014 fix for an unpinned task with no deployment
// default; the task tier is never empty, so that case cannot arise. The slug
// is still pinned on the task at create time, so the run is reproducible and
// does not drift with a later tier change.
func scheduledTaskModel(explicit string) string {
	if m := strings.TrimSpace(explicit); m != "" {
		return m
	}
	return agentcore.CurrentTaskModel()
}

func (s *Server) runStagedScheduleTask(ctx context.Context, approval *store.Approval, edits *scheduleTaskEdits) (string, error) {
	if s.scheduleTask == nil {
		return "", errors.New("scheduling from chat is not configured on this server")
	}
	var p scheduleTaskStagedArgs
	if err := json.Unmarshal([]byte(approval.ArgsJSON), &p); err != nil {
		return "", fmt.Errorf("parse schedule_task args: %w", err)
	}
	// The card's inline edits land BEFORE Validate, so a user-adjusted
	// name/prompt/cron is held to exactly the checks the staged args passed.
	if edits != nil {
		if edits.Name != nil {
			p.Name = *edits.Name
		}
		if edits.Prompt != nil {
			p.Prompt = *edits.Prompt
		}
		if edits.Cron != nil {
			p.Cron = *edits.Cron
		}
	}
	if err := p.Validate(); err != nil {
		return "", err
	}

	req := TaskScheduleRequest{
		Title:                strings.TrimSpace(p.Name),
		Prompt:               strings.TrimSpace(p.Prompt),
		Model:                scheduledTaskModel(p.Model),
		Cron:                 strings.TrimSpace(p.Cron),
		MaxIterations:        p.MaxIterations,
		AllowNetwork:         p.AllowNetwork,
		ThinkingBudgetTokens: p.ThinkingBudgetTokens,
		Tags:                 sanitizeTaskTagSlice(p.Tags),
		// The connectors this conversation had enabled when the card was staged
		// (ADR-0068) — the card showed them, the task gets them.
		Connectors: p.Connectors,
		// The approving user is the budget principal for this create (#601
		// part 2) — the seam refuses when their rolling budget is exhausted.
		RequestedBy: approval.UserEmail,
	}
	if t, ok := p.RunAtTime(); ok {
		req.RunAt = &t
	}

	// Bound the create call so the approval HTTP request returns promptly.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	res, err := s.scheduleTask(ctx, req)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("Scheduled task created.\n")
	if req.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", req.Title)
	}
	switch {
	case req.Cron != "":
		fmt.Fprintf(&b, "Schedule: recurring (cron %q)\n", req.Cron)
	case req.RunAt != nil:
		fmt.Fprintf(&b, "Schedule: one-time at %s\n", req.RunAt.Format(time.RFC3339))
	default:
		b.WriteString("Schedule: runs as soon as a worker is free\n")
	}
	if !res.NextRun.IsZero() {
		fmt.Fprintf(&b, "Next run: %s\n", res.NextRun.Format(time.RFC3339))
	}
	b.WriteString(describeTaskConnectors(p.Connectors, p.AlwaysOnConnectors))
	b.WriteString("\n")
	fmt.Fprintf(&b, "Task id: %s (status: %s)\n", res.ID, res.Status)
	if link := s.orchestratorTaskLink(); link != "" {
		fmt.Fprintf(&b, "View / manage it in the Operations Center: %s", link)
	} else {
		b.WriteString("View / manage it in the Operations Center.")
	}
	return strings.TrimSpace(b.String()), nil
}

// rejectionMessages returns the (claim, history) result strings to record when a
// user declines a staged approval, worded for the tool kind so the transcript
// reads honestly. The email wording is reserved for the tools that actually
// send email — a declined generic critical tool (a pages deploy, a deal write)
// used to be recorded as "User declined to send this email", which misread in
// the transcript and misinformed the next turn's model.
func rejectionMessages(toolName string) (claim, history string) {
	switch {
	case toolName == tools.ScheduleTaskToolName:
		return "User declined to create the scheduled task.",
			"User declined to create the scheduled task. No task was created."
	case toolName == tools.ManageTasksToolName:
		return "User declined the task change.",
			"User declined the task change. No tasks were modified."
	case toolName == tools.EnableMCPConnectorsToolName:
		return "User declined to enable the suggested MCP connector(s).",
			"User declined to enable the suggested MCP connector(s). Do not retry enable_mcp_connectors for these connectors unless the user asks."
	case tools.IsEmailToolName(toolName):
		return "User declined to send.", "User declined to send this email."
	default:
		return "User declined this action.",
			fmt.Sprintf("User declined the %s call. The action was not taken.", toolName)
	}
}

// actionVerb labels a failed staged action for the result_text prefix so the UI
// reads correctly per tool kind ("schedule failed: …" vs "send failed: …").
func actionVerb(toolName string) string {
	switch {
	case toolName == tools.ScheduleTaskToolName:
		return "schedule"
	case toolName == tools.ManageTasksToolName:
		return "task change"
	case toolName == tools.EnableMCPConnectorsToolName:
		return "enable connectors"
	case tools.IsEmailToolName(toolName):
		return "send"
	default:
		return "action"
	}
}

// orchestratorTaskLink returns a user-facing URL to the Operations Center, or ""
// when no externally-reachable base URL is configured. We deliberately link to
// the orchestrator landing page rather than synthesizing a per-task deep link:
// the web app has no per-task route, so a /tasks/<id> URL would 404 (honesty).
func (s *Server) orchestratorTaskLink() string {
	if s.cfg == nil || s.cfg.PublicBaseURL == "" {
		return ""
	}
	return strings.TrimRight(s.cfg.PublicBaseURL, "/") + "/orchestrator"
}

// appendToolResultToHistory writes a synthetic tool_result row so the
// conversation transcript reflects the outcome of the (async) approval.
func appendToolResultToHistory(ctx context.Context, st chatStore, convID, toolName, callID, text string) {
	entry := agent.HistoryEntry{
		Role: "tool",
		Type: "tool_result",
	}
	payload, _ := json.Marshal(map[string]any{
		"id":     callID,
		"name":   toolName,
		"text":   text,
		"is_err": false,
	})
	entry.Content = payload
	if _, err := st.AppendHistory(ctx, convID, []agent.HistoryEntry{entry}); err != nil {
		log.Printf("AppendHistory (approval result): %v", err)
	}
}

// ── helper on Manager: expose the MCP client to the approval path ──
// (added via a small method on Manager in agent/agent.go)
