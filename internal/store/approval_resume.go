package store

// Resume after approval (docs/RESUME-AFTER-APPROVAL.md, ADR-0083): the durable
// half. A card staged for a tool the bundle opted in
// (agent_policy.critical_tool_resume) is armed at staging. When the cards of a
// conversation are settled, the chat server calls ClaimApprovalResume, which
// in ONE transaction claims every armed, settled card and enqueues the single
// turn that continues the task as an input-queue row of mode 'resume' — or
// records, in the conversation, why it did not. Because the claim and the
// enqueue commit together, a resume is started at most once per card whatever
// the process does in between, and the queue row then carries it with the
// queue's own durability and Stop semantics. DropApprovalResumesAtBoot is the
// restart rule: a resume that was due but had not started is dropped with a
// note, never run unattended after a restart.

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

// approvals.resume_state values (migration 072).
const (
	ApprovalResumeArmed      = "armed"
	ApprovalResumeClaimed    = "claimed"
	ApprovalResumeSkipped    = "skipped"
	ApprovalResumeSuperseded = "superseded"
	ApprovalResumeDropped    = "dropped"
)

// Outcomes named in a resume turn's input.
const (
	ApprovalResumeOutcomeApproved = "approved"
	ApprovalResumeOutcomeDeclined = "declined"
	ApprovalResumeOutcomeTimedOut = "timed_out"
)

// Why ClaimApprovalResume claimed cards but enqueued no turn.
const (
	ApprovalResumeSkipRateLimited      = "rate_limited"
	ApprovalResumeSkipQueueFull        = "queue_full"
	ApprovalResumeSkipConversationGone = "conversation_gone"
)

// Notice kinds written into the conversation (agent.EntryTypeNotice).
const (
	NoticeApprovalResumeSkipped = "approval_resume_skipped"
	NoticeApprovalResumeDropped = "approval_resume_dropped"
)

// approvalTimedOutPrefix is the stable prefix of every timeout result text the
// chat server writes ("Approval timed out — auto-denied. …"); the web keys its
// Ask again affordance on the same prefix.
const approvalTimedOutPrefix = "Approval timed out"

// ArmApprovalResume marks a still-pending approval as one whose settlement
// starts a resume turn. false = no pending row matched (already resolved).
func (s *Store) ArmApprovalResume(ctx context.Context, userEmail, approvalID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals SET resume_state = 'armed'
		  WHERE id = $1 AND user_email = $2 AND status = 'pending' AND resume_state = ''`,
		approvalID, userEmail)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ApprovalResumeItem is one settled card a resume turn reports.
type ApprovalResumeItem struct {
	ID       string
	ToolName string
	// Outcome is ApprovalResumeOutcomeApproved, …Declined or …TimedOut.
	Outcome string
	// IsErr: approved, and the call reported an error.
	IsErr bool
}

// ApprovalResumeRequest is one ClaimApprovalResume call.
type ApprovalResumeRequest struct {
	ConversationID string
	// MaxPerHour caps the resume rows the conversation may accumulate in a
	// rolling hour; MaxPending is the queue's per-conversation depth cap.
	MaxPerHour int
	MaxPending int
	// IgnoreOutstanding, when set, excludes an approval from the "still
	// pending or executing" check: the chat server passes the cards whose
	// executing sentinel stands only because its own outcome write failed, so
	// one failed write cannot hold every later resume of the conversation.
	IgnoreOutstanding func(approvalID string) bool
}

// ApprovalResumeResult says what ClaimApprovalResume did.
type ApprovalResumeResult struct {
	// Outstanding names the approvals still pending or executing. When it is
	// non-empty nothing was claimed: the caller waits for them to settle.
	Outstanding []string
	// Claimed are the cards this call settled into the resume (oldest first).
	Claimed []ApprovalResumeItem
	// Input is the enqueued resume row, nil when none was enqueued.
	Input *InputQueueRow
	// Skipped is why Claimed produced no turn ("" when Input is set or
	// nothing was claimed).
	Skipped string
	// RecentResumes is how many resume rows the conversation already had in
	// the rolling hour (the cap's count).
	RecentResumes int
}

// ClaimApprovalResume claims every armed, settled approval of the
// conversation and enqueues one resume input for them, in one transaction:
//
//   - the conversation row is locked first (the same lock the queue's
//     position allocation takes), so concurrent callers serialize; a deleted
//     conversation gets no turn, and its settled armed cards are dropped;
//   - if any other approval of the conversation is still pending (preview
//     cards excepted: they are display-only and never expire) or still
//     executing, nothing changes and Outstanding names them;
//   - otherwise the armed settled cards are claimed. Over the hourly cap, or
//     with the queue full, they are marked skipped and a notice is appended to
//     the conversation instead; else one 'resume' queue row is inserted, keyed
//     by the first claimed card's id, so even a replay of this call could not
//     insert a second one.
func (s *Store) ClaimApprovalResume(ctx context.Context, req ApprovalResumeRequest) (ApprovalResumeResult, error) {
	var out ApprovalResumeResult
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()

	var owner string
	err = tx.QueryRowContext(ctx,
		`SELECT user_email FROM conversations WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`,
		req.ConversationID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		// Deleted (or never existed): never resume it. The settled armed cards
		// are dropped so nothing revisits them; a still-pending one stays armed
		// and is dropped the same way once it settles.
		if _, err := tx.ExecContext(ctx,
			`UPDATE approvals SET resume_state = 'dropped'
			  WHERE conversation_id = $1 AND resume_state = 'armed' AND status <> 'pending'`,
			req.ConversationID); err != nil {
			return out, err
		}
		out.Skipped = ApprovalResumeSkipConversationGone
		return out, tx.Commit()
	}
	if err != nil {
		return out, err
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM approvals
		  WHERE conversation_id = $1
		    AND ((status = 'pending' AND tool_name <> 'preview_email')
		      OR (status = 'approved' AND is_err IS NULL AND result_text = $2))
		  ORDER BY created_at, id`,
		req.ConversationID, ApprovalExecutingSentinel)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return out, err
		}
		if req.IgnoreOutstanding != nil && req.IgnoreOutstanding(id) {
			continue
		}
		out.Outstanding = append(out.Outstanding, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return out, err
	}
	_ = rows.Close()
	if len(out.Outstanding) > 0 {
		return out, nil // rolled back: nothing changed
	}

	// An approved card whose sentinel still stands (its outcome write failed)
	// is not settled: it stays armed, and boot recovery drops it with a note
	// once RecoverStrandedApprovals has recorded it as unknown.
	rows, err = tx.QueryContext(ctx,
		`UPDATE approvals SET resume_state = 'claimed'
		  WHERE conversation_id = $1 AND resume_state = 'armed' AND status <> 'pending'
		    AND NOT (status = 'approved' AND is_err IS NULL AND result_text = $2)
		  RETURNING id, tool_name, status, COALESCE(result_text, ''), is_err, COALESCE(resolved_at, 0), created_at`,
		req.ConversationID, ApprovalExecutingSentinel)
	if err != nil {
		return out, err
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
			return out, err
		}
		c.item.Outcome = approvalResumeOutcome(status, rt)
		c.item.IsErr = status == "approved" && isErr.Valid && isErr.Bool
		claimed = append(claimed, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return out, err
	}
	_ = rows.Close()
	if len(claimed) == 0 {
		return out, nil // nothing armed was settled: no-op
	}
	sort.Slice(claimed, func(i, j int) bool {
		if claimed[i].resolvedAt != claimed[j].resolvedAt {
			return claimed[i].resolvedAt < claimed[j].resolvedAt
		}
		if claimed[i].createdAt != claimed[j].createdAt {
			return claimed[i].createdAt < claimed[j].createdAt
		}
		return claimed[i].item.ID < claimed[j].item.ID
	})
	ids := make([]string, len(claimed))
	for i, c := range claimed {
		out.Claimed = append(out.Claimed, c.item)
		ids[i] = c.item.ID
	}

	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM chat_input_queue
		  WHERE conversation_id = $1 AND mode = 'resume' AND created_at > $2`,
		req.ConversationID, now-3600).Scan(&out.RecentResumes); err != nil {
		return out, err
	}
	var pending int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM chat_input_queue WHERE conversation_id = $1 AND state = 'queued'`,
		req.ConversationID).Scan(&pending); err != nil {
		return out, err
	}
	switch {
	case req.MaxPerHour > 0 && out.RecentResumes >= req.MaxPerHour:
		out.Skipped = ApprovalResumeSkipRateLimited
	case req.MaxPending > 0 && pending >= req.MaxPending:
		out.Skipped = ApprovalResumeSkipQueueFull
	}
	if out.Skipped != "" {
		if err := markApprovalResumeTx(ctx, tx, ids, ApprovalResumeSkipped); err != nil {
			return out, err
		}
		note := ApprovalResumeSkippedNotice(out.Skipped, req.MaxPerHour)
		if err := s.appendNoticeTx(ctx, tx, req.ConversationID, NoticeApprovalResumeSkipped, note); err != nil {
			return out, err
		}
		return out, tx.Commit()
	}

	row, _, err := s.insertInputTx(ctx, tx, InputQueueRow{
		ID: uuid.NewString(), ConversationID: req.ConversationID, UserEmail: owner,
		ClientInputID: "approval-resume:" + ids[0],
		Message:       ApprovalResumeInputText(out.Claimed),
		Attachments:   "[]", Mode: InputModeResume, State: InputStateQueued,
	})
	if err != nil {
		return out, err
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	out.Input = &row
	return out, nil
}

// approvalResumeOutcome names a settled card's outcome from its row.
func approvalResumeOutcome(status, resultText string) string {
	switch {
	case status == "approved":
		return ApprovalResumeOutcomeApproved
	case strings.HasPrefix(resultText, approvalTimedOutPrefix):
		return ApprovalResumeOutcomeTimedOut
	default:
		return ApprovalResumeOutcomeDeclined
	}
}

func markApprovalResumeTx(ctx context.Context, tx *sql.Tx, ids []string, state string) error {
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE approvals SET resume_state = $1 WHERE id = $2`, state, id); err != nil {
			return err
		}
	}
	return nil
}

// appendNoticeTx writes one UI-only notice entry into the conversation.
func (s *Store) appendNoticeTx(ctx context.Context, tx *sql.Tx, convID, kind, text string) error {
	payload, err := json.Marshal(agent.NoticeContent{Kind: kind, Text: text})
	if err != nil {
		return err
	}
	_, err = s.appendHistoryTx(ctx, tx, convID, []agent.HistoryEntry{{Role: "system", Type: agent.EntryTypeNotice, Content: payload}})
	return err
}

// ApprovalResumeInputText is the synthetic, clearly labelled input of a
// resume turn. It names each settled card and its outcome, points the model at
// the results already in the conversation, and tells it not to retry what a
// person declined or let time out.
func ApprovalResumeInputText(items []ApprovalResumeItem) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		p := fmt.Sprintf("%s approval_id=%s outcome=%s", it.ToolName, it.ID, it.Outcome)
		if it.IsErr {
			p += " result=error"
		}
		parts = append(parts, p)
	}
	head, noun, pron := "[Approval resolved]", "result is", "it"
	if len(items) > 1 {
		head, noun, pron = "[Approvals resolved]", "results are", "them"
	}
	return fmt.Sprintf("%s %s. The %s in the conversation; continue the task: verify %s and carry on, or stop if nothing remains. Do not retry a declined or timed-out action unless the user asks. (Written by fleet, not the user.)",
		head, strings.Join(parts, "; "), noun, pron)
}

// ApprovalResumeSkippedNotice is the conversation note for a resume that was
// due but not started.
func ApprovalResumeSkippedNotice(reason string, maxPerHour int) string {
	switch reason {
	case ApprovalResumeSkipRateLimited:
		return fmt.Sprintf("Automatic continue skipped: this chat already continued on its own %d times in the last hour after approvals. Send a message to continue.", maxPerHour)
	case ApprovalResumeSkipQueueFull:
		return "Automatic continue skipped: this chat's message queue is full. Send a message once it drains to continue."
	default:
		return "Automatic continue skipped. Send a message to continue."
	}
}

// AppendApprovalResumeDroppedNotice writes the restart note into one
// conversation: the chat server's launch guard calls it for a 'resume' row the
// boot sweep missed.
func (s *Store) AppendApprovalResumeDroppedNotice(ctx context.Context, convID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.appendNoticeTx(ctx, tx, convID, NoticeApprovalResumeDropped, approvalResumeDroppedNotice); err != nil {
		return err
	}
	return tx.Commit()
}

// approvalResumeDroppedNotice is the conversation note boot recovery writes.
const approvalResumeDroppedNotice = "Automatic continue was not started because fleet restarted before it could run. Nothing was re-run. Send a message to continue."

// DropApprovalResumesAtBoot is the restart rule for resume after approval:
// a resume that was due but had not started when the process stopped is
// dropped, never started unattended by a restart (the same rule the input
// queue follows: boot recovery never auto-drains). In one transaction it marks
// every armed, settled card dropped, cancels every 'resume' queue row still
// queued (including one RecoverInputQueue just returned to the queue because
// its turn died before committing its input), and appends one notice to each
// affected conversation. Run it after RecoverStrandedApprovals and
// RecoverInputQueue. A card still pending stays armed: it settles in the new
// process and resumes normally. Returns the conversations noted.
func (s *Store) DropApprovalResumesAtBoot(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	convs := map[string]bool{}
	collect := func(q string, args ...any) error {
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			convs[id] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		return rows.Close()
	}
	if err := collect(
		`UPDATE approvals SET resume_state = 'dropped'
		  WHERE resume_state = 'armed' AND status <> 'pending'
		    AND NOT (status = 'approved' AND is_err IS NULL AND result_text = $1)
		  RETURNING conversation_id`, ApprovalExecutingSentinel); err != nil {
		return 0, err
	}
	if err := collect(
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $1
		  WHERE mode = 'resume' AND state = 'queued'
		  RETURNING conversation_id`, time.Now().Unix()); err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(convs))
	for id := range convs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := s.appendNoticeTx(ctx, tx, id, NoticeApprovalResumeDropped, approvalResumeDroppedNotice); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit()
}
