package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/cron"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Chat schedules are rows of the scheduler (#2049 M5, ADR-0104): the table is
// sched.chat_schedules, created and owned by the sched migrations (076, rows moved by 077), and
// the scheduler's one tick loop claims, leases, settles and records every
// fire (internal/sched/db/chat_schedules.go). The chat store keeps the half
// that belongs to a conversation, in the same database: editing a schedule's
// definition in the transaction that locks and checks its conversation, and
// delivering a fire as a queued turn (DeliverChatScheduleFire).
const chatSchedulesTable = config.SchedSchema + ".chat_schedules"

// deleteUserChatSchedulesTx erases a user's chat schedules (and, by sched's
// cascade, their run history). The scheduler's table may not exist yet when a
// chat-only CLI verb (fleet chat user del, fleet admin rm) runs before the
// first boot has migrated the sched schema; then there is nothing to erase.
func deleteUserChatSchedulesTx(ctx context.Context, tx *sql.Tx, email string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, chatSchedulesTable).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM `+chatSchedulesTable+` WHERE user_email = $1`, email)
	return err
}

// ChatSchedule status values (the sched.chat_schedules CHECK constraint).
const (
	ChatScheduleStatusActive    = models.ChatScheduleStatusActive
	ChatScheduleStatusPaused    = models.ChatScheduleStatusPaused
	ChatScheduleStatusCompleted = models.ChatScheduleStatusCompleted
	ChatScheduleStatusCancelled = models.ChatScheduleStatusCancelled
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

	if _, err := s.lockAndVerifyOwnedOrMultiplayerTx(ctx, tx, in.UserEmail, in.ConversationID); err != nil {
		return nil, err
	}

	// If creating a recurring patrol and AllowMultiple is false, update the
	// conversation's existing active/paused recurring patrol in place.
	if cronSpec != "" && !in.AllowMultiple {
		var existingID string
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM `+chatSchedulesTable+`
			  WHERE conversation_id = $1 AND user_email = $2
			    AND status IN ('active', 'paused') AND cron_expr <> ''
			  ORDER BY created_at DESC, id DESC LIMIT 1
			  FOR UPDATE`,
			in.ConversationID, in.UserEmail,
		).Scan(&existingID)
		if err == nil && existingID != "" {
			sched, err := scanChatSchedule(tx.QueryRowContext(ctx,
				`UPDATE `+chatSchedulesTable+`
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
		`SELECT count(*) FROM `+chatSchedulesTable+`
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
		`INSERT INTO `+chatSchedulesTable+` (
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

// ListChatSchedules lists schedules for a conversation owned by userEmail.
// When includeTerminal is false, only active and paused schedules are returned.
func (s *Store) ListChatSchedules(ctx context.Context, userEmail, convID string, includeTerminal bool) ([]ChatSchedule, error) {
	if s == nil || s.db == nil {
		return []ChatSchedule{}, nil
	}
	query := `SELECT ` + chatScheduleSelectCols + `
		FROM ` + chatSchedulesTable + `
		WHERE conversation_id = $1 AND ($2 = '' OR user_email = $2)`
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

// UserPatrol is one of a user's recurring patrols, summarised for the
// Patrols index in the rail (docs/PATROL-LAUNCHPAD.md): just what the menu
// shows and links to. The prompt stays out — it has no length bound and the
// index never reads it. ConversationOwner is not serialised: the HTTP layer
// uses it to re-check access to chats the caller does not own.
type UserPatrol struct {
	ID                string `json:"id"`
	ConversationID    string `json:"conversation_id"`
	Title             string `json:"title"`
	ConversationTitle string `json:"conversation_title"`
	CronExpr          string `json:"cron"`
	Timezone          string `json:"timezone"`
	Status            string `json:"status"`
	NextRunAt         int64  `json:"next_run_at"`
	ConversationOwner string `json:"-"`
}

// ListUserPatrols returns the active and paused recurring patrols (not
// one-shot timers) that userEmail created, in any non-deleted conversation —
// their own, or a teammate's multiplayer chat they took part in — active ones
// first and each group soonest-next-run first. It does NOT check access to a
// conversation the user does not own; the caller must (multiplayer access can
// be revoked after the patrol was made), so it pages with limit/offset rather
// than capping: rows the caller then drops must not crowd out ones it keeps.
func (s *Store) ListUserPatrols(ctx context.Context, userEmail string, limit, offset int) ([]UserPatrol, error) {
	if s == nil || s.db == nil {
		return []UserPatrol{}, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT cs.id, cs.conversation_id, cs.title, c.title, cs.cron_expr, cs.timezone,
		        cs.status, cs.next_run_at, c.user_email
		   FROM `+chatSchedulesTable+` cs
		   JOIN conversations c ON c.id = cs.conversation_id
		  WHERE cs.user_email = $1
		    AND c.deleted_at IS NULL
		    AND cs.cron_expr <> ''
		    AND cs.status IN ('active', 'paused')
		  ORDER BY (cs.status = 'active') DESC, cs.next_run_at ASC, cs.id ASC
		  LIMIT $2 OFFSET $3`,
		userEmail, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []UserPatrol{}
	for rows.Next() {
		var p UserPatrol
		if err := rows.Scan(&p.ID, &p.ConversationID, &p.Title, &p.ConversationTitle, &p.CronExpr,
			&p.Timezone, &p.Status, &p.NextRunAt, &p.ConversationOwner); err != nil {
			return nil, err
		}
		out = append(out, p)
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
		   FROM `+chatSchedulesTable+`
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
	if cur.Status == ChatScheduleStatusCancelled || cur.Status == ChatScheduleStatusCompleted {
		if isNoOpTerminalSchedulePatch(cur.Status, patch) {
			_ = tx.Commit()
			return &cur, nil
		}
		return nil, fmt.Errorf("%w: cannot modify a %s schedule", ErrInvalidChatSchedule, cur.Status)
	}

	if err := applyChatSchedulePatch(&cur, patch, now); err != nil {
		return nil, err
	}

	updated, err := scanChatSchedule(tx.QueryRowContext(ctx,
		`UPDATE `+chatSchedulesTable+`
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
		// Revoke any fire in flight, under the row lock its settle needs: a
		// fire not yet delivered is then stale, and one already delivered —
		// whose queued turn is cancelled just below — settles to nothing,
		// so a turn the user stopped is never counted as a run and a paused
		// one-shot is never completed behind their back.
		if _, err := tx.ExecContext(ctx,
			`UPDATE `+chatSchedulesTable+`
			    SET lease_owner = NULL, lease_expires_at = NULL, fire_id = NULL,
			        fire_trigger = NULL, fire_run_number = NULL, fire_failures = 0
			  WHERE id = $1 AND lease_owner IS NOT NULL`, cur.ID); err != nil {
			return nil, err
		}
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

func isNoOpTerminalSchedulePatch(curStatus string, patch ChatSchedulePatch) bool {
	return patch.Status != nil && strings.TrimSpace(*patch.Status) == curStatus &&
		patch.Title == nil && patch.Prompt == nil && patch.CronExpr == nil &&
		patch.Timezone == nil && patch.DurationSeconds == nil && patch.MaxRuns == nil
}

func applyChatSchedulePatch(cur *ChatSchedule, patch ChatSchedulePatch, now time.Time) error {
	recomputeNext := false
	if patch.Title != nil {
		if t := strings.TrimSpace(*patch.Title); t != "" {
			cur.Title = t
		}
	}
	if patch.Prompt != nil {
		p := strings.TrimSpace(*patch.Prompt)
		if p == "" {
			return fmt.Errorf("%w: prompt cannot be empty", ErrInvalidChatSchedule)
		}
		cur.Prompt = p
	}
	if patch.Timezone != nil {
		normTZ, _, err := normalizeTimezone(*patch.Timezone)
		if err != nil {
			return err
		}
		cur.Timezone = normTZ
		recomputeNext = true
	}
	if patch.CronExpr != nil {
		if spec := strings.TrimSpace(*patch.CronExpr); spec != "" {
			if cur.CronExpr == "" && patch.MaxRuns == nil {
				cur.MaxRuns = 0
			}
			cur.CronExpr = spec
			cur.DurationSeconds = 0
			recomputeNext = true
		}
	}
	if patch.DurationSeconds != nil && *patch.DurationSeconds > 0 {
		if *patch.DurationSeconds < MinChatTimerDurationSeconds || *patch.DurationSeconds > MaxChatTimerDurationSeconds {
			return fmt.Errorf("%w: duration_seconds must be between %d and %d", ErrInvalidChatSchedule, MinChatTimerDurationSeconds, MaxChatTimerDurationSeconds)
		}
		cur.DurationSeconds = *patch.DurationSeconds
		cur.CronExpr = ""
		cur.MaxRuns = 1
		recomputeNext = true
	}
	if patch.MaxRuns != nil && cur.CronExpr != "" && *patch.MaxRuns >= 0 {
		cur.MaxRuns = *patch.MaxRuns
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
			return fmt.Errorf("%w: unsupported status %q", ErrInvalidChatSchedule, st)
		}
	}
	if recomputeNext && cur.Status == ChatScheduleStatusActive {
		if cur.CronExpr != "" {
			cleanSpec, normTZ, nextAt, err := validateAndComputeNextCron(cur.CronExpr, cur.Timezone, now)
			if err != nil {
				return err
			}
			cur.CronExpr = cleanSpec
			cur.Timezone = normTZ
			cur.NextRunAt = nextAt
		} else if cur.DurationSeconds > 0 {
			cur.NextRunAt = now.Unix() + int64(cur.DurationSeconds)
		}
	}
	return nil
}

// CancelChatSchedule marks a schedule cancelled and cancels any still-queued
// patrol input rows it enqueued.
func (s *Store) CancelChatSchedule(ctx context.Context, userEmail, convID, scheduleID string) (*ChatSchedule, error) {
	status := ChatScheduleStatusCancelled
	return s.UpdateChatSchedule(ctx, userEmail, convID, scheduleID, ChatSchedulePatch{Status: &status})
}

// ChatScheduleInputKey is the chat_input_queue idempotency key of a fire's
// turn: chat-patrol:<schedule>:<run>:<s|m>-<fire id> — s for a scheduled
// fire, m for Run now, so the key says which it was (sched migration 077's
// run-history backfill reads it, on a re-upgrade after a rollback too). The fire id survives the
// lease's expiry (the scheduler's recovery pass re-claims the same fire), so
// a fire delivered twice — its claimer died, or a Run now's caller went away,
// before it settled — finds its own row instead of queueing a second turn;
// and a fire that was revoked (a pause or cancel) never lends its key to a
// later fire of the same run number.
func ChatScheduleInputKey(fire models.ChatScheduleFire) string {
	kind := "s"
	if fire.Trigger == models.ChatScheduleTriggerManual {
		kind = "m"
	}
	return fmt.Sprintf("%s%s:%d:%s-%s", ChatPatrolInputKeyPrefix, fire.ScheduleID, fire.RunNumber, kind, fire.FireID)
}

// DeliverChatScheduleFire is the chat side of a chat schedule fire claimed by
// the scheduler: it enqueues the patrol or timer turn into the conversation's
// input queue, in one transaction that locks the conversation (the same lock
// order as insertInputTx and ClaimApprovalResume) and then the schedule row,
// and re-checks under those locks that the fire is still current.
//
// Outcomes:
//   - gone: the conversation was deleted, or the schedule's owner can no
//     longer use it. The scheduler cancels the schedule.
//   - stale: the schedule left the state the fire was claimed in (paused or
//     cancelled since, or re-claimed after this claim's lease expired).
//   - queued: the turn was enqueued, or had already been by an earlier
//     delivery of this same fire (InputID either way).
//   - coalesced (Run now): a patrol turn of the owner's is already queued or
//     running in this chat; InputID names it and the run does not count.
//   - busy (scheduled): a patrol turn of the owner's is in flight, an approval
//     card is waiting on them, or the input queue holds maxPending rows. The
//     slot is skipped, so patrols never pile up behind one another.
//
// A Run now against a full queue is an error, as it always was.
func (s *Store) DeliverChatScheduleFire(ctx context.Context, fire models.ChatScheduleFire, maxPending int) (models.ChatScheduleDelivery, error) {
	nowUnix := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return models.ChatScheduleDelivery{}, err
	}
	defer func() { _ = tx.Rollback() }()

	gone := models.ChatScheduleDelivery{Outcome: models.ChatScheduleGone}
	if _, err := s.lockAndVerifyOwnedOrMultiplayerTx(ctx, tx, fire.UserEmail, fire.ConversationID); err != nil {
		if errors.Is(err, ErrConversationNotFound) {
			return gone, nil
		}
		return models.ChatScheduleDelivery{}, err
	}

	var status string
	var lease sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT status, lease_owner FROM `+chatSchedulesTable+` WHERE id = $1 FOR UPDATE`,
		fire.ScheduleID,
	).Scan(&status, &lease)
	if errors.Is(err, sql.ErrNoRows) {
		return gone, nil
	}
	if err != nil {
		return models.ChatScheduleDelivery{}, err
	}
	stale := models.ChatScheduleDelivery{Outcome: models.ChatScheduleStale}
	if !lease.Valid || lease.String != fire.LeaseToken {
		return stale, nil
	}
	manual := fire.Trigger == models.ChatScheduleTriggerManual
	if status != ChatScheduleStatusActive && (!manual || status != ChatScheduleStatusPaused) {
		return stale, nil
	}

	key := ChatScheduleInputKey(fire)
	if row, err := scanInputRow(tx.QueryRowContext(ctx,
		inputQueueSelect+` WHERE conversation_id = $1 AND client_input_id = $2`,
		fire.ConversationID, key)); err == nil {
		return models.ChatScheduleDelivery{Outcome: models.ChatScheduleQueued, InputID: row.ID}, tx.Commit()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return models.ChatScheduleDelivery{}, err
	}

	// An earlier patrol turn of this owner's still queued or running.
	inFlight, err := scanInputRow(tx.QueryRowContext(ctx,
		inputQueueSelect+`
		  WHERE conversation_id = $1
		    AND user_email = $2
		    AND state IN ('queued', 'running')
		    AND (mode = 'patrol' OR client_input_id LIKE $3)
		  ORDER BY position ASC LIMIT 1`,
		fire.ConversationID, fire.UserEmail, ChatPatrolInputKeyPrefix+"%",
	))
	hasInFlight := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return models.ChatScheduleDelivery{}, err
	}
	if manual && hasInFlight {
		// Coalesce repeated Run-now clicks while a patrol turn is queued or
		// running, so they neither stack turns nor spend max_runs.
		return models.ChatScheduleDelivery{Outcome: models.ChatScheduleCoalesced, InputID: inFlight.ID}, tx.Commit()
	}

	busy := models.ChatScheduleDelivery{Outcome: models.ChatScheduleBusy}
	if hasInFlight {
		return busy, tx.Commit()
	}
	if !manual {
		var hasPendingApproval bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (
				SELECT 1 FROM approvals
				 WHERE conversation_id = $1
				   AND user_email = $2
				   AND status = 'pending'
				   AND tool_name NOT IN ('preview_email', 'suggest_advanced_model')
			)`,
			fire.ConversationID, fire.UserEmail,
		).Scan(&hasPendingApproval); err != nil {
			return models.ChatScheduleDelivery{}, err
		}
		if hasPendingApproval {
			return busy, tx.Commit()
		}
	}
	if maxPending > 0 {
		var pending int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM chat_input_queue WHERE conversation_id = $1 AND state = 'queued'`,
			fire.ConversationID,
		).Scan(&pending); err != nil {
			return models.ChatScheduleDelivery{}, err
		}
		if pending >= maxPending {
			if manual {
				return models.ChatScheduleDelivery{}, fmt.Errorf("conversation input queue is full (%d queued)", pending)
			}
			return busy, tx.Commit()
		}
	}

	if !manual {
		if err := supersedePendingModelSuggestionsTx(ctx, tx, fire.ConversationID, nowUnix); err != nil {
			return models.ChatScheduleDelivery{}, err
		}
	}
	row, _, err := s.insertInputTx(ctx, tx, InputQueueRow{
		ID:             uuid.NewString(),
		ConversationID: fire.ConversationID,
		UserEmail:      fire.UserEmail,
		ClientInputID:  key,
		Message: ChatPatrolInputText(ChatSchedule{
			ID:              fire.ScheduleID,
			Title:           fire.Title,
			Prompt:          fire.Prompt,
			CronExpr:        fire.CronExpr,
			DurationSeconds: fire.DurationSeconds,
		}, fire.RunNumber),
		Attachments: "[]",
		Mode:        InputModePatrol,
		State:       InputStateQueued,
	})
	if err != nil {
		return models.ChatScheduleDelivery{}, err
	}
	if err := tx.Commit(); err != nil {
		return models.ChatScheduleDelivery{}, err
	}
	return models.ChatScheduleDelivery{Outcome: models.ChatScheduleQueued, InputID: row.ID}, nil
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
		"%s\n%s\n\n(Triggered automatically by this conversation's %s schedule id=%s. You are continuing in the same chat conversation with full access to prior messages, workspace files, enabled connectors, and conversation context. Carry out the patrol instructions thoroughly, take any needed actions or updates, and write a clear summary of what you checked and changed.)",
		header,
		strings.TrimSpace(sched.Prompt),
		strings.ToLower(kindLabel),
		sched.ID,
	)
}

// ListQueuedPatrolConversations returns conversations that currently have a
// queued patrol input row in chat_input_queue (for example, a patrol row
// returned to 'queued' by RecoverInputQueue on daemon restart) so the periodic
// schedule sweep can kick their queue drain.
func (s *Store) ListQueuedPatrolConversations(ctx context.Context) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT conversation_id
		   FROM chat_input_queue
		  WHERE state = 'queued'
		    AND (mode = 'patrol' OR client_input_id LIKE $1)
		  ORDER BY conversation_id ASC
		  LIMIT 25`,
		ChatPatrolInputKeyPrefix+"%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var convID string
		if err := rows.Scan(&convID); err != nil {
			return nil, err
		}
		out = append(out, convID)
	}
	return out, rows.Err()
}

// ExtractChatPatrolPrompt strips the Fleet-generated "[Scheduled ...]" header
// line and "(Triggered automatically by ...)" footer from a ChatPatrolInputText
// entry so promoted tasks and saved workflows preserve the user's patrol
// instructions without synthetic wrapper text.
func ExtractChatPatrolPrompt(text string) string {
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, "[Scheduled ") {
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = strings.TrimSpace(s[nl+1:])
		}
	}
	if idx := strings.LastIndex(s, "\n\n(Triggered automatically by this conversation's "); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	return s
}
