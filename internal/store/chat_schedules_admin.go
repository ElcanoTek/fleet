package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Operator access to chat patrols (`fleet patrol`, docs/OPERATORS.md). The
// owner-scoped store methods above (UpdateChatSchedule and friends) take the
// owner's email and conversation, because the HTTP API only ever lets a user
// touch their own patrols. An operator fixing a client's patrol on the box
// has neither, so these methods address a schedule by id alone. They write
// the same columns the owner path writes and bump def_version the same way,
// so a fire already claimed under the old prompt is superseded exactly as if
// the owner had edited it in the Patrol bar.

// NoticePatrolUpdated is the UI-only notice kind written into a chat when an
// operator changes its patrol's instructions.
const NoticePatrolUpdated = "patrol_updated"

// MinChatScheduleIDPrefix is the shortest id prefix the operator lookups
// accept, so a typo cannot silently resolve to some other patrol.
const MinChatScheduleIDPrefix = 8

// MaxAdminChatSchedulePromptBytes bounds a prompt an operator can set. The
// largest live patrol prompts are ~12 KB; 64 KB leaves room without letting a
// wrong file (a CSV, a log) become a prompt.
const MaxAdminChatSchedulePromptBytes = 64 * 1024

// ErrChatScheduleAmbiguous is returned when an id prefix matches more than one
// schedule.
var ErrChatScheduleAmbiguous = errors.New("chat schedule id prefix matches more than one schedule")

// ChatSchedulePromptChangedError is returned when the prompt changed after the
// operator read it: the edit was computed against text that is no longer
// there, so applying it would silently discard the newer edit.
type ChatSchedulePromptChangedError struct {
	Expected string
	Current  string
}

func (e *ChatSchedulePromptChangedError) Error() string {
	return fmt.Sprintf("the patrol prompt changed since it was read (expected sha256 %s, now %s): run `fleet patrol show` again and rebuild the edit from the current prompt", shortSHA(e.Expected), shortSHA(e.Current))
}

// AdminChatSchedule is a chat schedule as an operator sees it: the schedule
// plus its chat's title, its definition version, whether a fire is in flight,
// and the sha256 of its prompt (the value `set-prompt --expect-sha256` takes).
type AdminChatSchedule struct {
	ChatSchedule
	ConversationTitle string `json:"conversation_title"`
	DefVersion        int64  `json:"def_version"`
	FireInFlight      bool   `json:"fire_in_flight"`
	PromptSHA256      string `json:"prompt_sha256"`
	// QueuedRunsUpdated, on the schedule AdminSetChatSchedulePrompt returns,
	// is how many of its runs were already waiting in the chat's queue and now
	// carry the new instructions.
	QueuedRunsUpdated int `json:"queued_runs_updated,omitempty"`
	// RunningWithOldPrompt reports a run of the schedule that was already
	// running when the edit landed: it finishes with the instructions it
	// started with.
	RunningWithOldPrompt bool `json:"running_with_old_prompt,omitempty"`
}

// AdminChatScheduleFilter narrows AdminListChatSchedules.
type AdminChatScheduleFilter struct {
	UserEmail      string
	ConversationID string
	// IncludeTerminal also lists completed and cancelled schedules.
	IncludeTerminal bool
	// PatrolsOnly drops one-shot timers.
	PatrolsOnly bool
	Limit       int
}

// AdminChatSchedulePromptEdit is one operator prompt edit.
type AdminChatSchedulePromptEdit struct {
	// ID is the schedule id or a unique prefix of at least
	// MinChatScheduleIDPrefix characters.
	ID string
	// ExpectSHA256 is the sha256 of the prompt the edit was built from. It is
	// required: an edit without it could overwrite a change the owner made
	// after the operator looked.
	ExpectSHA256 string
	Prompt       string
	// Title, when non-nil and non-blank, also renames the patrol.
	Title *string
	// Notice, when non-empty, is written into the chat as a UI-only notice
	// in the same transaction, so the chat's people see that the patrol's
	// instructions were changed and why.
	Notice string
	// Actor, when set, is recorded in chat.admin_actions with Reason.
	Actor  *AdminActor
	Reason string
}

// PromptSHA256 is the hex sha256 of a patrol prompt as stored.
func PromptSHA256(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

const adminChatScheduleSelect = `
	SELECT s.id, s.conversation_id, s.user_email, s.title, s.prompt, s.cron_expr, s.timezone,
	       s.duration_seconds, s.status, s.next_run_at, s.last_run_at, s.run_count, s.max_runs,
	       s.created_at, s.updated_at,
	       COALESCE(c.title, ''), s.def_version, s.lease_owner IS NOT NULL
	  FROM ` + chatSchedulesTable + ` s
	  JOIN conversations c ON c.id = s.conversation_id AND c.deleted_at IS NULL`

func scanAdminChatSchedule(row interface{ Scan(dest ...any) error }) (AdminChatSchedule, error) {
	var a AdminChatSchedule
	var lastRun sql.NullInt64
	if err := row.Scan(
		&a.ID, &a.ConversationID, &a.UserEmail, &a.Title, &a.Prompt, &a.CronExpr, &a.Timezone,
		&a.DurationSeconds, &a.Status, &a.NextRunAt, &lastRun, &a.RunCount, &a.MaxRuns,
		&a.CreatedAt, &a.UpdatedAt,
		&a.ConversationTitle, &a.DefVersion, &a.FireInFlight,
	); err != nil {
		return AdminChatSchedule{}, err
	}
	if lastRun.Valid {
		v := lastRun.Int64
		a.LastRunAt = &v
	}
	if a.CronExpr != "" {
		a.Kind = ChatScheduleKindPatrol
	} else {
		a.Kind = ChatScheduleKindTimer
	}
	a.PromptSHA256 = PromptSHA256(a.Prompt)
	return a, nil
}

// AdminListChatSchedules lists chat schedules across every user, soonest next
// run first.
func (s *Store) AdminListChatSchedules(ctx context.Context, f AdminChatScheduleFilter) ([]AdminChatSchedule, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if !f.IncludeTerminal {
		where = append(where, `s.status IN ('active', 'paused')`)
	}
	if f.PatrolsOnly {
		where = append(where, `s.cron_expr <> ''`)
	}
	if e := strings.TrimSpace(f.UserEmail); e != "" {
		add(`lower(s.user_email) = lower($%d)`, e)
	}
	if c := strings.TrimSpace(f.ConversationID); c != "" {
		add(`s.conversation_id LIKE $%d`, likePrefix(c))
	}
	q := adminChatScheduleSelect
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY (s.status = 'active') DESC, s.next_run_at, s.id`
	if f.Limit > 0 {
		args = append(args, f.Limit)
		q += fmt.Sprintf(` LIMIT $%d`, len(args))
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AdminChatSchedule
	for rows.Next() {
		a, err := scanAdminChatSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// likePrefix escapes a user-supplied id prefix for a LIKE 'prefix%' match.
func likePrefix(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(p) + "%"
}

// resolveChatScheduleID turns an id or unique prefix into the full id.
func resolveChatScheduleID(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, idOrPrefix string) (string, error) {
	p := strings.TrimSpace(idOrPrefix)
	if len(p) < MinChatScheduleIDPrefix {
		return "", fmt.Errorf("%w: give the schedule id or at least its first %d characters", ErrInvalidChatSchedule, MinChatScheduleIDPrefix)
	}
	rows, err := q.QueryContext(ctx,
		`SELECT s.id FROM `+chatSchedulesTable+` s
		   JOIN conversations c ON c.id = s.conversation_id AND c.deleted_at IS NULL
		  WHERE s.id = $1 OR s.id LIKE $2 ORDER BY (s.id = $1) DESC LIMIT 3`,
		p, likePrefix(p))
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch {
	case len(ids) == 0:
		return "", ErrChatScheduleNotFound
	case ids[0] == p:
		return p, nil
	case len(ids) > 1:
		return "", ErrChatScheduleAmbiguous
	default:
		return ids[0], nil
	}
}

// AdminGetChatSchedule returns one schedule by id or unique id prefix.
func (s *Store) AdminGetChatSchedule(ctx context.Context, idOrPrefix string) (AdminChatSchedule, error) {
	id, err := resolveChatScheduleID(ctx, s.db, idOrPrefix)
	if err != nil {
		return AdminChatSchedule{}, err
	}
	a, err := scanAdminChatSchedule(s.db.QueryRowContext(ctx, adminChatScheduleSelect+` WHERE s.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AdminChatSchedule{}, ErrChatScheduleNotFound
	}
	return a, err
}

// AdminSetChatSchedulePrompt replaces a live (active or paused) schedule's
// prompt — and optionally its title — as an operator, compare-and-swap on the
// prompt's sha256. It returns the schedule before and after. The status, cron
// and next run are untouched; def_version is bumped like an owner's edit, so
// a fire claimed under the old prompt is superseded rather than run with it.
func (s *Store) AdminSetChatSchedulePrompt(ctx context.Context, in AdminChatSchedulePromptEdit) (before, after AdminChatSchedule, err error) {
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return before, after, fmt.Errorf("%w: prompt cannot be empty", ErrInvalidChatSchedule)
	}
	if len(prompt) > MaxAdminChatSchedulePromptBytes {
		return before, after, fmt.Errorf("%w: prompt is %d bytes; the limit is %d", ErrInvalidChatSchedule, len(prompt), MaxAdminChatSchedulePromptBytes)
	}
	expect := strings.ToLower(strings.TrimSpace(in.ExpectSHA256))
	if expect == "" {
		return before, after, fmt.Errorf("%w: the sha256 of the prompt the edit was built from is required", ErrInvalidChatSchedule)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return before, after, err
	}
	defer func() { _ = tx.Rollback() }()

	id, err := resolveChatScheduleID(ctx, tx, in.ID)
	if err != nil {
		return before, after, err
	}
	// Lock the chat, then the schedule: the order a fire's delivery takes
	// them (DeliverChatScheduleFire), so an edit racing a firing patrol waits
	// instead of deadlocking.
	var convID string
	if err := tx.QueryRowContext(ctx, `SELECT conversation_id FROM `+chatSchedulesTable+` WHERE id = $1`, id).Scan(&convID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return before, after, ErrChatScheduleNotFound
		}
		return before, after, err
	}
	// A chat deleted (soft-deleted, inside its restore window) keeps its
	// schedules, but they are not an operator's to edit.
	var liveID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM conversations WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, convID).Scan(&liveID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return before, after, ErrChatScheduleNotFound
		}
		return before, after, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM `+chatSchedulesTable+` WHERE id = $1 FOR UPDATE`, id); err != nil {
		return before, after, err
	}
	before, err = scanAdminChatSchedule(tx.QueryRowContext(ctx, adminChatScheduleSelect+` WHERE s.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return before, after, ErrChatScheduleNotFound
	}
	if err != nil {
		return before, after, err
	}
	if before.Status != ChatScheduleStatusActive && before.Status != ChatScheduleStatusPaused {
		return before, after, fmt.Errorf("%w: cannot modify a %s schedule", ErrInvalidChatSchedule, before.Status)
	}
	if before.PromptSHA256 != expect {
		return before, after, &ChatSchedulePromptChangedError{Expected: expect, Current: before.PromptSHA256}
	}
	title := before.Title
	if in.Title != nil {
		if t := strings.TrimSpace(*in.Title); t != "" {
			title = t
		}
	}
	if prompt == before.Prompt && title == before.Title {
		// Nothing to change: no def_version bump, no notice.
		_ = tx.Commit()
		return before, before, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+chatSchedulesTable+`
		    SET prompt = $1, title = $2, updated_at = $3, def_version = def_version + 1
		  WHERE id = $4`,
		prompt, title, time.Now().Unix(), id); err != nil {
		return before, after, err
	}
	rewritten, err := rewriteQueuedPatrolRuns(ctx, tx, before, prompt, title)
	if err != nil {
		return before, after, err
	}
	var running bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM chat_input_queue
		  WHERE conversation_id = $1 AND state = 'running' AND mode = 'patrol' AND client_input_id LIKE $2)`,
		before.ConversationID, likePrefix(ChatPatrolInputKeyPrefix+before.ID+":")).Scan(&running); err != nil {
		return before, after, err
	}
	if n := strings.TrimSpace(in.Notice); n != "" {
		if err := s.appendNoticeTx(ctx, tx, before.ConversationID, NoticePatrolUpdated, n); err != nil {
			return before, after, err
		}
	}
	if in.Actor != nil {
		if err := in.Actor.validate(); err != nil {
			return before, after, err
		}
		detail := fmt.Sprintf("prompt sha256 %s -> %s", shortSHA(before.PromptSHA256), shortSHA(PromptSHA256(prompt)))
		if title != before.Title {
			detail += fmt.Sprintf("; title %q -> %q", before.Title, title)
		}
		if err := recordAdminAction(ctx, tx, *in.Actor, "patrol_prompt", before.ConversationID, id, strings.TrimSpace(in.Reason), detail); err != nil {
			return before, after, err
		}
	}
	after, err = scanAdminChatSchedule(tx.QueryRowContext(ctx, adminChatScheduleSelect+` WHERE s.id = $1`, id))
	if err != nil {
		return before, after, err
	}
	if err := tx.Commit(); err != nil {
		return before, after, err
	}
	after.QueuedRunsUpdated = rewritten
	after.RunningWithOldPrompt = running
	return before, after, nil
}

// rewriteQueuedPatrolRuns gives the new instructions to the schedule's runs
// that were already delivered into the chat's input queue and are still
// waiting there. The def_version bump only supersedes a fire that has not
// been delivered yet; a delivered run's queue row carries the prompt it was
// claimed with, and would otherwise run the old instructions once more. The
// rows are locked here; a drain claims with FOR UPDATE SKIP LOCKED, so it
// cannot take one of them until this edit commits, and the update re-checks
// that each is still queued.
func rewriteQueuedPatrolRuns(ctx context.Context, tx *sql.Tx, sched AdminChatSchedule, prompt, title string) (int, error) {
	prefix := ChatPatrolInputKeyPrefix + sched.ID + ":"
	rows, err := tx.QueryContext(ctx,
		`SELECT id, client_input_id FROM chat_input_queue
		  WHERE conversation_id = $1 AND state = 'queued' AND mode = 'patrol' AND client_input_id LIKE $2
		  FOR UPDATE`,
		sched.ConversationID, likePrefix(prefix))
	if err != nil {
		return 0, err
	}
	type queued struct {
		id  string
		run int
	}
	var todo []queued
	for rows.Next() {
		var rowID, key string
		if err := rows.Scan(&rowID, &key); err != nil {
			_ = rows.Close()
			return 0, err
		}
		run := 0
		if rest := strings.TrimPrefix(key, prefix); rest != key {
			if i := strings.IndexByte(rest, ':'); i > 0 {
				run, _ = strconv.Atoi(rest[:i])
			}
		}
		todo = append(todo, queued{id: rowID, run: run})
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	edited := sched.ChatSchedule
	edited.Prompt, edited.Title = prompt, title
	now := time.Now().Unix()
	for _, q := range todo {
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_input_queue SET message = $1, updated_at = $2 WHERE id = $3 AND state = 'queued'`,
			ChatPatrolInputText(edited, q.run), now, q.id); err != nil {
			return 0, err
		}
	}
	return len(todo), nil
}

// AdminSetChatScheduleStatus pauses or resumes a live schedule as an
// administrator, through the owner's own path (UpdateChatSchedule: a pause
// revokes a fire in flight and cancels the patrol's queued turns; a resume
// computes the next run), then records it and writes a notice into the chat.
func (s *Store) AdminSetChatScheduleStatus(ctx context.Context, a AdminActor, idOrPrefix, status, reason string) (AdminChatSchedule, error) {
	if err := a.validate(); err != nil {
		return AdminChatSchedule{}, err
	}
	why, err := cleanAdminReason(reason)
	if err != nil {
		return AdminChatSchedule{}, err
	}
	if status != ChatScheduleStatusActive && status != ChatScheduleStatusPaused {
		return AdminChatSchedule{}, fmt.Errorf("%w: status must be active or paused", ErrInvalidChatSchedule)
	}
	cur, err := s.AdminGetChatSchedule(ctx, idOrPrefix)
	if err != nil {
		return AdminChatSchedule{}, err
	}
	if cur.Status == status {
		return cur, nil
	}
	if _, err := s.UpdateChatSchedule(ctx, cur.UserEmail, cur.ConversationID, cur.ID, ChatSchedulePatch{Status: &status}); err != nil {
		return AdminChatSchedule{}, err
	}
	verb := "paused"
	if status == ChatScheduleStatusActive {
		verb = "resumed"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AdminChatSchedule{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.appendNoticeTx(ctx, tx, cur.ConversationID, NoticePatrolUpdated, adminNoticeText(fmt.Sprintf("%s the patrol %q", verb, cur.Title), why)); err != nil {
		return AdminChatSchedule{}, err
	}
	if err := recordAdminAction(ctx, tx, a, "patrol_"+strings.TrimSuffix(verb, "d"), cur.ConversationID, cur.ID, why, ""); err != nil {
		return AdminChatSchedule{}, err
	}
	if err := tx.Commit(); err != nil {
		return AdminChatSchedule{}, err
	}
	return s.AdminGetChatSchedule(ctx, cur.ID)
}
