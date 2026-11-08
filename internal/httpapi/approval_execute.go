package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

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
	if approval.ToolName == tools.PlaywrightBrowserToolName {
		return s.runStagedPlaywrightBrowser(ctx, approval)
	}
	if approval.ToolName == tools.ConnectorStudioToolName {
		return s.runStagedConnectorStudio(ctx, approval)
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
	// Email attachment paths were checked when the card was staged, but the
	// workspace stays writable while the card waits. Resolve them again now,
	// through the conversation's workspace, and send private copies of the
	// files so the connector reads exactly what was checked. The copies stay
	// on disk until the dispatch has let go of args (the argument hold below
	// removes them); until the hold takes them over, any early return removes
	// them here.
	releaseAttachments := func() {}
	if tools.IsEmailToolName(approval.ToolName) {
		cleanup, err := tools.SnapshotEmailAttachments(approval.ConversationID, args, s.outboundAttachmentStagingDir())
		if err != nil {
			return "", err
		}
		releaseAttachments = cleanup
	}
	attachmentsHeld := false
	defer func() {
		if !attachmentsHeld {
			releaseAttachments()
		}
	}()
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
	// charged, and the attachment copies stay on disk, until the dispatch has
	// let go of args: when CallMCP returns AND, for an out-of-process call
	// cancelled mid-flight, when the broker child has answered
	// (mcp.RetainArguments). The hold runs both releases then.
	resolved, err := s.resolveStagedFileReferences(callCtx, approval, catalogInputSchema(catalog, server, tool), args)
	if err != nil {
		return "", err
	}
	attachmentsHeld = true
	callCtx, hold := mcp.WithArgumentHold(callCtx, func() {
		resolved()
		releaseAttachments()
	})
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
			// our own row, and it is rendered with %q so any CR/LF would be escaped.
			logging.From(ctx).Error("approval: close MCP scope failed", "approval_id", safe.LogSafe(approval.ID), "err", err)
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
// if two catalog identities flatten to the same name, the longest server name
// wins.
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

func (s *Server) runStagedPlaywrightBrowser(ctx context.Context, approval *store.Approval) (string, error) {
	var params tools.PlaywrightBrowserParams
	if err := json.Unmarshal([]byte(approval.ArgsJSON), &params); err != nil {
		return "", fmt.Errorf("parse playwright_browser args: %w", err)
	}
	var conv *store.Conversation
	if s.store != nil && strings.TrimSpace(approval.ConversationID) != "" {
		conv, _ = s.getOwnedOrMultiplayerConversation(ctx, approval.UserEmail, approval.ConversationID)
	}
	pctx := s.newPlaywrightBrowserContext(conv, approval.UserEmail)
	return tools.RunPlaywrightBrowserForApproval(ctx, pctx, params)
}

func (s *Server) runStagedConnectorStudio(ctx context.Context, approval *store.Approval) (string, error) {
	var params tools.ConnectorStudioParams
	if err := json.Unmarshal([]byte(approval.ArgsJSON), &params); err != nil {
		return "", fmt.Errorf("parse connector_studio args: %w", err)
	}
	var conv *store.Conversation
	if s.store != nil && strings.TrimSpace(approval.ConversationID) != "" {
		conv, _ = s.getOwnedOrMultiplayerConversation(ctx, approval.UserEmail, approval.ConversationID)
	}
	handler := s.newConnectorStudioHandler(conv, approval.UserEmail)
	res, err := handler(ctx, params)
	if err != nil {
		return "", err
	}
	out, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode connector_studio result: %w", err)
	}
	return string(out), nil
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
	var isolatedConv *store.Conversation
	if s.store != nil && approval != nil && approval.ConversationID != "" {
		conv, cerr := s.getOwnedOrMultiplayerConversation(ctx, approval.UserEmail, approval.ConversationID)
		if cerr != nil {
			return nil, func() {}, fmt.Errorf("resolve conversation for approval workspace: %w", cerr)
		}
		if conv == nil {
			return nil, func() {}, fmt.Errorf("resolve conversation for approval workspace: conversation %s not found", approval.ConversationID)
		}
		if err := s.activateTurnPrivateWorkspace(conv, approval.UserEmail); err != nil {
			return nil, func() {}, fmt.Errorf("isolate participant workspace for approval: %w", err)
		}
		s.materializeUserSkills(ctx, approval.UserEmail, approval.ConversationID)
		isolatedConv = conv
	}
	sb, cleanup, err := takeStagedBashSandbox(ctx, pool, lockdown)
	if err != nil {
		if isolatedConv != nil {
			s.restoreTurnPrivateWorkspace(isolatedConv, approval.UserEmail)
		}
		return nil, func() {}, err
	}
	if isolatedConv != nil {
		origCleanup := cleanup
		cleanup = func() {
			if sb != nil {
				sb.Close()
			}
			if origCleanup != nil {
				origCleanup()
			}
			s.restoreTurnPrivateWorkspace(isolatedConv, approval.UserEmail)
		}
	}
	return sb, cleanup, nil
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
	conv, err := s.getOwnedOrMultiplayerConversation(ctx, approval.UserEmail, approval.ConversationID)
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
		fmt.Fprintf(&b, "View / manage it on the Background tasks page: %s", link)
	} else {
		b.WriteString("View / manage it on the Background tasks page.")
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
func appendToolResultToHistory(ctx context.Context, st chatStore, convID, toolName, callID, text string, senderEmail ...string) {
	entry := agent.HistoryEntry{
		Role: "tool",
		Type: "tool_result",
	}
	payloadMap := map[string]any{
		"id":     callID,
		"name":   toolName,
		"text":   text,
		"is_err": false,
	}
	if len(senderEmail) > 0 && strings.TrimSpace(senderEmail[0]) != "" {
		payloadMap["sender_email"] = strings.TrimSpace(senderEmail[0])
	}
	payload, _ := json.Marshal(payloadMap)
	entry.Content = payload
	if _, err := st.AppendHistory(ctx, convID, []agent.HistoryEntry{entry}); err != nil {
		logging.From(ctx).Error("AppendHistory failed (approval result)", "err", err)
	}
}

// outboundAttachmentStagingDir is where an approved email's attachments are
// copied for the send: under the host-side attachment directory, which no
// sandbox mounts. Empty (no config) means the process temp dir.
func (s *Server) outboundAttachmentStagingDir() string {
	if s.cfg == nil {
		return ""
	}
	return tools.OutboundAttachmentStagingDir(s.cfg.EmailAttachmentDir)
}
