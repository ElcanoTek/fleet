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

// chatScheduleRunsTable is the schedules' run history (sched-owned; the chat
// store only reads it, to tell an owner their patrol's slots are skipped).
const chatScheduleRunsTable = config.SchedSchema + ".chat_schedule_runs"

// deleteUserChatSchedulesTx erases a user's chat schedules (and, by sched's
// cascade, their run history). The scheduler's table may not exist yet when a
// chat-only CLI verb (fleet chat user del, fleet admin rm) runs before the
// first boot has migrated the sched schema; then there is nothing to erase.
//
// The legacy table matters too: on a database from the previous release that
// a chat-only verb opened (the chat store migrates, the scheduler does not),
// a user's schedules are still in chat.chat_schedules — some of them in other
// people's conversations, which the conversation cascade does not reach — and
// sched migration 077 would later move them under the erased identity. So
// whichever of the two tables exists is cleaned; both, when both do.
func deleteUserChatSchedulesTx(ctx context.Context, tx *sql.Tx, email string) error {
	for _, t := range []struct{ table, del string }{
		{chatSchedulesTable, `DELETE FROM ` + chatSchedulesTable + ` WHERE user_email = $1`},
		{legacyChatSchedulesTable, `DELETE FROM ` + legacyChatSchedulesTable + ` WHERE user_email = $1`},
	} {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, t.table).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := tx.ExecContext(ctx, t.del, email); err != nil {
			return err
		}
	}
	return nil
}

// legacyChatSchedulesTable is where chat schedules lived before sched
// migration 077 moved them (chat migrations 086 + 096).
const legacyChatSchedulesTable = config.ChatSchema + ".chat_schedules"

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

// NoticeArchivePausedSchedules is the UI-only notice written into a chat whose
// patrols were paused because it was archived.
const NoticeArchivePausedSchedules = "archive_paused_schedules"

func archivePausedNotice(n int) string {
	if n == 1 {
		return "Archiving this chat paused its schedule (a patrol or timer). Resume it from the Patrol bar after unarchiving."
	}
	return fmt.Sprintf("Archiving this chat paused its %d patrols and timers. Resume them from the Patrol bar after unarchiving.", n)
}

// errScheduleInArchivedChat refuses starting a schedule in an archived chat.
var errScheduleInArchivedChat = fmt.Errorf("%w: this chat is archived; unarchive it before starting a patrol or timer", ErrInvalidChatSchedule)

// conversationArchivedTx reports whether the chat is archived.
func conversationArchivedTx(ctx context.Context, tx *sql.Tx, convID string) (bool, error) {
	var archived bool
	err := tx.QueryRowContext(ctx, `SELECT archived_at IS NOT NULL FROM conversations WHERE id = $1`, convID).Scan(&archived)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return archived, err
}

// pauseConversationSchedulesTx pauses every active schedule in a chat, whoever
// owns it, with the same effects as an owner's pause (UpdateChatSchedule): a
// fire in flight is revoked and the schedules' queued turns are cancelled. The
// caller holds the conversation's row lock. It returns how many it paused.
func pauseConversationSchedulesTx(ctx context.Context, tx *sql.Tx, convID string, nowUnix int64) (int, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE `+chatSchedulesTable+`
		    SET status = 'paused', updated_at = $2,
		        lease_owner = NULL, lease_expires_at = NULL, fire_id = NULL,
		        fire_trigger = NULL, fire_run_number = NULL, fire_failures = 0
		  WHERE conversation_id = $1 AND status = 'active'`, convID, nowUnix)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	// Queued patrol turns are cancelled even when nothing was active: a Run
	// now on a paused patrol, or a timer that completed after queueing its
	// turn, leaves one behind.
	if _, err := tx.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $1
		  WHERE conversation_id = $2 AND state = 'queued' AND mode = 'patrol'`,
		nowUnix, convID); err != nil {
		return 0, err
	}
	return int(n), nil
}

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
	// Skipped is set on an active recurring patrol whose latest scheduled
	// slots were skipped because an approval card is still waiting on its
	// owner (ListChatSchedules fills it): the chat tells the owner the
	// patrol is held until they answer the card. Nil otherwise.
	Skipped *ChatScheduleSkipped `json:"skipped,omitempty"`
}

// ChatScheduleSkipped describes a patrol's latest run of skipped slots.
type ChatScheduleSkipped struct {
	// Reason is why: models.ChatScheduleBusyApprovalPending (the only
	// reason reported today).
	Reason string `json:"reason"`
	// Count is how many slots in a row were skipped.
	Count int `json:"count"`
	// Slots are the skipped slots' times (Unix seconds), oldest first: the
	// newest maxSkippedSlots of them when Count is larger.
	Slots []int64 `json:"slots"`
}

// maxSkippedSlots bounds ChatScheduleSkipped.Slots; Count still says how
// many there were.
const maxSkippedSlots = 6

// patrolNonBlockingApprovalTools are the pending approval cards that do NOT
// hold a scheduled patrol fire (an email preview, a model suggestion): every
// other pending card of the owner's does (DeliverChatScheduleFire).
const patrolNonBlockingApprovalTools = `('preview_email', 'suggest_advanced_model')`

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

// PreviewChatScheduleCron validates a recurring patrol's cadence exactly as
// an edit does (validateAndComputeNextCron: the cron parser, the timezone and
// MinChatPatrolCronInterval) and returns its canonical form and its next n
// firings after now, as Unix seconds. The administrator's schedule edit shows
// these before it is applied.
func PreviewChatScheduleCron(cronExpr, tz string, now time.Time, n int) (spec, zone string, runs []int64, err error) {
	spec, zone, first, err := validateAndComputeNextCron(cronExpr, tz, now)
	if err != nil {
		return "", "", nil, err
	}
	runs = []int64{first}
	if n <= 1 {
		return spec, zone, runs, nil
	}
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return "", "", nil, fmt.Errorf("%w: invalid cron expression %q: %s", ErrInvalidChatSchedule, spec, err.Error())
	}
	_, loc, err := normalizeTimezone(zone)
	if err != nil {
		return "", "", nil, err
	}
	at := time.Unix(first, 0).In(loc)
	for len(runs) < n {
		at = sched.Next(at)
		if at.IsZero() {
			break
		}
		runs = append(runs, at.Unix())
	}
	return spec, zone, runs, nil
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
	if archived, err := conversationArchivedTx(ctx, tx, in.ConversationID); err != nil {
		return nil, err
	} else if archived {
		return nil, errScheduleInArchivedChat
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
				        max_runs = $6, updated_at = $7, def_version = def_version + 1
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Display data: a failed read leaves the list unannotated rather than
	// failing every caller that only needs the schedules themselves.
	if skipped, err := s.skippedForApproval(ctx, convID, out); err == nil {
		for i := range out {
			out[i].Skipped = skipped[out[i].ID]
		}
	}
	return out, nil
}

// skippedForApproval reports, for each active recurring patrol in list, the
// latest slots it skipped because an approval card is still waiting on the
// patrol's owner. A scheduled fire skips its slot while such a card is
// pending (DeliverChatScheduleFire: busy, reason approval_pending) — by
// design, so a patrol never stacks turns behind an unanswered card — and
// without this the owner had no way to know.
//
// The skipped slots are the trailing streak of the schedule's run history:
// its busy approval_pending rows (or busy rows recorded before reasons were,
// an empty reason) newer than its latest row of any other kind, and fired no
// earlier than the oldest card still pending — so slots skipped behind a
// card already answered are never pinned on the one waiting now. No pending
// card, no entry: the next slot fires as usual once the owner answers.
func (s *Store) skippedForApproval(ctx context.Context, convID string, list []ChatSchedule) (map[string]*ChatScheduleSkipped, error) {
	ids := make([]string, 0, len(list))
	for _, sc := range list {
		if sc.Status == ChatScheduleStatusActive && sc.Kind == ChatScheduleKindPatrol {
			ids = append(ids, sc.ID)
		}
	}
	return s.skippedSchedules(ctx, convID, ids)
}

// skippedSchedules is skippedForApproval for a set of schedule IDs. convID
// narrows it to one conversation; "" reads the IDs wherever they live (the
// Patrols page lists a person's patrols across their chats).
func (s *Store) skippedSchedules(ctx context.Context, convID string, ids []string) (map[string]*ChatScheduleSkipped, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, slot, n FROM (
			SELECT cs.id,
			       COALESCE(r.due_at, floor(EXTRACT(EPOCH FROM r.fired_at))::bigint) AS slot,
			       r.id AS run_id,
			       count(*) OVER (PARTITION BY cs.id) AS n,
			       row_number() OVER (PARTITION BY cs.id ORDER BY r.id DESC) AS rn
			  FROM `+chatSchedulesTable+` cs
			  JOIN LATERAL (
			        SELECT min(a.created_at) AS since
			          FROM approvals a
			         WHERE a.conversation_id = cs.conversation_id
			           AND a.user_email = cs.user_email
			           AND a.status = 'pending'
			           AND a.tool_name NOT IN `+patrolNonBlockingApprovalTools+`
			       ) pend ON pend.since IS NOT NULL
			  JOIN `+chatScheduleRunsTable+` r ON r.schedule_id = cs.id
			 WHERE ($1 = '' OR cs.conversation_id = $1)
			   AND cs.id = ANY($2::text[])
			   AND r.trigger_kind = 'schedule'
			   AND r.outcome = 'busy'
			   AND r.reason IN ($3, '')
			   AND r.fired_at >= to_timestamp(pend.since)
			   AND r.id > COALESCE((
			        SELECT max(o.id) FROM `+chatScheduleRunsTable+` o
			         WHERE o.schedule_id = cs.id
			           AND NOT (o.outcome = 'busy' AND o.reason IN ($3, ''))
			       ), 0)
		) streak
		 WHERE rn <= $4
		 ORDER BY id, run_id`,
		convID, ids, models.ChatScheduleBusyApprovalPending, maxSkippedSlots)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*ChatScheduleSkipped{}
	for rows.Next() {
		var (
			id      string
			slot, n int64
		)
		if err := rows.Scan(&id, &slot, &n); err != nil {
			return nil, err
		}
		sk := out[id]
		if sk == nil {
			sk = &ChatScheduleSkipped{Reason: models.ChatScheduleBusyApprovalPending, Count: int(n)}
			out[id] = sk
		}
		sk.Slots = append(sk.Slots, slot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// UserPatrol is one recurring patrol as the Patrols page and the sidebar list
// it (docs/SIDEBAR.md, docs/PATROL-LAUNCHPAD.md): what a row shows and links
// to. The listings leave the prompt out — it has no length bound and no row
// reads it; GetUserPatrol fills it for the patrol's own creator.
// ConversationOwner is not serialised: the HTTP layer uses it to re-check
// access to chats the caller does not own.
type UserPatrol struct {
	ID                string `json:"id"`
	ConversationID    string `json:"conversation_id"`
	Title             string `json:"title"`
	ConversationTitle string `json:"conversation_title"`
	CronExpr          string `json:"cron"`
	Timezone          string `json:"timezone"`
	Status            string `json:"status"`
	NextRunAt         int64  `json:"next_run_at"`
	LastRunAt         *int64 `json:"last_run_at,omitempty"`
	RunCount          int    `json:"run_count"`
	// ApprovalMode is the patrol's chat's approval mode ("manual", "auto" or
	// "autonomous"): how much a run may do without stopping for a card.
	ApprovalMode string `json:"approval_mode"`
	// Mine is true for a patrol the caller set up (they can run, pause, edit
	// and remove it). False is a teammate's patrol in a chat the caller can
	// open: listed to read, and OwnerEmail says whose it is.
	Mine       bool   `json:"mine"`
	OwnerEmail string `json:"owner_email,omitempty"`
	// OwnsChat is true when the patrol's chat is the caller's own. The
	// approval mode is the chat's, and only its owner may change it
	// (SetApprovalMode), so a patrol the caller set up in a teammate's
	// Multiplayer chat is Mine with OwnsChat false.
	OwnsChat bool `json:"owns_chat"`
	// Multiplayer and TeamVisible say how the patrol's chat is shared, so a
	// teammate's row links to the page that opens for them: the chat itself
	// (Multiplayer) or its read-only team view.
	Multiplayer bool `json:"multiplayer,omitempty"`
	TeamVisible bool `json:"team_visible,omitempty"`
	// Skipped is set on the caller's own active patrol whose latest slots
	// were skipped because an approval card is waiting on them.
	Skipped *ChatScheduleSkipped `json:"skipped,omitempty"`
	// Prompt is what each run is asked to do. Only GetUserPatrol sets it,
	// and only for the patrol's own creator.
	Prompt            string `json:"prompt,omitempty"`
	ConversationOwner string `json:"-"`
}

// userPatrolCols selects a UserPatrol row from chat schedules cs joined to
// their conversation c. The approval mode is the conversation's, reconciled
// the way Conversation.EffectiveApprovalMode does for a row written before
// approval_mode existed.
const userPatrolCols = `cs.id, cs.conversation_id, cs.title, c.title, cs.cron_expr, cs.timezone,
	cs.status, cs.next_run_at, cs.last_run_at, cs.run_count,
	CASE WHEN COALESCE(c.approval_mode, '') IN ('auto', 'autonomous') THEN c.approval_mode
	     WHEN c.auto_approve THEN 'auto' ELSE 'manual' END,
	cs.user_email, c.user_email, c.multiplayer, c.team_visible`

func scanUserPatrol(row rowScanner, caller string) (UserPatrol, error) {
	var (
		p       UserPatrol
		lastRun sql.NullInt64
		creator string
	)
	if err := row.Scan(&p.ID, &p.ConversationID, &p.Title, &p.ConversationTitle, &p.CronExpr,
		&p.Timezone, &p.Status, &p.NextRunAt, &lastRun, &p.RunCount, &p.ApprovalMode,
		&creator, &p.ConversationOwner, &p.Multiplayer, &p.TeamVisible); err != nil {
		return UserPatrol{}, err
	}
	if lastRun.Valid {
		p.LastRunAt = &lastRun.Int64
	}
	p.Mine = strings.EqualFold(creator, caller)
	p.OwnsChat = strings.EqualFold(p.ConversationOwner, caller)
	if !p.Mine {
		p.OwnerEmail = creator
	}
	return p, nil
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
		`SELECT `+userPatrolCols+`
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
		p, err := scanUserPatrol(rows, userEmail)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListSharedPatrols returns the active and paused recurring patrols that
// OTHER people run in chats userEmail can open: a teammate's chat shared
// with their team or in Multiplayer (the set
// ListSharedAndMultiplayerConversations gives, so the rule for "can open" is
// that one rule), and their own chats where a participant set a patrol up.
// Active first, soonest next run first, at most limit rows. These are for
// reading: only its creator can run, pause, edit or remove a patrol.
func (s *Store) ListSharedPatrols(ctx context.Context, userEmail string, limit int) ([]UserPatrol, error) {
	if s == nil || s.db == nil || strings.TrimSpace(userEmail) == "" {
		return []UserPatrol{}, nil
	}
	shared, err := s.ListSharedAndMultiplayerConversations(ctx, userEmail)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(shared))
	sharedByID := make(map[string]*Conversation, len(shared))
	for i := range shared {
		ids = append(ids, shared[i].ID)
		sharedByID[shared[i].ID] = &shared[i]
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userPatrolCols+`
		   FROM `+chatSchedulesTable+` cs
		   JOIN conversations c ON c.id = cs.conversation_id
		  WHERE cs.user_email <> $1
		    AND (c.id = ANY($2::text[]) OR c.user_email = $1)
		    AND c.deleted_at IS NULL
		    AND c.archived_at IS NULL
		    AND cs.cron_expr <> ''
		    AND cs.status IN ('active', 'paused')
		  ORDER BY (cs.status = 'active') DESC, cs.next_run_at ASC, cs.id ASC
		  LIMIT $3`,
		userEmail, ids, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []UserPatrol{}
	for rows.Next() {
		p, err := scanUserPatrol(rows, userEmail)
		if err != nil {
			return nil, err
		}
		// How the caller reaches a teammate's chat is the shared list's
		// answer, not the chat's raw flags: a Multiplayer chat the caller is
		// excluded from but can read as a team chat is listed there with
		// Multiplayer false, and its patrol must link to the team view.
		if c, ok := sharedByID[p.ConversationID]; ok {
			p.Multiplayer = c.Multiplayer
			p.TeamVisible = c.TeamVisible
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PatrolSkips reports, for the given patrols (the caller's own, across
// their chats), the latest slots each skipped because an approval card is
// waiting: the same answer ListChatSchedules gives per chat. Display data;
// the listings call it best-effort.
func (s *Store) PatrolSkips(ctx context.Context, scheduleIDs []string) (map[string]*ChatScheduleSkipped, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	return s.skippedSchedules(ctx, "", scheduleIDs)
}

// GetUserPatrol returns one active or paused recurring patrol by ID, or nil
// when there is none. It does NOT check that userEmail may see it (the HTTP
// layer does, with the same rules as the listings); it only decides whose it
// is, and carries the prompt for its creator alone.
func (s *Store) GetUserPatrol(ctx context.Context, userEmail, scheduleID string) (*UserPatrol, error) {
	if s == nil || s.db == nil || scheduleID == "" {
		return nil, nil
	}
	var prompt string
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userPatrolCols+`, cs.prompt
		   FROM `+chatSchedulesTable+` cs
		   JOIN conversations c ON c.id = cs.conversation_id
		  WHERE cs.id = $1
		    AND c.deleted_at IS NULL
		    AND cs.cron_expr <> ''
		    AND cs.status IN ('active', 'paused')`,
		scheduleID,
	)
	p, err := scanUserPatrol(patrolWithPrompt{row, &prompt}, userEmail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if p.Mine {
		p.Prompt = prompt
	}
	return &p, nil
}

// patrolWithPrompt scans userPatrolCols plus one trailing prompt column.
type patrolWithPrompt struct {
	row    rowScanner
	prompt *string
}

func (r patrolWithPrompt) Scan(dest ...any) error {
	return r.row.Scan(append(dest, r.prompt)...)
}

// PatrolRun is one entry of a patrol's run history, newest first: a run that
// was queued into the chat, or a slot that was turned away and why.
type PatrolRun struct {
	// At is when the scheduler fired it (Unix seconds); DueAt the slot it
	// was for, when recorded.
	At    int64 `json:"at"`
	DueAt int64 `json:"due_at,omitempty"`
	// Trigger is "schedule" (the cadence) or "manual" (Run now).
	Trigger string `json:"trigger"`
	// Outcome is "queued" (a turn ran in the chat), "busy" (the slot was
	// skipped; Reason says why) or "gone" (the chat was no longer there).
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason,omitempty"`
	RunNumber int    `json:"run_number,omitempty"`
}

// MaxPatrolRuns bounds one read of a patrol's run history.
const MaxPatrolRuns = 20

// ListPatrolRuns returns the newest limit entries of a patrol's run history
// (sched.chat_schedule_runs, which the scheduler writes and prunes with the
// tasks' history). It checks no access; the caller has.
func (s *Store) ListPatrolRuns(ctx context.Context, scheduleID string, limit int) ([]PatrolRun, error) {
	if s == nil || s.db == nil || scheduleID == "" {
		return []PatrolRun{}, nil
	}
	if limit <= 0 || limit > MaxPatrolRuns {
		limit = MaxPatrolRuns
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT floor(EXTRACT(EPOCH FROM fired_at))::bigint, COALESCE(due_at, 0), trigger_kind, outcome,
		        reason, COALESCE(run_number, 0)
		   FROM `+chatScheduleRunsTable+`
		  WHERE schedule_id = $1
		  ORDER BY id DESC
		  LIMIT $2`,
		scheduleID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PatrolRun{}
	for rows.Next() {
		var r PatrolRun
		if err := rows.Scan(&r.At, &r.DueAt, &r.Trigger, &r.Outcome, &r.Reason, &r.RunNumber); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateChatSchedule applies a partial update (pause, resume, or cadence/prompt
// edit) to an existing schedule owned by userEmail in convID.
func (s *Store) UpdateChatSchedule(ctx context.Context, userEmail, convID, scheduleID string, patch ChatSchedulePatch) (*ChatSchedule, error) {
	now := time.Now()

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

	updated, err := updateLockedChatScheduleTx(ctx, tx, cur, patch, now)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &updated, nil
}

// updateLockedChatScheduleTx applies patch to cur, a live schedule row the
// caller has already locked FOR UPDATE inside tx, and writes it: the one
// write path of a schedule edit, shared by the owner (UpdateChatSchedule) and
// an administrator (AdminSetChatScheduleCron), so both validate the same way
// (applyChatSchedulePatch), bump def_version the same way (a definition edit
// supersedes a fire claimed under the old definition), recompute the next run
// the same way, and revoke a fire in flight on a pause or cancel.
func updateLockedChatScheduleTx(ctx context.Context, tx *sql.Tx, cur ChatSchedule, patch ChatSchedulePatch, now time.Time) (ChatSchedule, error) {
	nowUnix := now.Unix()
	wasActive := cur.Status == ChatScheduleStatusActive
	if err := applyChatSchedulePatch(&cur, patch, now); err != nil {
		return ChatSchedule{}, err
	}
	if !wasActive && cur.Status == ChatScheduleStatusActive {
		// Resuming: an archived chat's schedules stay off (SetArchived paused
		// them so a filed-away chat stops spending).
		if archived, err := conversationArchivedTx(ctx, tx, cur.ConversationID); err != nil {
			return ChatSchedule{}, err
		} else if archived {
			return ChatSchedule{}, errScheduleInArchivedChat
		}
	}

	updated, err := scanChatSchedule(tx.QueryRowContext(ctx,
		`UPDATE `+chatSchedulesTable+`
		    SET title = $1, prompt = $2, cron_expr = $3, timezone = $4,
		        duration_seconds = $5, status = $6, next_run_at = $7,
		        max_runs = $8, updated_at = $9,
		        def_version = def_version + CASE WHEN $11 THEN 1 ELSE 0 END
		  WHERE id = $10
		  RETURNING `+chatScheduleSelectCols,
		cur.Title, cur.Prompt, cur.CronExpr, cur.Timezone,
		cur.DurationSeconds, cur.Status, cur.NextRunAt,
		cur.MaxRuns, nowUnix, cur.ID, patch.editsDefinition(),
	))
	if err != nil {
		return ChatSchedule{}, err
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
			return ChatSchedule{}, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_input_queue
			    SET state = 'cancelled', updated_at = $1
			  WHERE conversation_id = $2 AND state = 'queued' AND mode = 'patrol'
			    AND client_input_id LIKE $3`,
			nowUnix, cur.ConversationID, ChatPatrolInputKeyPrefix+cur.ID+":%",
		); err != nil {
			return ChatSchedule{}, err
		}
	}

	return updated, nil
}

// editsDefinition reports whether the patch edits the schedule's definition
// (what a fire delivers or how the next slot is computed) rather than only
// its status. Such an edit bumps def_version, superseding any fire claimed
// under the old definition.
func (p ChatSchedulePatch) editsDefinition() bool {
	return p.Title != nil || p.Prompt != nil || p.CronExpr != nil || p.Timezone != nil ||
		p.DurationSeconds != nil || p.MaxRuns != nil
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
	if !recomputeNext {
		return nil
	}
	// A new cadence or timezone on a recurring schedule is validated (and
	// stored in its canonical form) whether the schedule is running or
	// paused: a paused patrol given a cron that cannot run would otherwise
	// be refused only when someone tries to resume it. Only a running
	// schedule's next run moves now; a resume computes a paused one's.
	if cur.CronExpr != "" && cur.Status != ChatScheduleStatusCancelled {
		cleanSpec, normTZ, nextAt, err := validateAndComputeNextCron(cur.CronExpr, cur.Timezone, now)
		if err != nil {
			return err
		}
		cur.CronExpr = cleanSpec
		cur.Timezone = normTZ
		if cur.Status == ChatScheduleStatusActive {
			cur.NextRunAt = nextAt
		}
	} else if cur.DurationSeconds > 0 && cur.Status == ChatScheduleStatusActive {
		cur.NextRunAt = now.Unix() + int64(cur.DurationSeconds)
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
//   - rejected (Run now): the input queue holds maxPending rows. The user is
//     told (429) and the fire ends; nothing runs it once the queue drains.
//
// Errors are only for what a retry may fix (the database).
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

	var (
		status     string
		lease      sql.NullString
		defVersion int64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT status, lease_owner, def_version FROM `+chatSchedulesTable+` WHERE id = $1 FOR UPDATE`,
		fire.ScheduleID,
	).Scan(&status, &lease, &defVersion)
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
	if defVersion != fire.DefVersion {
		// The definition was edited after the claim (a new prompt, cadence or
		// kind): this fire's snapshot is out of date. Queue nothing and end
		// the fire; the scheduler records nothing and its next pass fires the
		// definition as it is now.
		if _, err := tx.ExecContext(ctx,
			`UPDATE `+chatSchedulesTable+`
			    SET lease_owner = NULL, lease_expires_at = NULL, fire_id = NULL, fire_trigger = NULL,
			        fire_run_number = NULL, fire_failures = 0, fire_def_version = NULL
			  WHERE id = $1`, fire.ScheduleID); err != nil {
			return models.ChatScheduleDelivery{}, err
		}
		return models.ChatScheduleDelivery{Outcome: models.ChatScheduleSuperseded}, tx.Commit()
	}

	// Patrol provenance is the server-owned mode, never the caller-supplied
	// client_input_id alone: a user input that borrows a chat-patrol: key is
	// not this fire's turn.
	key := ChatScheduleInputKey(fire)
	if row, err := scanInputRow(tx.QueryRowContext(ctx,
		inputQueueSelect+` WHERE conversation_id = $1 AND client_input_id = $2 AND mode = 'patrol'`,
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
		    AND mode = 'patrol'
		  ORDER BY position ASC LIMIT 1`,
		fire.ConversationID, fire.UserEmail,
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

	busy := func(reason string) models.ChatScheduleDelivery {
		return models.ChatScheduleDelivery{Outcome: models.ChatScheduleBusy, Reason: reason}
	}
	if hasInFlight {
		return busy(models.ChatScheduleBusyPatrolInFlight), tx.Commit()
	}
	if !manual {
		var hasPendingApproval bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (
				SELECT 1 FROM approvals
				 WHERE conversation_id = $1
				   AND user_email = $2
				   AND status = 'pending'
				   AND tool_name NOT IN `+patrolNonBlockingApprovalTools+`
			)`,
			fire.ConversationID, fire.UserEmail,
		).Scan(&hasPendingApproval); err != nil {
			return models.ChatScheduleDelivery{}, err
		}
		if hasPendingApproval {
			return busy(models.ChatScheduleBusyApprovalPending), tx.Commit()
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
				// Refused for good, not an error to retry: a retry would run
				// the turn later, unasked, once the queue drained.
				return models.ChatScheduleDelivery{Outcome: models.ChatScheduleRejected, Reason: models.ChatScheduleBusyQueueFull}, tx.Commit()
			}
			return busy(models.ChatScheduleBusyQueueFull), tx.Commit()
		}
	}

	if !manual {
		if err := supersedePendingModelSuggestionsTx(ctx, tx, fire.ConversationID, nowUnix); err != nil {
			return models.ChatScheduleDelivery{}, err
		}
	}
	// Record, with the delivered turn, the slot this fire moves a patrol to.
	// The settle computes it again; this copy is for 077's down migration,
	// which must count a delivered-but-unsettled fire the way the old engine
	// would (it cannot evaluate cron in SQL).
	if strings.TrimSpace(fire.CronExpr) != "" {
		if next, nerr := models.NextChatScheduleRun(fire.CronExpr, fire.Timezone, time.Now()); nerr == nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE `+chatSchedulesTable+` SET fire_next_run_at = $2 WHERE id = $1`,
				fire.ScheduleID, next.Unix()); err != nil {
				return models.ChatScheduleDelivery{}, err
			}
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
// queued patrol or admin-message input row in chat_input_queue (for example,
// a row returned to 'queued' by RecoverInputQueue on daemon restart) so the
// periodic schedule sweep can kick their queue drain. Neither kind has a
// client waiting to submit again, which is what would otherwise kick it.
func (s *Store) ListQueuedPatrolConversations(ctx context.Context) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT conversation_id
		   FROM chat_input_queue
		  WHERE state = 'queued'
		    AND mode IN ('patrol', 'admin')
		  ORDER BY conversation_id ASC
		  LIMIT 25`,
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
