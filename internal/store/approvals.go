package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// AutoApproveConnectorsRequest carries the inputs for AutoApproveEnableMCPConnectors.
type AutoApproveConnectorsRequest struct {
	UserEmail         string
	ConversationID    string
	ToolName          string
	ToolCallID        string
	ArgsJSON          string
	ResultText        string
	ExpiresAt         int64
	MergedServers     []string
	AllowResume       bool
	MaxPerHour        int
	MaxPending        int
	IgnoreOutstanding func(approvalID string) bool
}

// AutoApproveConnectorsResult reports what AutoApproveEnableMCPConnectors committed.
type AutoApproveConnectorsResult struct {
	Approval      *Approval
	Superseded    int64
	ResumeInput   *InputQueueRow
	ResumeClaimed []ApprovalResumeItem
	ResumeSkipped string
	RecentResumes int
}

// AutoApproveEnableMCPConnectors atomically supersedes any pending connector
// approvals in ConversationID, persists MergedServers into the conversation's
// optional_mcp_servers_enabled column, inserts the approved audit row, and (when
// AllowResume is true and no other approval or rate/queue limit blocks it)
// enqueues the continuation 'resume' input row in a single database
// transaction. When the continuation cannot be enqueued immediately, the audit
// row is committed with resume_state = 'skipped' (never 'armed') so no
// asynchronous retry or later settlement can launch a duplicate resume turn.
func (s *Store) AutoApproveEnableMCPConnectors(
	ctx context.Context,
	req AutoApproveConnectorsRequest,
) (AutoApproveConnectorsResult, error) {
	var out AutoApproveConnectorsResult
	mergedServers := req.MergedServers
	if mergedServers == nil {
		mergedServers = []string{}
	}
	payload, err := json.Marshal(mergedServers)
	if err != nil {
		return out, fmt.Errorf("marshal optional mcp servers: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()

	owner, err := s.lockAndVerifyOwnedOrMultiplayerTx(ctx, tx, req.UserEmail, req.ConversationID)
	if err != nil {
		return out, err
	}

	now := time.Now().Unix()
	supRes, err := tx.ExecContext(ctx,
		`UPDATE approvals
		    SET status = 'rejected',
		        result_text = 'Superseded by a newer call to this tool.',
		        resolved_at = $1,
		        resume_state = CASE WHEN resume_state = 'armed' THEN 'superseded' ELSE resume_state END
		  WHERE conversation_id = $2
		    AND tool_name = $3
		    AND ($4 = '' OR LOWER(user_email) = LOWER($4))
		    AND status = 'pending'`,
		now, req.ConversationID, req.ToolName, strings.TrimSpace(req.UserEmail),
	)
	if err != nil {
		return out, err
	}
	out.Superseded, _ = supRes.RowsAffected()

	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET optional_mcp_servers_enabled = $1, updated_at = $2
		  WHERE id = $3 AND user_email = $4 AND deleted_at IS NULL`,
		payload, now, req.ConversationID, owner,
	); err != nil {
		return out, err
	}

	resumeUser := strings.TrimSpace(req.UserEmail)
	if resumeUser == "" {
		resumeUser = owner
	}

	canEnqueue := req.AllowResume
	if canEnqueue {
		hasOutstanding, qerr := hasOutstandingApprovalsTx(ctx, tx, req.ConversationID, resumeUser, req.IgnoreOutstanding)
		if qerr != nil {
			return out, qerr
		}
		if hasOutstanding {
			canEnqueue = false
		}
	}

	if canEnqueue {
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM chat_input_queue
			  WHERE conversation_id = $1 AND mode = 'resume' AND created_at > $2
			    AND ($3 = '' OR LOWER(user_email) = LOWER($3))`,
			req.ConversationID, now-3600, resumeUser,
		).Scan(&out.RecentResumes); err != nil {
			return out, err
		}
		var pending int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM chat_input_queue WHERE conversation_id = $1 AND state = 'queued'`,
			req.ConversationID,
		).Scan(&pending); err != nil {
			return out, err
		}
		switch {
		case req.MaxPerHour > 0 && out.RecentResumes >= req.MaxPerHour:
			out.ResumeSkipped = ApprovalResumeSkipRateLimited
			canEnqueue = false
		case req.MaxPending > 0 && pending >= req.MaxPending:
			out.ResumeSkipped = ApprovalResumeSkipQueueFull
			canEnqueue = false
		}
	}

	id := uuid.NewString()
	var expiresArg any
	if req.ExpiresAt > 0 {
		expiresArg = req.ExpiresAt
	}
	resumeState := ApprovalResumeSkipped
	if canEnqueue {
		resumeState = ApprovalResumeClaimed
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO approvals (
			id, conversation_id, user_email, tool_name, tool_call_id, args_json,
			status, result_text, created_at, resolved_at, expires_at, is_err, resume_state
		) VALUES ($1, $2, $3, $4, $5, $6, 'approved', $7, $8, $8, $9, FALSE, $10)`,
		id, req.ConversationID, req.UserEmail, req.ToolName, req.ToolCallID, req.ArgsJSON,
		req.ResultText, now, expiresArg, resumeState,
	); err != nil {
		return out, err
	}

	var priorClaimed []ApprovalResumeItem
	var priorIDs []string
	if canEnqueue || out.ResumeSkipped != "" {
		var cerr error
		priorClaimed, priorIDs, cerr = claimSettledArmedApprovalsTx(ctx, tx, req.ConversationID, resumeUser)
		if cerr != nil {
			return out, cerr
		}
	}

	if canEnqueue {
		priorClaimed = append(priorClaimed, ApprovalResumeItem{
			ID:       id,
			ToolName: req.ToolName,
			Outcome:  ApprovalResumeOutcomeApproved,
		})
		out.ResumeClaimed = priorClaimed
		firstID := id
		if len(priorIDs) > 0 {
			firstID = priorIDs[0]
		}
		if err := supersedePendingModelSuggestionsTx(ctx, tx, req.ConversationID, now); err != nil {
			return out, err
		}
		row, _, err := s.insertInputTx(ctx, tx, InputQueueRow{
			ID:             uuid.NewString(),
			ConversationID: req.ConversationID,
			UserEmail:      resumeUser,
			ClientInputID:  "approval-resume:" + firstID,
			Message:        ApprovalResumeInputText(out.ResumeClaimed),
			Attachments:    "[]",
			Mode:           InputModeResume,
			State:          InputStateQueued,
		})
		if err != nil {
			return out, err
		}
		out.ResumeInput = &row
	} else if out.ResumeSkipped != "" {
		if len(priorIDs) > 0 {
			if err := markApprovalResumeTx(ctx, tx, priorIDs, ApprovalResumeSkipped); err != nil {
				return out, err
			}
		}
		note := ApprovalResumeSkippedNotice(out.ResumeSkipped, req.MaxPerHour)
		if err := s.appendNoticeTx(ctx, tx, req.ConversationID, NoticeApprovalResumeSkipped, note); err != nil {
			return out, err
		}
	}

	if err := tx.Commit(); err != nil {
		return out, err
	}
	out.Approval = &Approval{
		ID: id, ConversationID: req.ConversationID, UserEmail: req.UserEmail,
		ToolName: req.ToolName, ToolCallID: req.ToolCallID, ArgsJSON: req.ArgsJSON,
		Status: approvalStatusApproved, ResultText: req.ResultText,
		CreatedAt: now, ResolvedAt: now, ExpiresAt: req.ExpiresAt,
		IsErr: sql.NullBool{Bool: false, Valid: true}, ResumeState: resumeState,
	}
	return out, nil
}

func hasOutstandingApprovalsTx(ctx context.Context, tx *sql.Tx, convID, userEmail string, ignore func(string) bool) (bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM approvals
		  WHERE conversation_id = $1
		    AND ($3 = '' OR LOWER(user_email) = LOWER($3))
		    AND ((status = 'pending' AND tool_name NOT IN ('preview_email', 'suggest_advanced_model'))
		      OR (status = 'approved' AND is_err IS NULL AND result_text = $2))
		  ORDER BY created_at, id`,
		convID, ApprovalExecutingSentinel, strings.TrimSpace(userEmail),
	)
	if err != nil {
		return false, err
	}
	hasOutstanding := false
	for rows.Next() {
		var outID string
		if err := rows.Scan(&outID); err != nil {
			_ = rows.Close()
			return false, err
		}
		if ignore != nil && ignore(outID) {
			continue
		}
		hasOutstanding = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	_ = rows.Close()
	return hasOutstanding, nil
}

func claimSettledArmedApprovalsTx(ctx context.Context, tx *sql.Tx, convID, userEmail string) ([]ApprovalResumeItem, []string, error) {
	rows, err := tx.QueryContext(ctx,
		`UPDATE approvals SET resume_state = 'claimed'
		  WHERE conversation_id = $1 AND resume_state = 'armed' AND status <> 'pending'
		    AND ($3 = '' OR LOWER(user_email) = LOWER($3))
		    AND NOT (status = 'approved' AND is_err IS NULL AND result_text = $2)
		  RETURNING id, tool_name, status, COALESCE(result_text, ''), is_err, COALESCE(resolved_at, 0), created_at`,
		convID, ApprovalExecutingSentinel, strings.TrimSpace(userEmail),
	)
	if err != nil {
		return nil, nil, err
	}
	type claimedRow struct {
		item       ApprovalResumeItem
		resolvedAt int64
		createdAt  int64
	}
	var claimed []claimedRow
	for rows.Next() {
		var (
			c          claimedRow
			status, rt string
			isErr      sql.NullBool
		)
		if err := rows.Scan(&c.item.ID, &c.item.ToolName, &status, &rt, &isErr, &c.resolvedAt, &c.createdAt); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		c.item.Outcome = approvalResumeOutcome(status, rt)
		c.item.IsErr = status == "approved" && isErr.Valid && isErr.Bool
		claimed = append(claimed, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, err
	}
	_ = rows.Close()
	sort.Slice(claimed, func(i, j int) bool {
		if claimed[i].resolvedAt != claimed[j].resolvedAt {
			return claimed[i].resolvedAt < claimed[j].resolvedAt
		}
		if claimed[i].createdAt != claimed[j].createdAt {
			return claimed[i].createdAt < claimed[j].createdAt
		}
		return claimed[i].item.ID < claimed[j].item.ID
	})
	items := make([]ApprovalResumeItem, len(claimed))
	ids := make([]string, len(claimed))
	for i, c := range claimed {
		items[i] = c.item
		ids[i] = c.item.ID
	}
	return items, ids, nil
}

// StageEnableMCPConnectorsApproval atomically supersedes any pending connector
// approvals for (convID, toolName) and inserts a new pending approval row with
// resume_state = 'armed' in a single database transaction. If buildArgs is
// non-nil, it receives the JSON payloads of any pending rows being superseded
// so the caller can merge earlier pending connector requests from the same step
// into the newly staged card's payload before inserting it.
func (s *Store) StageEnableMCPConnectorsApproval(
	ctx context.Context,
	userEmail, convID, toolName, toolCallID, argsJSON string,
	expiresAt int64,
	buildArgs func(supersededArgsJSON []string) string,
) (*Approval, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := s.lockAndVerifyOwnedOrMultiplayerTx(ctx, tx, userEmail, convID); err != nil {
		return nil, 0, err
	}

	now := time.Now().Unix()
	rows, err := tx.QueryContext(ctx,
		`UPDATE approvals
		    SET status = 'rejected',
		        result_text = 'Superseded by a newer call to this tool.',
		        resolved_at = $1,
		        resume_state = CASE WHEN resume_state = 'armed' THEN 'superseded' ELSE resume_state END
		  WHERE conversation_id = $2
		    AND tool_name = $3
		    AND ($4 = '' OR LOWER(user_email) = LOWER($4))
		    AND status = 'pending'
		  RETURNING COALESCE(args_json, '')`,
		now, convID, toolName, strings.TrimSpace(userEmail),
	)
	if err != nil {
		return nil, 0, err
	}
	var supersededArgs []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		supersededArgs = append(supersededArgs, raw)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, err
	}
	_ = rows.Close()
	superseded := int64(len(supersededArgs))
	if buildArgs != nil && len(supersededArgs) > 0 {
		if mergedJSON := buildArgs(supersededArgs); mergedJSON != "" {
			argsJSON = mergedJSON
		}
	}

	id := uuid.NewString()
	var expiresArg any
	if expiresAt > 0 {
		expiresArg = expiresAt
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO approvals (
			id, conversation_id, user_email, tool_name, tool_call_id, args_json,
			status, created_at, expires_at, resume_state
		) VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $8, $9)`,
		id, convID, userEmail, toolName, toolCallID, argsJSON,
		now, expiresArg, ApprovalResumeArmed,
	); err != nil {
		return nil, 0, err
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return &Approval{
		ID: id, ConversationID: convID, UserEmail: userEmail,
		ToolName: toolName, ToolCallID: toolCallID, ArgsJSON: argsJSON,
		Status: approvalStatusPending, CreatedAt: now, ExpiresAt: expiresAt,
		ResumeState: ApprovalResumeArmed,
	}, superseded, nil
}

// Approval is a pending high-risk tool call awaiting user consent.
type Approval struct {
	ID             string
	ConversationID string
	UserEmail      string
	ToolName       string
	ArgsJSON       string
	Status         string // pending|approved|rejected
	ResultText     string
	CreatedAt      int64
	ResolvedAt     int64
	// ToolCallID is the id the agent assigned to the tool_call event
	// in the conversation history. Populated when the orchestration
	// layer stages the call; empty for older rows. The post-approval
	// resolver uses this to write the real tool_result back under the
	// same id the chip is keyed on, so the UI updates instead of
	// orphaning the result row.
	ToolCallID string
	// ExpiresAt is the unix-seconds deadline after which a still-pending
	// approval is auto-denied by the server-side expiry sweep — the
	// default-DENY-on-timeout contract for the web approval path (#225).
	// 0 means "no expiry" (legacy rows, or a non-positive resolved timeout);
	// the sweep and the UI countdown both treat 0 as "never expires".
	ExpiresAt int64
	// MCPServer / MCPAccount record the credential seat that was active when
	// the call was staged (#167 residual 2). A card outlives its turn scope,
	// so execution reopens a scope from this pair instead of silently running
	// on the broker's default bundle seat. MCPServer is empty for native tools
	// and for rows staged before the columns existed; MCPAccount empty means
	// the default seat.
	MCPServer  string
	MCPAccount string
	// IsErr is the staged tool's execution outcome, written atomically with
	// result_text by SetApprovalResult. Valid=false means unknown: still
	// executing after the claim sentinel, a rejection, or a legacy approved
	// row whose run finished before this column existed. status stays consent
	// (pending|approved|rejected) even when IsErr is true.
	IsErr sql.NullBool
	// CardJSON is the readable approval card a bundle-declared describer
	// produced when the call was staged (docs/APPROVAL-CARD-DESCRIBERS.md):
	// canonical JSON, validated before it was stored and again on every read.
	// Empty when no describer is declared, the describer failed, or the row
	// predates the column — the client then renders the generic card.
	CardJSON string
	// ResumeState is the resume-after-approval bookkeeping (migration 072,
	// docs/RESUME-AFTER-APPROVAL.md): "" (the tool did not opt in, or the row
	// predates the column), ApprovalResumeArmed, ApprovalResumeClaimed,
	// ApprovalResumeSkipped, ApprovalResumeSuperseded or ApprovalResumeDropped.
	ResumeState string
	// GroupID is the approval group the card joined at staging (migration
	// 075, docs/GROUPED-APPROVALS.md): the staging turn's id, for a tool the
	// bundle lists in critical_tool_group_approval. Empty for every other row.
	GroupID string
	// ProgressJSON is the latest progress an approved, still-executing call
	// reported (migration 076, docs/APPROVAL-PROGRESS.md): canonical JSON
	// written by SetApprovalProgress. Empty when none.
	ProgressJSON string
	// ResultCardJSON is the outcome card the call's own result carried
	// (migration 077, docs/APPROVAL-RESULT-CARDS.md): canonical JSON written
	// once by SetApprovalResultCard. Empty when none.
	ResultCardJSON string
	// NarrowedJSON records a card the person approved narrowed (migration
	// 079, docs/APPROVAL-NARROWING.md): canonical JSON {"arg", "staged",
	// "approved"} written by ClaimApprovalNarrowed in the claim itself.
	// ArgsJSON stays the staged call; execution replaces only Arg with
	// Approved. Empty for every card approved whole.
	NarrowedJSON string
	// StopRequestedAt is when the person pressed Stop on the running call
	// (migration 080, docs/APPROVAL-STOP.md), unix seconds, written once by
	// MarkApprovalStopRequested while the call executes. 0 when no stop was
	// asked for.
	StopRequestedAt int64
	// NarrowableArg is the argument a person may narrow on this card, frozen
	// at staging by SetApprovalNarrowableArg (migration 079,
	// docs/APPROVAL-NARROWING.md). Empty = the card approves its call whole.
	NarrowableArg string
}

// approvalCols is the one column list the full approval readers select
// (GetApproval, ListPendingApprovals, ListResolvedApprovals,
// ListExpiredApprovals, LatestApprovalByTool), in scanApproval's order, so
// every reader fills every Approval field the same way. ListExecutingApprovals
// stays narrow on purpose: it is polled for settlement and must not download
// completed cards or argument bodies.
const approvalCols = `id, conversation_id, user_email, tool_name, args_json, status,
	COALESCE(result_text, ''), created_at, COALESCE(resolved_at, 0),
	COALESCE(tool_call_id, ''), COALESCE(expires_at, 0),
	COALESCE(mcp_server, ''), COALESCE(mcp_account, ''),
	is_err, COALESCE(card_json, ''), resume_state, COALESCE(group_id, ''), COALESCE(progress_json, ''),
	COALESCE(result_card_json, ''), COALESCE(narrowed_json, ''), COALESCE(stop_requested_at, 0), COALESCE(narrowable_arg, '')`

// scanApproval scans one row selected with approvalCols.
func scanApproval(row rowScanner) (Approval, error) {
	var a Approval
	err := row.Scan(&a.ID, &a.ConversationID, &a.UserEmail, &a.ToolName,
		&a.ArgsJSON, &a.Status, &a.ResultText, &a.CreatedAt, &a.ResolvedAt, &a.ToolCallID, &a.ExpiresAt,
		&a.MCPServer, &a.MCPAccount, &a.IsErr, &a.CardJSON, &a.ResumeState, &a.GroupID, &a.ProgressJSON, &a.ResultCardJSON,
		&a.NarrowedJSON, &a.StopRequestedAt, &a.NarrowableArg)
	return a, err
}

// scanApprovals collects every row of a query selected with approvalCols.
func scanApprovals(rows *sql.Rows) ([]Approval, error) {
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// approvalClaimable is the claim predicate on an approval row: still pending
// and not past its expires_at deadline. A still-pending approval past its
// deadline is default-deny (#225 / #1109) whether or not the expiry sweep has
// run yet; NULL/0 expires_at never expires. $1 is always the caller's "now".
const approvalClaimable = `status = 'pending' AND (expires_at IS NULL OR expires_at = 0 OR expires_at > $1)`

// ListPendingApprovals returns every pending approval for a conversation,
// oldest first. Used on page reload to re-render approval cards that were
// staged but never resolved in the previous browser session.
func (s *Store) ListPendingApprovals(ctx context.Context, userEmail, convID string) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+approvalCols+`
		 FROM approvals
		 WHERE conversation_id = $1 AND ($2 = '' OR LOWER(user_email) = LOWER($2)) AND status = 'pending'
		 ORDER BY created_at ASC`,
		convID, strings.TrimSpace(userEmail),
	)
	if err != nil {
		return nil, err
	}
	return scanApprovals(rows)
}

// MaxResolvedApprovalsPerConversation caps how many resolved approvals the
// conversation GET re-hydrates as transcript cards. Approvals are one row per
// human-gated action, so a conversation rarely accumulates more than a
// handful; the cap only bounds a pathological chat's payload. Newest-first is
// applied in SQL so the cap keeps the RECENT cards, then callers re-sort.
const MaxResolvedApprovalsPerConversation = 100

// ListResolvedApprovals returns the most recent non-pending approvals for a
// conversation, oldest first (capped at MaxResolvedApprovalsPerConversation).
// Used on page reload so resolved approval cards — the "Email sent ✓" outcome,
// a notify-mode "ran without asking" record and its undo hint, a timed-out
// card — survive a reload instead of the transcript silently changing shape
// the first time the user leaves and comes back (#1153's record contract).
func (s *Store) ListResolvedApprovals(ctx context.Context, userEmail, convID string) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+approvalCols+`
		 FROM approvals
		 WHERE conversation_id = $1 AND user_email = $2 AND status <> 'pending'
		 ORDER BY created_at DESC
		 LIMIT $3`,
		convID, userEmail, MaxResolvedApprovalsPerConversation,
	)
	if err != nil {
		return nil, err
	}
	out, err := scanApprovals(rows)
	if err != nil {
		return nil, err
	}
	// Oldest first for display, matching ListPendingApprovals.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ListExecutingApprovals returns settlement identifiers without downloading
// completed cards or their argument bodies. Filter before any display limit so
// an older in-flight execution cannot be hidden by newer completed approvals.
func (s *Store) ListExecutingApprovals(ctx context.Context, userEmail, convID, sentinel string) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, tool_name, COALESCE(tool_call_id, ''), COALESCE(mcp_server, ''), COALESCE(mcp_account, ''), COALESCE(progress_json, ''), COALESCE(narrowed_json, ''), COALESCE(stop_requested_at, 0) FROM approvals
		WHERE conversation_id = $1 AND user_email = $2 AND status = 'approved'
		AND is_err IS NULL AND result_text = $3 ORDER BY created_at`, convID, userEmail, sentinel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a := Approval{Status: "approved", ResultText: sentinel}
		if err := rows.Scan(&a.ID, &a.ToolName, &a.ToolCallID, &a.MCPServer, &a.MCPAccount, &a.ProgressJSON, &a.NarrowedJSON, &a.StopRequestedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ApprovalExecutingSentinel represents a claimed action with no recorded outcome.
const ApprovalExecutingSentinel = "Approved — executing…"

// RecoverStrandedApprovals runs before accepting requests at startup. An action
// may have committed remotely before the process died, so record uncertainty,
// never failure or a retry: repeating it could duplicate a side effect.
func (s *Store) RecoverStrandedApprovals(ctx context.Context) (int64, error) {
	const outcome = ApprovalRecoveryOutcome
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `UPDATE approvals SET result_text = $1
		WHERE status = 'approved' AND is_err IS NULL AND result_text = $2
		AND NOT EXISTS (SELECT 1 FROM turns WHERE turns.conversation_id = approvals.conversation_id AND turns.status = 'running')
		RETURNING id, conversation_id, tool_name, COALESCE(tool_call_id, ''), COALESCE(narrowed_json, ''), COALESCE(stop_requested_at, 0)`, outcome, ApprovalExecutingSentinel)
	if err != nil {
		return 0, err
	}
	var recovered []Approval
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.ConversationID, &a.ToolName, &a.ToolCallID, &a.NarrowedJSON, &a.StopRequestedAt); err != nil {
			_ = rows.Close()
			return 0, err
		}
		recovered = append(recovered, a)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, a := range recovered {
		callID := a.ToolCallID
		if callID == "" {
			callID = a.ID
		}
		text := ApprovalRecoveryText(a.NarrowedJSON, a.StopRequestedAt > 0)
		payload, err := json.Marshal(map[string]any{"id": callID, "name": a.ToolName, "text": text, "is_err": true})
		if err != nil {
			return 0, err
		}
		if _, err := s.appendHistoryTx(ctx, tx, a.ConversationID, []agent.HistoryEntry{{Role: "tool", Type: "tool_result", Content: payload}}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(recovered)), nil
}

// CreateApproval stages a pending approval and returns the row.
// toolCallID is the agent-assigned id of the tool_call event being
// staged; empty is allowed (older code paths) but populating it lets
// the post-approval resolver write its tool_result back under the same
// id the UI chip is keyed on.
// expiresAt is the unix-seconds default-deny deadline for the staged approval
// (#225); pass 0 for "no expiry" (the column is stored NULL and the server-side
// expiry sweep skips the row).
// seat records the credential {server, account} the staging turn was running on
// (#167 residual 2). A zero ApprovalSeat is a native tool or an unknown seat and
// stores NULL, which execution reads back as "the default bundle seat".
func (s *Store) CreateApproval(ctx context.Context, convID, userEmail, toolName, toolCallID, argsJSON string, expiresAt int64, seat ApprovalSeat) (*Approval, error) {
	id := uuid.NewString()
	now := time.Now().Unix()
	var expiresArg any // NULL when there is no timeout
	if expiresAt > 0 {
		expiresArg = expiresAt
	}
	// The seat is stored only when a server authored the call. Writing NULL for
	// a native tool keeps "no seat recorded" distinct from "the default seat was
	// deliberately chosen", which is what a recorded server + empty account means.
	var serverArg, accountArg any
	if seat.Server != "" {
		serverArg, accountArg = seat.Server, seat.Account
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO approvals (id, conversation_id, user_email, tool_name, tool_call_id, args_json, status, created_at, expires_at, mcp_server, mcp_account)
		 VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $8, $9, $10)`,
		id, convID, userEmail, toolName, toolCallID, argsJSON, now, expiresArg, serverArg, accountArg,
	)
	if err != nil {
		return nil, err
	}
	return &Approval{
		ID: id, ConversationID: convID, UserEmail: userEmail,
		ToolName: toolName, ToolCallID: toolCallID, ArgsJSON: argsJSON, Status: approvalStatusPending,
		CreatedAt: now, ExpiresAt: expiresAt,
		MCPServer: seat.Server, MCPAccount: seat.Account,
	}, nil
}

// SetApprovalCard attaches a describer's card to a still-pending approval the
// user owns. It reports whether a row was updated: a card for a row that was
// already resolved (or superseded) in between is dropped, never written onto
// a settled decision.
func (s *Store) SetApprovalCard(ctx context.Context, userEmail, approvalID, cardJSON string) (bool, error) {
	return s.setPendingApprovalColumn(ctx, "card_json", userEmail, approvalID, cardJSON)
}

// setPendingApprovalColumn writes one staging-time column on a still-pending
// approval the user owns, and reports whether a row was updated. column is
// one of the constant names its three callers pass, never request input.
func (s *Store) setPendingApprovalColumn(ctx context.Context, column, userEmail, approvalID, value string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		//nolint:gosec // G202: column is a constant from SetApprovalCard/SetApprovalNarrowableArg/SetApprovalGroup; every value is a bound placeholder.
		`UPDATE approvals SET `+column+` = $1 WHERE id = $2 AND user_email = $3 AND status = 'pending'`,
		value, approvalID, userEmail)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetApprovalNarrowableArg freezes, at staging, the argument a person may
// narrow on a still-pending approval the user owns (migration 079). It
// reports whether a row was updated.
func (s *Store) SetApprovalNarrowableArg(ctx context.Context, userEmail, approvalID, arg string) (bool, error) {
	return s.setPendingApprovalColumn(ctx, "narrowable_arg", userEmail, approvalID, arg)
}

// SetApprovalGroup records the approval group a still-pending approval the
// user owns joined at staging (docs/GROUPED-APPROVALS.md). It reports whether
// a row was updated: a row resolved or superseded in between keeps no group,
// so it can never be pulled into a later group decision.
func (s *Store) SetApprovalGroup(ctx context.Context, userEmail, approvalID, groupID string) (bool, error) {
	return s.setPendingApprovalColumn(ctx, "group_id", userEmail, approvalID, groupID)
}

// SetApprovalProgress records the latest progress of an approved call that is
// still executing (docs/APPROVAL-PROGRESS.md). It reports whether a row was
// updated: once the call's outcome is written (or the row was never claimed,
// or belongs to someone else) nothing changes, so a late update can never
// land on a settled card.
func (s *Store) SetApprovalProgress(ctx context.Context, userEmail, approvalID, progressJSON string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals SET progress_json = $1
		 WHERE id = $2 AND user_email = $3 AND status = 'approved' AND is_err IS NULL AND result_text = $4`,
		progressJSON, approvalID, userEmail, ApprovalExecutingSentinel)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// NotifyRecordResultPrefix opens the result text of every notify-mode record
// (#1153); httpapi writes it and tags reloaded rows `recorded` by it. Shared
// here so SetApprovalResultCard can tell a record row from a decided one.
const NotifyRecordResultPrefix = "Ran without asking"

// SetApprovalResultCard stores the outcome card a call's own result carried
// (docs/APPROVAL-RESULT-CARDS.md). It writes once, and only on a row whose
// outcome it can describe: an approved call that is still executing (the
// card is written just before SetApprovalResult records the outcome), or a
// notify-mode record (approved, is_err false, the record text). It reports
// whether a row was updated; a declined, failed or settled call, another
// user's row, or a row that already has a card is left unchanged.
func (s *Store) SetApprovalResultCard(ctx context.Context, userEmail, approvalID, cardJSON string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals SET result_card_json = $1
		 WHERE id = $2 AND user_email = $3 AND status = 'approved' AND result_card_json IS NULL
		   AND ((is_err IS NULL AND result_text = $4) OR (is_err = FALSE AND starts_with(result_text, $5)))`,
		cardJSON, approvalID, userEmail, ApprovalExecutingSentinel, NotifyRecordResultPrefix)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ApprovalSeat is the public credential selection a staged MCP call ran under.
// Both fields are configuration identifiers; no credential value is stored.
type ApprovalSeat struct {
	// Server is the bundle server name that exports the staged tool. Empty for
	// native tools (bash, preview_email).
	Server string
	// Account is the named seat, or empty for the default bundle seat.
	Account string
}

// GetApproval looks up a pending approval, scoped by user_email.
func (s *Store) GetApproval(ctx context.Context, userEmail, approvalID string) (*Approval, error) {
	a, err := scanApproval(s.db.QueryRowContext(ctx,
		`SELECT `+approvalCols+` FROM approvals WHERE id = $1 AND user_email = $2`,
		approvalID, userEmail,
	))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

// MaxExpiredApprovalsPerSweep bounds a single expiry-sweep batch so one sweep
// can't load an unbounded backlog into memory. A sweep that hits the cap logs
// and the next tick picks up the remainder (#225). Exported so the httpapi
// sweep can detect a full batch from a single source of truth.
const MaxExpiredApprovalsPerSweep = 500

// ListExpiredApprovals returns pending approvals whose expires_at deadline has
// passed (expires_at > 0 AND < now), oldest-deadline first, across ALL users
// and conversations. This is the read half of the server-side expiry sweep; the
// caller atomically claims and auto-denies each row (default-DENY-on-timeout).
// Rows with NULL/0 expires_at never expire and are excluded.
func (s *Store) ListExpiredApprovals(ctx context.Context, now int64) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+approvalCols+`
		 FROM approvals
		 WHERE status = 'pending' AND expires_at IS NOT NULL AND expires_at > 0 AND expires_at < $1
		 ORDER BY expires_at ASC
		 LIMIT $2`,
		now, MaxExpiredApprovalsPerSweep,
	)
	if err != nil {
		return nil, err
	}
	return scanApprovals(rows)
}

// Approval lifecycle statuses.
const (
	approvalStatusPending  = "pending"
	approvalStatusApproved = "approved"
	approvalStatusRejected = "rejected"
)

func validApprovalResolution(status string) bool {
	return status == approvalStatusApproved || status == approvalStatusRejected
}

// ResolveApproval marks the approval approved or rejected and records the
// tool result text. Safe to call twice — second write is no-op via guard.
func (s *Store) ResolveApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) error {
	if !validApprovalResolution(newStatus) {
		return fmt.Errorf("invalid approval status %q", newStatus)
	}
	// An approved ResolveApproval IS the outcome (notify-mode records), so
	// is_err=false is part of the write. Rejections leave is_err as it was
	// (NULL on a pending row).
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals SET status = $1, result_text = $2, resolved_at = $3,
		        is_err = CASE WHEN $1 = 'approved' THEN FALSE ELSE is_err END
		 WHERE id = $4 AND user_email = $5 AND status = 'pending'`,
		newStatus, resultText, time.Now().Unix(), approvalID, userEmail,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("approval not found or already resolved")
	}
	return nil
}

// ClaimApproval atomically transitions a pending approval to newStatus
// and reports whether this caller won the claim. The staged tool must
// only be fired by the winner — two concurrent approve requests (a
// double-click, a mobile retry, two open tabs) would otherwise both
// pass an in-memory "still pending" check and both send the email.
//
// Expired rows are not claimable: a still-pending approval past its
// expires_at deadline is default-deny (#225 / #1109), regardless of
// whether SweepExpiredApprovals has run yet. Rows with NULL/0
// expires_at never expire. The expiry sweep uses ClaimExpiredApproval
// so it can still flip those rows for notification/audit.
func (s *Store) claimApprovalForAuthorizedUser(ctx context.Context, userEmail, approvalID, resultText, narrowedJSON string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	var convID string
	err = tx.QueryRowContext(ctx,
		`SELECT conversation_id FROM approvals WHERE id = $2 AND user_email = $3 AND `+approvalClaimable,
		now, approvalID, userEmail,
	).Scan(&convID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := s.lockAndVerifyOwnedOrMultiplayerTx(ctx, tx, userEmail, convID); err != nil {
		if errors.Is(err, ErrConversationNotFound) {
			return false, nil
		}
		return false, err
	}
	// An empty narrowedJSON (a card approved whole) leaves narrowed_json as
	// it was — NULL on a pending row.
	res, err := tx.ExecContext(ctx,
		`UPDATE approvals SET status = 'approved', result_text = $2, resolved_at = $1,
		        narrowed_json = COALESCE(NULLIF($3, ''), narrowed_json)
		 WHERE id = $4 AND user_email = $5 AND `+approvalClaimable,
		now, resultText, narrowedJSON, approvalID, userEmail,
	)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ClaimApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) (bool, error) {
	if !validApprovalResolution(newStatus) {
		return false, fmt.Errorf("invalid approval status %q", newStatus)
	}
	if newStatus == approvalStatusApproved {
		return s.claimApprovalForAuthorizedUser(ctx, userEmail, approvalID, resultText, "")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals SET status = $2, result_text = $3, resolved_at = $1
		 WHERE id = $4 AND user_email = $5 AND `+approvalClaimable,
		time.Now().Unix(), newStatus, resultText, approvalID, userEmail,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkApprovalStopRequested records that the person pressed Stop on a call
// that is still executing (docs/APPROVAL-STOP.md): it sets stop_requested_at
// once, on the owner's approved row whose outcome is not recorded yet (the
// executing sentinel, no is_err). It reports whether this call set it; a
// second press, a settled card or another user's row changes nothing.
func (s *Store) MarkApprovalStopRequested(ctx context.Context, userEmail, approvalID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals SET stop_requested_at = $1
		 WHERE id = $2 AND user_email = $3 AND status = 'approved'
		   AND is_err IS NULL AND result_text = $4 AND stop_requested_at IS NULL`,
		time.Now().Unix(), approvalID, userEmail, ApprovalExecutingSentinel)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ClaimApprovalNarrowed is ClaimApproval for a card the person approved
// narrowed (docs/APPROVAL-NARROWING.md): the same claim predicate (pending,
// not expired, the user's row), resolving the row as approved with
// resultText (the executing sentinel) AND writing narrowedJSON in the same
// UPDATE. So a narrowed approval can never be recorded without what was
// narrowed, and execution can never read an approved row whose narrowing
// was lost: either both land or the card stays pending.
func (s *Store) ClaimApprovalNarrowed(ctx context.Context, userEmail, approvalID, resultText, narrowedJSON string) (bool, error) {
	if narrowedJSON == "" {
		return false, errors.New("ClaimApprovalNarrowed: empty narrowing record")
	}
	return s.claimApprovalForAuthorizedUser(ctx, userEmail, approvalID, resultText, narrowedJSON)
}

// ClaimApprovalAndSetModel resolves a pending approval as "approved" AND pins
// its conversation to model in ONE transaction. It exists for the
// suggest_advanced_model card, whose resolution IS the model flip: doing the
// two writes separately (claim, then SetModel) left a window where a transient
// failure on the second write recorded the approval as approved — with a
// result_text claiming the conversation was pinned — while the conversation
// stayed on the old model, and a retry then saw a resolved row and never
// re-attempted the pin. Here either both rows change or neither does: a lost
// claim returns (false, nil) with nothing written; a conversation that is
// missing, soft-deleted or not the user's returns ErrConversationNotFound and
// the claim is rolled back, so the approval stays pending and the caller can
// retry. Same claim predicate as ClaimApproval (pending + not expired).
func (s *Store) ClaimApprovalAndSetModel(ctx context.Context, userEmail, approvalID, resultText, convID, model string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	var pending int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM approvals WHERE id = $2 AND user_email = $3 AND `+approvalClaimable,
		now, approvalID, userEmail,
	).Scan(&pending); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	ownerEmail, err := s.lockAndVerifyOwnedOrMultiplayerTx(ctx, tx, userEmail, convID)
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE approvals SET status = 'approved', result_text = $2, resolved_at = $1, is_err = FALSE
		 WHERE id = $3 AND user_email = $4 AND `+approvalClaimable,
		now, resultText, approvalID, userEmail,
	)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	res, err = tx.ExecContext(ctx,
		`UPDATE conversations SET model = $1, updated_at = $2 WHERE id = $3 AND user_email = $4 AND deleted_at IS NULL`,
		model, now, convID, ownerEmail,
	)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, ErrConversationNotFound
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ClaimExpiredApproval atomically rejects (or otherwise resolves) a
// pending approval whose expires_at deadline has already passed. Used
// only by the expiry sweep (#225) so notification/audit still happens
// after ClaimApproval starts refusing expired rows (#1109).
func (s *Store) ClaimExpiredApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) (bool, error) {
	if !validApprovalResolution(newStatus) {
		return false, fmt.Errorf("invalid approval status %q", newStatus)
	}
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals SET status = $1, result_text = $2, resolved_at = $3
		 WHERE id = $4 AND user_email = $5 AND status = 'pending'
		   AND expires_at IS NOT NULL AND expires_at > 0 AND expires_at <= $6`,
		newStatus, resultText, now, approvalID, userEmail, now,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetApprovalResult records the staged tool's outcome on an
// already-claimed (non-pending) approval. result_text and is_err are
// written together so an idempotent retry cannot echo the claim sentinel
// (or a result without its error bit).
func (s *Store) SetApprovalResult(ctx context.Context, userEmail, approvalID, resultText string, isErr bool) error {
	// The next model turn must see the same outcome as the approval card.
	// Commit both together so a crash cannot leave APPROVAL_REQUIRED as the
	// only model-visible result of an action that already executed.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var convID, tool, callID string
	err = tx.QueryRowContext(ctx,
		`UPDATE approvals SET result_text = $1, is_err = $2
		 WHERE id = $3 AND user_email = $4 AND status <> 'pending'
		 RETURNING conversation_id, tool_name, COALESCE(NULLIF(tool_call_id, ''), id)`,
		resultText, isErr, approvalID, userEmail,
	).Scan(&convID, &tool, &callID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	payloadMap := map[string]any{"id": callID, "name": tool, "text": resultText, "is_err": isErr}
	if sender := strings.TrimSpace(userEmail); sender != "" {
		payloadMap["sender_email"] = sender
	}
	payload, err := json.Marshal(payloadMap)
	if err != nil {
		return err
	}
	if _, err := s.appendHistoryTx(ctx, tx, convID, []agent.HistoryEntry{{Role: "tool", Type: "tool_result", Content: payload}}); err != nil {
		return err
	}
	return tx.Commit()
}

// LatestApprovalByTool returns the most recent approval (any status)
// for a (conversation, tool) pair, or (nil, nil) if none exists. The
// suggest_advanced_model gate uses this to look up the prior card's
// disposition: an approved row stops re-suggestions for the rest of
// the conversation; a rejected row triggers a user-turn cooldown.
func (s *Store) LatestApprovalByTool(ctx context.Context, convID, toolName string) (*Approval, error) {
	a, err := scanApproval(s.db.QueryRowContext(ctx,
		`SELECT `+approvalCols+`
		 FROM approvals
		 WHERE conversation_id = $1 AND tool_name = $2
		 ORDER BY created_at DESC
		 LIMIT 1`,
		convID, toolName,
	))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

// CountUserMessagesAfterTimestamp returns the number of user-role
// messages in a conversation whose created_at is strictly greater than
// ts. Used by the suggest_advanced_model gate to enforce a
// "re-suggest after N user turns" cooldown — counting user-role text
// messages reflects actual user-driven turns rather than tool/assistant
// chatter inside a single turn.
func (s *Store) CountUserMessagesAfterTimestamp(ctx context.Context, convID string, ts int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages
		 WHERE conversation_id = $1 AND role = 'user' AND type = 'text' AND created_at > $2`,
		convID, ts,
	).Scan(&n)
	return n, err
}

// CountUserMessagesAfterResolution counts a conversation's user messages sent
// after an approval card was answered, for the suggest_advanced_model
// cooldown. Timestamps are whole seconds, so on their own they cannot order a
// message against an answer given in the same second. The answer's
// tool_result note (written under callID right after the claim) is the exact
// boundary in message order: only user messages with a later id count. The
// note is best-effort; without it the count falls back to messages strictly
// after the answer's second, which can only leave a same-second message out
// (a longer cooldown), never count one sent before the answer.
func (s *Store) CountUserMessagesAfterResolution(ctx context.Context, convID, callID string, resolvedAt int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`WITH note AS (
		   SELECT MAX(r.id) AS id FROM messages r
		   WHERE r.conversation_id = $1 AND r.type = 'tool_result'
		     AND r.created_at >= $3 AND r.content::jsonb->>'id' = $2)
		 SELECT COUNT(*) FROM messages m, note
		 WHERE m.conversation_id = $1 AND m.role = 'user' AND m.type = 'text'
		   AND CASE WHEN note.id IS NULL THEN m.created_at > $3
		            ELSE m.created_at >= $3 AND m.id > note.id END`,
		convID, callID, resolvedAt,
	).Scan(&n)
	return n, err
}

// SupersedePendingApprovals marks every pending approval for a
// (conversation, tool) pair as rejected, with a canned result text
// explaining it was superseded. Used when the agent stages a fresh
// approval for the same tool — e.g. retrying a preview_email after
// the first call contained garbage. Without this the UI accumulates
// stacked cards and the user has to dismiss each one manually.
// Returns the number of rows updated so the caller can decide
// whether to log or inject a history entry.
func (s *Store) SupersedePendingApprovals(ctx context.Context, userEmail, convID, toolName string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals
		   SET status = 'rejected',
		       result_text = 'Superseded by a newer call to this tool.',
		       resolved_at = $1,
		       -- A superseded card was never decided by anyone: it must not
		       -- start a resume turn (docs/RESUME-AFTER-APPROVAL.md). Its
		       -- replacement is armed in its own right.
		       resume_state = CASE WHEN resume_state = 'armed' THEN 'superseded' ELSE resume_state END
		 WHERE conversation_id = $2
		   AND tool_name = $3
		   AND ($4 = '' OR LOWER(user_email) = LOWER($4))
		   AND status = 'pending'`,
		time.Now().Unix(), convID, toolName, strings.TrimSpace(userEmail),
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// supersedePendingModelSuggestionsTx resolves any pending suggest_advanced_model
// cards in convID when a background patrol or approval-resume turn is enqueued,
// so a later "Switch & retry" click cannot truncate and re-submit the newer
// background turn instead of the user message that triggered the suggestion.
func supersedePendingModelSuggestionsTx(ctx context.Context, tx *sql.Tx, convID string, nowUnix int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE approvals
		   SET status = 'rejected',
		       result_text = 'Superseded by a newer turn.',
		       resolved_at = $1,
		       resume_state = CASE WHEN resume_state = 'armed' THEN 'superseded' ELSE resume_state END
		 WHERE conversation_id = $2
		   AND tool_name = 'suggest_advanced_model'
		   AND status = 'pending'`,
		nowUnix, convID,
	)
	return err
}
