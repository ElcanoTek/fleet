package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Account event types and sources (docs/ACCOUNT-EVENTS.md). Kept in sync with
// the CHECK constraints in migrations/067_account_events.sql.
const (
	AccountEventAccessChanged = "user.access_changed"
	AccountEventDeleted       = "user.deleted"

	AccountEventSourceAdminUI          = "admin_ui"
	AccountEventSourceCLI              = "cli"
	AccountEventSourceSystem           = "system"
	AccountEventSourceResync           = "resync"
	AccountEventSourceIdentityProvider = "identity_provider"
)

// AccountEvent is one outbox row: the full resulting state of one Chat account
// after a change, plus its delivery bookkeeping.
type AccountEvent struct {
	ID         int64
	EventID    string
	Type       string
	Email      string
	Enabled    bool
	ChatRole   string
	OpsRole    string
	Source     string
	Actor      string
	OccurredAt int64
	CreatedAt  int64
	Attempts   int
}

// AccountAccess is the Chat plane's membership view of one account: its role
// and whether Central Auth (or an operator) left it enabled.
type AccountAccess struct {
	Email   string
	Role    string
	Enabled bool
}

// AccountEventStats summarizes the outbox for `fleet account-events status`.
// LastError is the most recent failure message; it never carries the URL's
// path or query, or the signing secret.
type AccountEventStats struct {
	Pending         int
	Failed          int
	Delivered       int
	OldestPendingAt int64
	LastError       string
}

// AccountAccess returns the role and enabled flag for email, including
// centrally disabled rows (GetUser hides those). found=false means no Chat
// account exists.
func (s *Store) AccountAccess(ctx context.Context, email string) (AccountAccess, bool, error) {
	a := AccountAccess{Email: normalizeEmail(email)}
	err := s.db.QueryRowContext(ctx, `SELECT role, enabled FROM users WHERE email = $1`, a.Email).Scan(&a.Role, &a.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountAccess{}, false, nil
	}
	if err != nil {
		return AccountAccess{}, false, err
	}
	return a, true, nil
}

// ListAccountAccess returns every Chat account's membership view, disabled rows
// included, ordered by email.
func (s *Store) ListAccountAccess(ctx context.Context) ([]AccountAccess, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT email, role, enabled FROM users ORDER BY email ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountAccess
	for rows.Next() {
		var a AccountAccess
		if err := rows.Scan(&a.Email, &a.Role, &a.Enabled); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// EnqueueAccountEvent appends ev to the outbox, due immediately. EventID,
// OccurredAt and CreatedAt are filled in when empty. The stored row (with its
// sequence ID) is returned.
func (s *Store) EnqueueAccountEvent(ctx context.Context, ev AccountEvent) (AccountEvent, error) {
	ev.Email = normalizeEmail(ev.Email)
	ev.Actor = normalizeEmail(ev.Actor)
	if ev.Email == "" {
		return AccountEvent{}, errors.New("account event: email required")
	}
	if ev.EventID == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return AccountEvent{}, err
		}
		ev.EventID = "evt_" + hex.EncodeToString(b[:])
	}
	now := time.Now().Unix()
	if ev.OccurredAt == 0 {
		ev.OccurredAt = now
	}
	if ev.CreatedAt == 0 {
		ev.CreatedAt = now
	}
	err := s.db.QueryRowContext(ctx, `INSERT INTO account_events(
		event_id, type, email, enabled, chat_role, ops_role, source, actor, occurred_at, created_at, next_attempt_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10) RETURNING id`,
		ev.EventID, ev.Type, ev.Email, ev.Enabled, ev.ChatRole, ev.OpsRole, ev.Source, ev.Actor,
		ev.OccurredAt, ev.CreatedAt).Scan(&ev.ID)
	if err != nil {
		return AccountEvent{}, fmt.Errorf("enqueue account event: %w", err)
	}
	return ev, nil
}

// ClaimDueAccountEvents leases up to limit deliverable rows. Only the oldest
// undelivered row of each email is eligible, so one account's events reach the
// receiver in order: a newer event waits while an older one is retrying. A
// claimed row stays leased for lease; an unacknowledged lease expires and the
// row becomes claimable again.
func (s *Store) ClaimDueAccountEvents(ctx context.Context, now int64, limit int, lease time.Duration) ([]AccountEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH heads AS (
			SELECT DISTINCT ON (email) id, next_attempt_at, lease_until
			  FROM account_events
			 WHERE delivered_at IS NULL AND failed_at IS NULL
			 ORDER BY email, id
		), due AS (
			SELECT id FROM heads
			 WHERE next_attempt_at <= $1 AND (lease_until IS NULL OR lease_until <= $1)
			 ORDER BY id
			 LIMIT $2
		)
		UPDATE account_events e
		   SET lease_until = $3, attempts = e.attempts + 1
		  FROM due
		 WHERE e.id = due.id AND (e.lease_until IS NULL OR e.lease_until <= $1)
		RETURNING e.id, e.event_id, e.type, e.email, e.enabled, e.chat_role, e.ops_role,
		          e.source, e.actor, e.occurred_at, e.created_at, e.attempts`,
		now, limit, now+int64(lease/time.Second))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountEvent
	for rows.Next() {
		var ev AccountEvent
		if err := rows.Scan(&ev.ID, &ev.EventID, &ev.Type, &ev.Email, &ev.Enabled, &ev.ChatRole, &ev.OpsRole,
			&ev.Source, &ev.Actor, &ev.OccurredAt, &ev.CreatedAt, &ev.Attempts); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// RETURNING carries no order guarantee; deliver oldest first.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// MarkAccountEventDelivered records the receiver's 2xx and releases the lease.
func (s *Store) MarkAccountEventDelivered(ctx context.Context, id, now int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE account_events
		SET delivered_at = $2, lease_until = NULL, last_error = NULL
		WHERE id = $1 AND delivered_at IS NULL`, id, now)
	return err
}

// MarkAccountEventFailed records a failed attempt. giveUp ends the row's
// retries (failed_at) so the email's next event can proceed; otherwise the row
// is retried at retryAt.
func (s *Store) MarkAccountEventFailed(ctx context.Context, id, now, retryAt int64, giveUp bool, message string) error {
	var failedAt any
	if giveUp {
		failedAt = now
	}
	_, err := s.db.ExecContext(ctx, `UPDATE account_events
		SET lease_until = NULL, next_attempt_at = $2, failed_at = $3, last_error = $4
		WHERE id = $1 AND delivered_at IS NULL`, id, retryAt, failedAt, truncateAccountEventError(message))
	return err
}

func truncateAccountEventError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 500 {
		return message[:500]
	}
	return message
}

// AccountEventStats counts outbox rows by state.
func (s *Store) AccountEventStats(ctx context.Context) (AccountEventStats, error) {
	var st AccountEventStats
	var oldest sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT
		COUNT(*) FILTER (WHERE delivered_at IS NULL AND failed_at IS NULL),
		COUNT(*) FILTER (WHERE failed_at IS NOT NULL),
		COUNT(*) FILTER (WHERE delivered_at IS NOT NULL),
		MIN(created_at) FILTER (WHERE delivered_at IS NULL AND failed_at IS NULL)
		FROM account_events`).Scan(&st.Pending, &st.Failed, &st.Delivered, &oldest)
	if err != nil {
		return st, err
	}
	st.OldestPendingAt = oldest.Int64
	var lastErr sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT last_error FROM account_events
		WHERE last_error IS NOT NULL AND delivered_at IS NULL
		ORDER BY id DESC LIMIT 1`).Scan(&lastErr)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	st.LastError = lastErr.String
	return st, nil
}

// PruneAccountEvents deletes delivered rows older than deliveredBefore and
// given-up rows older than failedBefore (unix seconds). Pending rows are never
// pruned.
func (s *Store) PruneAccountEvents(ctx context.Context, deliveredBefore, failedBefore int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM account_events
		WHERE (delivered_at IS NOT NULL AND delivered_at < $1)
		   OR (failed_at IS NOT NULL AND failed_at < $2)`, deliveredBefore, failedBefore)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
