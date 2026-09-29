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
	return accountAccess(ctx, s.db, email)
}

// accountEventsDB is what the account-events statements run on: the pool, or
// the one transaction an AccountEventsTx holds.
type accountEventsDB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func accountAccess(ctx context.Context, q accountEventsDB, email string) (AccountAccess, bool, error) {
	a := AccountAccess{Email: normalizeEmail(email)}
	err := q.QueryRowContext(ctx, `SELECT role, enabled FROM users WHERE email = $1`, a.Email).Scan(&a.Role, &a.Enabled)
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
	return enqueueAccountEvent(ctx, s.db, ev)
}

func enqueueAccountEvent(ctx context.Context, q accountEventsDB, ev AccountEvent) (AccountEvent, error) {
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
	err := q.QueryRowContext(ctx, `INSERT INTO account_events(
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
		SET lease_until = NULL, next_attempt_at = $2, failed_at = $3, last_error = $4, last_failed_at = $5
		WHERE id = $1 AND delivered_at IS NULL`, id, retryAt, failedAt, truncateAccountEventError(message), now)
	return err
}

// ReleaseAccountEventLeases hands claimed rows back without recording an
// attempt: the deliverer was stopped before it could send them, which says
// nothing about the receiver. Without it a shutdown mid-batch leaves every
// claimed row unavailable until its lease expires.
func (s *Store) ReleaseAccountEventLeases(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE account_events
		SET lease_until = NULL, attempts = GREATEST(attempts - 1, 0)
		WHERE id = ANY($1::bigint[]) AND delivered_at IS NULL AND failed_at IS NULL`, ids)
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
		ORDER BY last_failed_at DESC NULLS LAST, id DESC LIMIT 1`).Scan(&lastErr)
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

// DeletedAccountEmails returns emails the feed has reason to believe once had a
// Chat account that no longer exists: an identity-provider desired-state row,
// or an outbox row whose latest event for the email is not a deletion that is
// delivered or still pending (a deletion the receiver rejected or that gave up
// is repaired too, once the receiver is fixed). `fleet account-events resync` republishes these as user.deleted,
// so a deletion event that was lost (a crash before enqueue, a CLI that could
// not open a database, a give-up) is repaired like any other. Delivered and
// given-up outbox rows are pruned after 7 and 30 days, so the provider rows
// are what make this durable for provider-managed accounts.
func (s *Store) DeletedAccountEmails(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT email FROM (
			SELECT email FROM external_access_state
			UNION
			SELECT email FROM (
				SELECT DISTINCT ON (email) email, type, failed_at
				  FROM account_events
				 ORDER BY email, id DESC
			) latest WHERE type <> 'user.deleted' OR failed_at IS NOT NULL
		) known
		WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.email = known.email)
		ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		out = append(out, email)
	}
	return out, rows.Err()
}

// ProviderStateToken identifies the identity provider's stored desired state
// for email as the provider last wrote it — every row's issuer, subject and
// version — so a later AdoptFleetAccessChange can tell whether a provider push
// landed in between. It deliberately leaves out the fields a Fleet adoption
// writes: overlapping Fleet changes are ordered by the per-account lock
// (InAccountEventsTx), each reading its state and adopting it inside the lock,
// so the one that commits last adopts the latest state; a token that moved on
// every Fleet adoption would instead make that last, correct adoption skip.
// "" means the provider holds no row for email.
func (s *Store) ProviderStateToken(ctx context.Context, email string) (string, error) {
	return providerStateToken(ctx, s.db, normalizeEmail(email), "")
}

func providerStateToken(ctx context.Context, q queryer, email, lock string) (string, error) {
	rows, err := q.QueryContext(ctx, `SELECT issuer, subject, version FROM external_access_state
		WHERE email = $1 ORDER BY issuer, subject`+lock, email)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var issuer, subject string
		var version int64
		if err := rows.Scan(&issuer, &subject, &version); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s\x00%s\x00%d\x00", issuer, subject, version)
	}
	return b.String(), rows.Err()
}

// AdoptFleetAccessChange records a change made in Fleet itself (the admin UI,
// the CLI, the boot seed) as the baseline of email's identity-provider
// desired state, so the provider's own at-least-once redelivery of an older,
// already-applied version cannot re-apply the roles it held before Fleet's
// change: handleExternalAccess reconciles the Ops plane from this row even for
// a version it has already seen. exists=false (the account was deleted in
// Fleet) marks the row not allowed, keeping its roles so a newer grant from the
// provider is non-destructive; exists with enabled restores it. Only the
// roles and allowed flag move — the
// version stays the provider's, so its next real change still wins.
//
// token is ProviderStateToken as read before Fleet's change. The rows are
// locked (FOR UPDATE, the lock ApplyExternalAccess takes) and the adoption
// happens only when they still match it: a provider push that committed while
// Fleet's change was in flight is the more recent change, and overwriting its
// roles under its newer version would leave a redelivery of that version
// reconciling Ops from roles the provider never sent while Chat, already at
// that version, is not touched. adopted=false reports that skip (or that the
// provider holds no row for email).
func (s *Store) AdoptFleetAccessChange(ctx context.Context, email, token string, exists, enabled bool, chatRole, opsRole string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	adopted, err := adoptFleetAccessChange(ctx, tx, email, token, exists, enabled, chatRole, opsRole)
	if err != nil || !adopted {
		return false, err
	}
	return true, tx.Commit()
}

// adoptFleetAccessChange is AdoptFleetAccessChange's statements, on a
// transaction the caller commits.
func adoptFleetAccessChange(ctx context.Context, tx accountEventsDB, email, token string, exists, enabled bool, chatRole, opsRole string) (bool, error) {
	email = normalizeEmail(email)
	if exists && (!ValidRole(chatRole) || !validOpsRole(opsRole)) {
		return false, fmt.Errorf("adopt fleet access change: invalid roles %q/%q", chatRole, opsRole)
	}
	current, err := providerStateToken(ctx, tx, email, " FOR UPDATE")
	if err != nil {
		return false, err
	}
	if current == "" || current != token {
		return false, nil
	}
	if exists {
		// allowed follows the account's enabled state, so an account Fleet
		// re-creates after deleting it is allowed again (its deletion had set
		// allowed=false, and a redelivery would otherwise revoke the new Ops
		// grant), while a change to a centrally disabled account keeps it off.
		_, err = tx.ExecContext(ctx, `UPDATE external_access_state SET chat_role = $2, ops_role = $3, allowed = $4 WHERE email = $1`,
			email, chatRole, opsRole, enabled)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE external_access_state SET allowed = FALSE WHERE email = $1`, email)
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// accountEventsLockClass is the first key of the two-key advisory lock that
// serializes one account's feed work (the two-key form never collides with
// the single-key migration lock).
const accountEventsLockClass = 0x0ACC

// AccountEventsTx is one account's feed work — reading its Chat state,
// queuing the event, adopting the provider baseline — on a single chat
// transaction that holds the account's advisory lock (InAccountEventsTx).
type AccountEventsTx struct {
	tx *sql.Tx
}

// AccountAccess is Store.AccountAccess on the transaction.
func (t *AccountEventsTx) AccountAccess(ctx context.Context, email string) (AccountAccess, bool, error) {
	return accountAccess(ctx, t.tx, email)
}

// EnqueueAccountEvent is Store.EnqueueAccountEvent on the transaction.
func (t *AccountEventsTx) EnqueueAccountEvent(ctx context.Context, ev AccountEvent) (AccountEvent, error) {
	return enqueueAccountEvent(ctx, t.tx, ev)
}

// ProviderStateToken is Store.ProviderStateToken on the transaction.
func (t *AccountEventsTx) ProviderStateToken(ctx context.Context, email string) (string, error) {
	return providerStateToken(ctx, t.tx, normalizeEmail(email), "")
}

// AdoptFleetAccessChange is Store.AdoptFleetAccessChange on the transaction,
// inside a savepoint: a failed adoption is rolled back alone, so it can never
// take the event queued before it down with it.
func (t *AccountEventsTx) AdoptFleetAccessChange(ctx context.Context, email, token string, exists, enabled bool, chatRole, opsRole string) (bool, error) {
	if _, err := t.tx.ExecContext(ctx, `SAVEPOINT adopt_baseline`); err != nil {
		return false, err
	}
	adopted, err := adoptFleetAccessChange(ctx, t.tx, email, token, exists, enabled, chatRole, opsRole)
	if err != nil {
		if _, rbErr := t.tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT adopt_baseline`); rbErr != nil {
			return false, errors.Join(err, rbErr)
		}
		return false, err
	}
	_, err = t.tx.ExecContext(ctx, `RELEASE SAVEPOINT adopt_baseline`)
	return adopted, err
}

// InAccountEventsTx runs fn on one chat transaction holding email's advisory
// lock (pg_advisory_xact_lock), and commits what fn wrote even when fn returns
// an error (an adoption failure must not un-queue the event before it).
//
// The Recorder does its after-snapshot, enqueue and adoption here, in the
// server and the CLI alike, so two overlapping changes to one account enqueue
// in the order they read the state: one that read before another's change can
// never be queued after it, which would leave a receiver on the stale state
// for good. Everything runs on the transaction's own connection — the lock
// never holds one pool connection while asking the pool for another, so even
// FLEET_CHAT_DB_MAX_CONNS=1 cannot starve it — and the lock is transaction
// scoped, so it is released by the commit or rollback and can never outlive
// the work on a pooled session. (The Ops-plane read in between goes to the
// separate sched database.)
func (s *Store) InAccountEventsTx(ctx context.Context, email string, fn func(*AccountEventsTx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, accountEventsLockClass, normalizeEmail(email)); err != nil {
		return fmt.Errorf("lock account events: %w", err)
	}
	fnErr := fn(&AccountEventsTx{tx: tx})
	if err := tx.Commit(); err != nil {
		return errors.Join(fnErr, err)
	}
	return fnErr
}
