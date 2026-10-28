package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/cron"
)

// ChatSchedule status values (mirrors migration 086 CHECK constraint).
const (
	ChatScheduleStatusActive    = "active"
	ChatScheduleStatusPaused    = "paused"
	ChatScheduleStatusCompleted = "completed"
	ChatScheduleStatusCancelled = "cancelled"
)

// ChatScheduleKind values derived from CronExpr vs DurationSeconds.
const (
	ChatScheduleKindPatrol = "patrol"
	ChatScheduleKindTimer  = "timer"
)

// ChatPatrolInputKeyPrefix prefixes client_input_id on every chat_input_queue
// row enqueued by a chat schedule (patrol or timer), so launchQueuedTurn marks
// the resulting turn with agent.InputKindChatPatrol.
const ChatPatrolInputKeyPrefix = "chat-patrol:"

// ChatPatrolInputPrefix is the human-readable header prefix on the turn text
// generated for a scheduled patrol or timer turn.
const ChatPatrolInputPrefix = "[Scheduled "

// MaxActiveSchedulesPerConversation bounds how many active or paused schedules
// a single conversation can hold at once.
const MaxActiveSchedulesPerConversation = 5

// Validation bounds for recurring patrols and one-shot timers. Vars so tests
// can tighten or relax them without sleeping.
var (
	MinChatPatrolCronInterval   = 5 * time.Minute
	MinChatTimerDurationSeconds = 10
	MaxChatTimerDurationSeconds = 30 * 24 * 3600
)

// Sentinel errors returned by chat schedule store operations.
var (
	ErrChatScheduleNotFound = errors.New("chat schedule not found")
	ErrInvalidChatSchedule  = errors.New("invalid chat schedule")
)

// ChatSchedule is a recurring patrol or one-shot timer bound to a conversation.
type ChatSchedule struct {
	ID              string `json:"id"`
	ConversationID  string `json:"conversation_id"`
	UserEmail       string `json:"user_email"`
	Title           string `json:"title"`
	Prompt          string `json:"prompt"`
	Kind            string `json:"kind"`
	CronExpr        string `json:"cron,omitempty"`
	Timezone        string `json:"timezone"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	Status          string `json:"status"`
	NextRunAt       int64  `json:"next_run_at"`
	LastRunAt       *int64 `json:"last_run_at,omitempty"`
	RunCount        int    `json:"run_count"`
	MaxRuns         int    `json:"max_runs,omitempty"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

// ChatScheduleInput is the creation payload for CreateChatSchedule.
type ChatScheduleInput struct {
	ConversationID  string
	UserEmail       string
	Title           string
	Prompt          string
	CronExpr        string
	Timezone        string
	DurationSeconds int
	MaxRuns         int
	// AllowMultiple, when false (default), updates an existing active/paused
	// recurring patrol in the same conversation instead of creating a second
	// overlapping recurring patrol.
	AllowMultiple bool
}

// ChatSchedulePatch describes a partial update to an existing ChatSchedule.
type ChatSchedulePatch struct {
	Title           *string
	Prompt          *string
	CronExpr        *string
	Timezone        *string
	DurationSeconds *int
	MaxRuns         *int
	Status          *string
}

// ClaimedChatSchedule pairs a schedule that just fired with the input-queue
// row enqueued for its turn.
type ClaimedChatSchedule struct {
	Schedule ChatSchedule
	Input    InputQueueRow
}

const chatScheduleSelectCols = `
	id, conversation_id, user_email, title, prompt, cron_expr, timezone,
	duration_seconds, status, next_run_at, last_run_at, run_count, max_runs,
	created_at, updated_at
`

func scanChatSchedule(row interface{ Scan(dest ...any) error }) (ChatSchedule, error) {
	var (
		s         ChatSchedule
		lastRunAt sql.NullInt64
	)
	err := row.Scan(
		&s.ID, &s.ConversationID, &s.UserEmail, &s.Title, &s.Prompt,
		&s.CronExpr, &s.Timezone, &s.DurationSeconds, &s.Status,
		&s.NextRunAt, &lastRunAt, &s.RunCount, &s.MaxRuns,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if err != nil {
		return ChatSchedule{}, err
	}
	if lastRunAt.Valid {
		v := lastRunAt.Int64
		s.LastRunAt = &v
	}
	if s.CronExpr != "" {
		s.Kind = ChatScheduleKindPatrol
	} else {
		s.Kind = ChatScheduleKindTimer
	}
	return s, nil
}

// normalizeTimezone validates tz (defaulting empty to "UTC") and returns the
// canonical location name and *time.Location.
func normalizeTimezone(tz string) (string, *time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return "UTC", time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return "", nil, fmt.Errorf("%w: invalid timezone %q: %s", ErrInvalidChatSchedule, tz, err.Error())
	}
	return tz, loc, nil
}

// cleanCronExpr strips any inline CRON_TZ=/TZ= prefix if the caller passed one
// and returns the bare cron spec and extracted timezone (if any).
func cleanCronExpr(spec, tz string) (string, string, *time.Location, error) {
	spec = strings.TrimSpace(spec)
	if strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=") {
		i := strings.Index(spec, " ")
		eq := strings.Index(spec, "=")
		if i < 0 || eq < 0 || eq+1 >= i {
			return "", "", nil, fmt.Errorf("%w: invalid CRON_TZ prefix in %q", ErrInvalidChatSchedule, spec)
		}
		if strings.TrimSpace(tz) == "" {
			tz = spec[eq+1 : i]
		}
		spec = strings.TrimSpace(spec[i+1:])
	}
	normTZ, loc, err := normalizeTimezone(tz)
	if err != nil {
		return "", "", nil, err
	}
	return spec, normTZ, loc, nil
}

// validateAndComputeNextCron parses a 5-field cron expression (or @descriptor),
// enforces MinChatPatrolCronInterval between consecutive firings, and returns
// the next firing Unix timestamp after now.
func validateAndComputeNextCron(cronExpr, tz string, now time.Time) (string, string, int64, error) {
	cleanSpec, normTZ, loc, err := cleanCronExpr(cronExpr, tz)
	if err != nil {
		return "", "", 0, err
	}
	if cleanSpec == "" {
		return "", "", 0, fmt.Errorf("%w: cron expression cannot be empty", ErrInvalidChatSchedule)
	}
	sched, err := cron.ParseStandard(cleanSpec)
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: invalid cron expression %q: %s", ErrInvalidChatSchedule, cleanSpec, err.Error())
	}
	base := now.In(loc)
	next1 := sched.Next(base)
	if next1.IsZero() {
		return "", "", 0, fmt.Errorf("%w: cron expression %q never fires", ErrInvalidChatSchedule, cleanSpec)
	}
	if MinChatPatrolCronInterval > 0 {
		cur := next1
		for i := 0; i < 48; i++ {
			nxt := sched.Next(cur)
			if nxt.IsZero() {
				break
			}
			if nxt.Sub(cur) < MinChatPatrolCronInterval {
				return "", "", 0, fmt.Errorf("%w: patrol cadence must be at least %s between runs", ErrInvalidChatSchedule, MinChatPatrolCronInterval)
			}
			cur = nxt
		}
	}
	return cleanSpec, normTZ, next1.Unix(), nil
}

// CreateChatSchedule creates a recurring patrol or one-shot timer on a
// conversation owned by in.UserEmail. When in.CronExpr is set and
// !in.AllowMultiple, an existing active or paused recurring patrol on the same
// conversation is updated in place so changing a patrol's cadence or prompt
// never leaves duplicate patrols running.
func (s *Store) CreateChatSchedule(ctx context.Context, in ChatScheduleInput) (*ChatSchedule, error) {
	now := time.Now()
	nowUnix := now.Unix()

	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return nil, fmt.Errorf("%w: prompt is required", ErrInvalidChatSchedule)
	}
	cronSpec := strings.TrimSpace(in.CronExpr)
	if (cronSpec == "" && in.DurationSeconds <= 0) || (cronSpec != "" && in.DurationSeconds > 0) {
		return nil, fmt.Errorf("%w: specify either cron (for a recurring patrol) or duration_seconds (for a one-shot timer), not both", ErrInvalidChatSchedule)
	}

	var (
		normTZ    string
		nextRunAt int64
		maxRuns   = in.MaxRuns
		err       error
	)
	if cronSpec != "" {
		cronSpec, normTZ, nextRunAt, err = validateAndComputeNextCron(cronSpec, in.Timezone, now)
		if err != nil {
			return nil, err
		}
		if maxRuns < 0 {
			maxRuns = 0
		}
	} else {
		if in.DurationSeconds < MinChatTimerDurationSeconds || in.DurationSeconds > MaxChatTimerDurationSeconds {
			return nil, fmt.Errorf("%w: duration_seconds must be between %d and %d", ErrInvalidChatSchedule, MinChatTimerDurationSeconds, MaxChatTimerDurationSeconds)
		}
		normTZ, _, err = normalizeTimezone(in.Timezone)
		if err != nil {
			return nil, err
		}
		nextRunAt = nowUnix + int64(in.DurationSeconds)
		maxRuns = 1
	}

	title := strings.TrimSpace(in.Title)
	if title == "" {
		if cronSpec != "" {
			title = "Chat Patrol"
		} else {
			title = "Scheduled Timer"
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var owner string
	err = tx.QueryRowContext(ctx,
		`SELECT user_email FROM conversations WHERE id = $1 AND user_email = $2 AND deleted_at IS NULL FOR UPDATE`,
		in.ConversationID, in.UserEmail,
	).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, err
	}

	// If creating a recurring patrol and AllowMultiple is false, update the
	// conversation's existing active/paused recurring patrol in place.
	if cronSpec != "" && !in.AllowMultiple {
		var existingID string
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM chat_schedules
			  WHERE conversation_id = $1 AND user_email = $2
			    AND status IN ('active', 'paused') AND cron_expr <> ''
			  ORDER BY created_at DESC, id DESC LIMIT 1
			  FOR UPDATE`,
			in.ConversationID, in.UserEmail,
		).Scan(&existingID)
		if err == nil && existingID != "" {
			sched, err := scanChatSchedule(tx.QueryRowContext(ctx,
				`UPDATE chat_schedules
				    SET title = $1, prompt = $2, cron_expr = $3, timezone = $4,
				        duration_seconds = 0, status = 'active', next_run_at = $5,
				        max_runs = $6, updated_at = $7
				  WHERE id = $8
				  RETURNING `+chatScheduleSelectCols,
				title, prompt, cronSpec, normTZ, nextRunAt, maxRuns, nowUnix, existingID,
			))
			if err != nil {
				return nil, err
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return &sched, nil
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}

	var activeCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM chat_schedules
		  WHERE conversation_id = $1 AND status IN ('active', 'paused')`,
		in.ConversationID,
	).Scan(&activeCount); err != nil {
		return nil, err
	}
	if activeCount >= MaxActiveSchedulesPerConversation {
		return nil, fmt.Errorf("%w: conversation already has %d active/paused schedules (max %d)",
			ErrInvalidChatSchedule, activeCount, MaxActiveSchedulesPerConversation)
	}

	id := uuid.NewString()
	sched, err := scanChatSchedule(tx.QueryRowContext(ctx,
		`INSERT INTO chat_schedules (
			id, conversation_id, user_email, title, prompt, cron_expr, timezone,
			duration_seconds, status, next_run_at, run_count, max_runs,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'active', $9, 0, $10, $11, $11)
		RETURNING `+chatScheduleSelectCols,
		id, in.ConversationID, in.UserEmail, title, prompt, cronSpec, normTZ,
		in.DurationSeconds, nextRunAt, maxRuns, nowUnix,
	))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &sched, nil
}

// GetChatSchedule returns a single schedule scoped by owner and conversation.
func (s *Store) GetChatSchedule(ctx context.Context, userEmail, convID, scheduleID string) (*ChatSchedule, error) {
	sched, err := scanChatSchedule(s.db.QueryRowContext(ctx,
		`SELECT `+chatScheduleSelectCols+`
		   FROM chat_schedules
		  WHERE id = $1 AND conversation_id = $2 AND user_email = $3`,
		scheduleID, convID, userEmail,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sched, nil
}

// ListChatSchedules lists schedules for a conversation owned by userEmail.
// When includeTerminal is false, only active and paused schedules are returned.
func (s *Store) ListChatSchedules(ctx context.Context, userEmail, convID string, includeTerminal bool) ([]ChatSchedule, error) {
	if s == nil || s.db == nil {
		return []ChatSchedule{}, nil
	}
	query := `SELECT ` + chatScheduleSelectCols + `
		FROM chat_schedules
		WHERE conversation_id = $1 AND user_email = $2`
	if !includeTerminal {
		query += ` AND status IN ('active', 'paused')`
	}
	query += ` ORDER BY created_at ASC, id ASC`

	rows, err := s.db.QueryContext(ctx, query, convID, userEmail)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ChatSchedule{}
	for rows.Next() {
		item, err := scanChatSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// UpdateChatSchedule applies a partial update (pause, resume, or cadence/prompt
// edit) to an existing schedule owned by userEmail in convID.
func (s *Store) UpdateChatSchedule(ctx context.Context, userEmail, convID, scheduleID string, patch ChatSchedulePatch) (*ChatSchedule, error) {
	now := time.Now()
	nowUnix := now.Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	cur, err := scanChatSchedule(tx.QueryRowContext(ctx,
		`SELECT `+chatScheduleSelectCols+`
		   FROM chat_schedules
		  WHERE id = $1 AND conversation_id = $2 AND user_email = $3
		  FOR UPDATE`,
		scheduleID, convID, userEmail,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChatScheduleNotFound
	}
	if err != nil {
		return nil, err
	}

	recomputeNext := false
	if patch.Title != nil {
		if t := strings.TrimSpace(*patch.Title); t != "" {
			cur.Title = t
		}
	}
	if patch.Prompt != nil {
		p := strings.TrimSpace(*patch.Prompt)
		if p == "" {
			return nil, fmt.Errorf("%w: prompt cannot be empty", ErrInvalidChatSchedule)
		}
		cur.Prompt = p
	}
	if patch.Timezone != nil {
		normTZ, _, err := normalizeTimezone(*patch.Timezone)
		if err != nil {
			return nil, err
		}
		cur.Timezone = normTZ
		recomputeNext = true
	}
	if patch.CronExpr != nil {
		spec := strings.TrimSpace(*patch.CronExpr)
		if spec != "" {
			cur.CronExpr = spec
			cur.DurationSeconds = 0
			recomputeNext = true
		}
	}
	if patch.DurationSeconds != nil && *patch.DurationSeconds > 0 {
		if *patch.DurationSeconds < MinChatTimerDurationSeconds || *patch.DurationSeconds > MaxChatTimerDurationSeconds {
			return nil, fmt.Errorf("%w: duration_seconds must be between %d and %d", ErrInvalidChatSchedule, MinChatTimerDurationSeconds, MaxChatTimerDurationSeconds)
		}
		cur.DurationSeconds = *patch.DurationSeconds
		cur.CronExpr = ""
		cur.MaxRuns = 1
		recomputeNext = true
	}
	if patch.MaxRuns != nil && cur.CronExpr != "" {
		if *patch.MaxRuns >= 0 {
			cur.MaxRuns = *patch.MaxRuns
		}
	}
	if patch.Status != nil {
		st := strings.TrimSpace(*patch.Status)
		switch st {
		case ChatScheduleStatusActive:
			if cur.Status != ChatScheduleStatusActive {
				recomputeNext = true
			}
			cur.Status = ChatScheduleStatusActive
		case ChatScheduleStatusPaused, ChatScheduleStatusCancelled:
			cur.Status = st
		default:
			return nil, fmt.Errorf("%w: unsupported status %q", ErrInvalidChatSchedule, st)
		}
	}

	if recomputeNext && cur.Status == ChatScheduleStatusActive {
		if cur.CronExpr != "" {
			cleanSpec, normTZ, nextAt, err := validateAndComputeNextCron(cur.CronExpr, cur.Timezone, now)
			if err != nil {
				return nil, err
			}
			cur.CronExpr = cleanSpec
			cur.Timezone = normTZ
			cur.NextRunAt = nextAt
		} else if cur.DurationSeconds > 0 {
			cur.NextRunAt = nowUnix + int64(cur.DurationSeconds)
		}
	}

	updated, err := scanChatSchedule(tx.QueryRowContext(ctx,
		`UPDATE chat_schedules
		    SET title = $1, prompt = $2, cron_expr = $3, timezone = $4,
		        duration_seconds = $5, status = $6, next_run_at = $7,
		        max_runs = $8, updated_at = $9
		  WHERE id = $10
		  RETURNING `+chatScheduleSelectCols,
		cur.Title, cur.Prompt, cur.CronExpr, cur.Timezone,
		cur.DurationSeconds, cur.Status, cur.NextRunAt,
		cur.MaxRuns, nowUnix, cur.ID,
	))
	if err != nil {
		return nil, err
	}

	if updated.Status == ChatScheduleStatusCancelled || updated.Status == ChatScheduleStatusPaused {
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_input_queue
			    SET state = 'cancelled', updated_at = $1
			  WHERE conversation_id = $2 AND state = 'queued' AND client_input_id LIKE $3`,
			nowUnix, convID, ChatPatrolInputKeyPrefix+scheduleID+":%",
		); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &updated, nil
}

// CancelChatSchedule marks a schedule cancelled and cancels any still-queued
// patrol input rows it enqueued.
func (s *Store) CancelChatSchedule(ctx context.Context, userEmail, convID, scheduleID string) (*ChatSchedule, error) {
	status := ChatScheduleStatusCancelled
	return s.UpdateChatSchedule(ctx, userEmail, convID, scheduleID, ChatSchedulePatch{Status: &status})
}

// TriggerChatScheduleNow immediately enqueues a turn for scheduleID (used by
// the UI's "Run now" button and manual test triggers).
func (s *Store) TriggerChatScheduleNow(ctx context.Context, userEmail, convID, scheduleID string, maxPending int) (*ClaimedChatSchedule, error) {
	now := time.Now()
	nowUnix := now.Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var owner string
	err = tx.QueryRowContext(ctx,
		`SELECT user_email FROM conversations WHERE id = $1 AND user_email = $2 AND deleted_at IS NULL FOR UPDATE`,
		convID, userEmail,
	).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, err
	}

	sched, err := scanChatSchedule(tx.QueryRowContext(ctx,
		`SELECT `+chatScheduleSelectCols+`
		   FROM chat_schedules
		  WHERE id = $1 AND conversation_id = $2 AND user_email = $3
		  FOR UPDATE`,
		scheduleID, convID, userEmail,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChatScheduleNotFound
	}
	if err != nil {
		return nil, err
	}

	// Coalesce repeated Run-now clicks when a patrol turn is already queued or
	// running in this conversation, so clicking Run-now while busy does not stack
	// duplicate patrol turns or burn run_count against MaxRuns.
	existingInput, err := scanInputRow(tx.QueryRowContext(ctx,
		inputQueueSelect+`
		  WHERE conversation_id = $1
		    AND state IN ('queued', 'running')
		    AND (mode = 'patrol' OR client_input_id LIKE $2)
		  ORDER BY position ASC LIMIT 1`,
		convID, ChatPatrolInputKeyPrefix+"%",
	))
	if err == nil {
		if cerr := tx.Commit(); cerr != nil {
			return nil, cerr
		}
		return &ClaimedChatSchedule{Schedule: sched, Input: existingInput}, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	if maxPending > 0 {
		var pending int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM chat_input_queue WHERE conversation_id = $1 AND state = 'queued'`,
			convID,
		).Scan(&pending); err != nil {
			return nil, err
		}
		if pending >= maxPending {
			return nil, fmt.Errorf("conversation input queue is full (%d queued)", pending)
		}
	}

	newRunCount := sched.RunCount + 1
	nextStatus := sched.Status
	nextRunAt := sched.NextRunAt
	if sched.CronExpr != "" {
		if sched.MaxRuns > 0 && newRunCount >= sched.MaxRuns {
			nextStatus = ChatScheduleStatusCompleted
		} else if _, _, nextAt, err := validateAndComputeNextCron(sched.CronExpr, sched.Timezone, now); err == nil {
			nextRunAt = nextAt
		}
	} else {
		nextStatus = ChatScheduleStatusCompleted
	}

	updated, err := scanChatSchedule(tx.QueryRowContext(ctx,
		`UPDATE chat_schedules
		    SET run_count = $1, last_run_at = $2, next_run_at = $3,
		        status = $4, updated_at = $2
		  WHERE id = $5
		  RETURNING `+chatScheduleSelectCols,
		newRunCount, nowUnix, nextRunAt, nextStatus, sched.ID,
	))
	if err != nil {
		return nil, err
	}

	clientInputID := fmt.Sprintf("%s%s:%d:%d", ChatPatrolInputKeyPrefix, sched.ID, newRunCount, nowUnix)
	row, _, err := s.insertInputTx(ctx, tx, InputQueueRow{
		ID:             uuid.NewString(),
		ConversationID: convID,
		UserEmail:      owner,
		ClientInputID:  clientInputID,
		Message:        ChatPatrolInputText(updated, newRunCount),
		Attachments:    "[]",
		Mode:           InputModePatrol,
		State:          InputStateQueued,
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ClaimedChatSchedule{Schedule: updated, Input: row}, nil
}

// ClaimDueChatSchedules claims active schedules whose next_run_at <= now and
// enqueues their patrol/timer turns into chat_input_queue.
//
// Coalescing and backpressure rules:
//   - If the owning conversation was deleted, the schedule is marked cancelled.
//   - If the conversation already has a queued or running patrol input in
//     chat_input_queue, or has a pending non-preview approval waiting on the
//     user, or its input queue is at maxPending, a recurring cron schedule
//     advances next_run_at to the next future tick without inserting a
//     duplicate queue row (so patrols never pile up while a previous patrol or
//     approval is still in flight).
func (s *Store) ClaimDueChatSchedules(ctx context.Context, now time.Time, maxPending int) ([]ClaimedChatSchedule, error) {
	nowUnix := now.Unix()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, conversation_id
		   FROM chat_schedules
		  WHERE status = 'active' AND next_run_at <= $1
		  ORDER BY next_run_at ASC, id ASC
		  LIMIT 25`,
		nowUnix,
	)
	if err != nil {
		return nil, err
	}
	type dueRef struct {
		id     string
		convID string
	}
	var due []dueRef
	for rows.Next() {
		var d dueRef
		if err := rows.Scan(&d.id, &d.convID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		due = append(due, d)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	var claimed []ClaimedChatSchedule
	for _, d := range due {
		item, err := s.claimOneChatSchedule(ctx, d.id, d.convID, now, maxPending)
		if err != nil {
			return claimed, err
		}
		if item != nil {
			claimed = append(claimed, *item)
		}
	}
	return claimed, nil
}

func (s *Store) claimOneChatSchedule(ctx context.Context, scheduleID, convID string, now time.Time, maxPending int) (*ClaimedChatSchedule, error) {
	nowUnix := now.Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the conversation first (same lock order as insertInputTx and
	// ClaimApprovalResume) so concurrent queue operations never deadlock.
	var owner string
	err = tx.QueryRowContext(ctx,
		`SELECT user_email FROM conversations WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`,
		convID,
	).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		_, _ = tx.ExecContext(ctx,
			`UPDATE chat_schedules SET status = 'cancelled', updated_at = $1 WHERE id = $2`,
			nowUnix, scheduleID,
		)
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}

	sched, err := scanChatSchedule(tx.QueryRowContext(ctx,
		`SELECT `+chatScheduleSelectCols+`
		   FROM chat_schedules
		  WHERE id = $1 AND status = 'active' AND next_run_at <= $2
		  FOR UPDATE`,
		scheduleID, nowUnix,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Check whether an earlier patrol turn is still queued/running, an approval
	// card is awaiting human action, or the queue is full.
	var hasInFlightPatrol bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM chat_input_queue
			 WHERE conversation_id = $1
			   AND state IN ('queued', 'running')
			   AND (mode = 'patrol' OR client_input_id LIKE $2)
		)`,
		convID, ChatPatrolInputKeyPrefix+"%",
	).Scan(&hasInFlightPatrol); err != nil {
		return nil, err
	}

	var hasPendingApproval bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM approvals
			 WHERE conversation_id = $1
			   AND status = 'pending'
			   AND tool_name <> 'preview_email'
		)`,
		convID,
	).Scan(&hasPendingApproval); err != nil {
		return nil, err
	}

	var pendingCount int
	if maxPending > 0 {
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM chat_input_queue WHERE conversation_id = $1 AND state = 'queued'`,
			convID,
		).Scan(&pendingCount); err != nil {
			return nil, err
		}
	}

	busy := hasInFlightPatrol || hasPendingApproval || (maxPending > 0 && pendingCount >= maxPending)
	if busy {
		// Advance next_run_at so a blocked one-shot timer or recurring patrol
		// never sits at the head of ORDER BY next_run_at ASC and starves later
		// due schedules in other conversations.
		nextAt := nowUnix + 30
		if sched.CronExpr != "" {
			if _, _, cronNext, err := validateAndComputeNextCron(sched.CronExpr, sched.Timezone, now); err == nil {
				nextAt = cronNext
			}
		}
		_, _ = tx.ExecContext(ctx,
			`UPDATE chat_schedules SET next_run_at = $1, updated_at = $2 WHERE id = $3`,
			nextAt, nowUnix, sched.ID,
		)
		_ = tx.Commit()
		return nil, nil
	}

	newRunCount := sched.RunCount + 1
	nextStatus := ChatScheduleStatusActive
	nextRunAt := sched.NextRunAt
	if sched.CronExpr != "" {
		if sched.MaxRuns > 0 && newRunCount >= sched.MaxRuns {
			nextStatus = ChatScheduleStatusCompleted
		} else {
			_, _, nextAt, err := validateAndComputeNextCron(sched.CronExpr, sched.Timezone, now)
			if err != nil {
				_, _ = tx.ExecContext(ctx,
					`UPDATE chat_schedules SET status = 'cancelled', updated_at = $1 WHERE id = $2`,
					nowUnix, sched.ID,
				)
				return nil, tx.Commit()
			}
			nextRunAt = nextAt
		}
	} else {
		nextStatus = ChatScheduleStatusCompleted
	}

	updated, err := scanChatSchedule(tx.QueryRowContext(ctx,
		`UPDATE chat_schedules
		    SET run_count = $1, last_run_at = $2, next_run_at = $3,
		        status = $4, updated_at = $2
		  WHERE id = $5
		  RETURNING `+chatScheduleSelectCols,
		newRunCount, nowUnix, nextRunAt, nextStatus, sched.ID,
	))
	if err != nil {
		return nil, err
	}

	clientInputID := fmt.Sprintf("%s%s:%d", ChatPatrolInputKeyPrefix, sched.ID, newRunCount)
	row, _, err := s.insertInputTx(ctx, tx, InputQueueRow{
		ID:             uuid.NewString(),
		ConversationID: convID,
		UserEmail:      owner,
		ClientInputID:  clientInputID,
		Message:        ChatPatrolInputText(updated, newRunCount),
		Attachments:    "[]",
		Mode:           InputModePatrol,
		State:          InputStateQueued,
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ClaimedChatSchedule{Schedule: updated, Input: row}, nil
}

// ChatPatrolInputText formats the synthetic input message for a scheduled
// patrol or timer turn. The turn is persisted with InputKindChatPatrol so the
// web UI renders it as a PatrolNotice card rather than a user speech bubble.
func ChatPatrolInputText(sched ChatSchedule, runNumber int) string {
	kindLabel := "Scheduled patrol"
	if sched.DurationSeconds > 0 && sched.CronExpr == "" {
		kindLabel = "Scheduled timer"
	}
	title := strings.TrimSpace(sched.Title)
	header := fmt.Sprintf("[%s #%d]", kindLabel, runNumber)
	if title != "" {
		header = fmt.Sprintf("[%s: %s (run #%d)]", kindLabel, title, runNumber)
	}
	return fmt.Sprintf(
		"%s\n%s\n\n(Triggered automatically by this conversation's %s schedule id=%s. You are continuing in the same chat conversation with full access to prior messages, workspace files, persistent Python state, and enabled connectors. Carry out the patrol instructions thoroughly, take any needed actions or updates, and write a clear summary of what you checked and changed.)",
		header,
		strings.TrimSpace(sched.Prompt),
		strings.ToLower(kindLabel),
		sched.ID,
	)
}
