package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// adminRequestActorDeleted is the result a pending chat-administration
// request gets when the administrator who asked for it is deleted.
const adminRequestActorDeleted = "cancelled: the requesting administrator's account was deleted"

// cancelPendingAdminWorkTx cancels, inside DeleteUser's transaction, the
// chat-administration work the account asked for that no server has picked
// up: a request still pending fails with a fixed reason, and a queued
// admin-mode message it sent is cancelled (a row claimed but not yet launched
// counts as queued). Work already under way is left to finish: a request a
// server claimed (running) is carried out and settled as usual, and an
// admin-mode message whose turn started runs to its end — it runs as the
// chat's owner. Finished requests, the audit log and administrators' notes
// stay as history.
//
// DeleteUser runs it right after its guard, before it touches any chat: a
// server queueing a message locks the request row, then the chat
// (QueueAdminMessage), so taking the request rows first here keeps the two
// in one order. The request rows are updated first, under the row locks a
// server taking a request also takes (ClaimAdminRequest, QueueAdminMessage):
// a message it queued before this statement is then cancelled by the next.
func cancelPendingAdminWorkTx(ctx context.Context, tx *sql.Tx, email string) error {
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx,
		`UPDATE chat.admin_requests SET state = 'failed', result = $2, updated_at = $3
		  WHERE state = 'pending' AND actor = $1`,
		email, adminRequestActorDeleted, now); err != nil {
		return fmt.Errorf("cancel pending chat-administration requests: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE chat.chat_input_queue SET state = 'cancelled', updated_at = $2
		  WHERE mode = 'admin' AND sender_email = $1
		    AND (state = 'queued' OR (state = 'running' AND turn_id LIKE $3))`,
		email, now, ClaimTurnPrefix+"%"); err != nil {
		return fmt.Errorf("cancel queued admin-mode messages: %w", err)
	}
	return nil
}

// lockParticipantChatsTx locks, inside DeleteUser's transaction and before it
// removes any of the account's rows, the chats in which the account has an
// approval card or an auto-submit countdown. Two writers turn those into a
// new queued message for the account while holding only the chat row — an
// approval resume (ClaimApprovalResume) and a countdown that fires
// (sweepOneUIAutoSubmit) — so with the chat locked here first such a message
// either commits before the deletion reads the queue (and is removed with
// the rest) or is written after it, when the cards and countdowns it comes
// from are gone. New ones cannot appear meanwhile: creating either takes the
// account-row lock this transaction holds.
func lockParticipantChatsTx(ctx context.Context, tx *sql.Tx, email string) error {
	if _, err := tx.ExecContext(ctx,
		`SELECT 1 FROM chat.conversations
		  WHERE id IN (SELECT conversation_id FROM chat.approvals WHERE user_email = $1
		               UNION SELECT conversation_id FROM chat.ui_autosubmits WHERE user_email = $1)
		  ORDER BY id
		  FOR UPDATE`, email); err != nil {
		return fmt.Errorf("lock the account's chats: %w", err)
	}
	return nil
}

// deleteAccountKeyedRowsTx removes, inside DeleteUser's transaction, the rows
// that name the account by email and that nothing else in DeleteUser reaches:
// tables with no foreign key to core.users, so neither the users row nor the
// conversations it owned take them along. Each would otherwise be read as the
// account's by a later account created under the same email (#563).
//
// email is DeleteUser's normalized address (normalizeEmail), and every table
// here stores the address normalized the same way, so each statement is an
// exact match on both sides.
func deleteAccountKeyedRowsTx(ctx context.Context, tx *sql.Tx, email string) error {
	for _, st := range []struct {
		what string
		sql  string
		args []any
	}{
		// The Autonomous-mode risk consent: a later account must accept it
		// itself (GetAutonomousAcceptance matches user_email exactly, as
		// written by the same normaliser).
		{"Autonomous-mode acceptance",
			`DELETE FROM chat.autonomous_acceptances WHERE user_email = $1`, []any{email}},
		// The browsers the account turned notifications on in. The rows have
		// no foreign key and are otherwise removed per endpoint only.
		{"push subscriptions",
			`DELETE FROM chat.push_subscriptions WHERE user_email = $1`, []any{email}},
		// Connectors other people shared with the account. Grants on the
		// account's own connectors go with them (ON DELETE CASCADE).
		{"connector share grants",
			`DELETE FROM chat.remote_mcp_shares WHERE grantee = $1`, []any{email}},
		// Connector sign-ins the account started on a connector it did not
		// own; those on its own connectors cascade with them.
		{"connector sign-in flows",
			`DELETE FROM chat.remote_mcp_oauth_flow WHERE user_email = $1`, []any{email}},
		// Auto-submit countdowns the account armed in other people's chats.
		{"auto-submit countdowns",
			`DELETE FROM chat.ui_autosubmits WHERE user_email = $1`, []any{email}},
		// The memory graph (legacy/chat/030). Relations also cascade from the
		// memories and entities they name; entities have no parent at all.
		{"memory relations",
			`DELETE FROM chat.memory_relations WHERE user_email = $1`, []any{email}},
		{"memory entities",
			`DELETE FROM chat.memory_entities WHERE user_email = $1`, []any{email}},
		// Central sign-in session generations: a fleet session minted from a
		// central Auth sign-in carries the generation of its (issuer,
		// subject), and is admitted for whichever account holds the email.
		// Without the row the claim matches nothing, so those sessions end
		// here; the next central sign-in starts a new generation.
		{"central sign-in sessions",
			`DELETE FROM core.external_auth_epochs WHERE email = $1`, []any{email}},
		// Prompt library (docs/PROMPT-LIBRARY.md): a private prompt is seen
		// only by its author and goes with the account. A workspace-shared one
		// stays for the people reading it, owned by no account from here on.
		{"private prompts",
			`DELETE FROM sched.prompt_library WHERE owner_username = $1 AND visibility = 'private'`, []any{email}},
		{"workspace-shared prompts",
			`UPDATE sched.prompt_library SET owner_username = $2 || id::text WHERE owner_username = $1`,
			[]any{email, models.ReleasedPromptOwnerPrefix}},
	} {
		if _, err := tx.ExecContext(ctx, st.sql, st.args...); err != nil {
			return fmt.Errorf("delete %s: %w", st.what, err)
		}
	}
	return nil
}

// lockAccountTx takes, first in the caller's transaction, a key-share lock on
// the account row of email, and returns ErrUserNotFound when the address has
// no account. Every write of a row that DeleteUser removes by the account's
// address takes it (directly, through withAccountRow, or through
// lockWriterAccountTx / lockAndVerifyOwnedOrMultiplayerTx where a chat's
// owner may have no account): DeleteUser locks the same row FOR UPDATE before
// anything else, so such a write either commits before the deletion starts —
// and the deletion removes the row — or waits for the deletion and is
// refused. A later account under the same email therefore never finds a row
// written for the deleted one. TestAccountKeyedWritersTakeTheAccountLock
// keeps new writers on it.
func lockAccountTx(ctx context.Context, tx *sql.Tx, email string) error {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM core.users WHERE email = $1 FOR KEY SHARE`, normalizeEmail(email)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUserNotFound
	}
	return err
}

// withAccountRow runs fn in a transaction that first takes the account-row
// lock (lockAccountTx), and returns ErrUserNotFound without running fn when
// the account does not exist. The account-keyed writes that need no other
// transaction go through it — settings, subscriptions, consents, and the
// memory, skill and persona proposals a turn makes after it has finished — so
// a request or turn that outlives its account's deletion cannot re-create the
// account's rows.
func (s *Store) withAccountRow(ctx context.Context, email string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockAccountTx(ctx, tx, email); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// lockWriterAccountTx takes, first in the caller's transaction, a key-share
// lock on the account row of the address a participant write is made for,
// then refuses the write when that address has no account and is not the
// chat's owner (a chat a webhook started may belong to an address with no
// account; its own writes stay allowed). DeleteUser locks the same row FOR
// UPDATE before it touches any chat, so such a write either commits before
// the deletion starts — and the deletion removes it — or waits for it and is
// refused. Take it before any chat row lock.
func lockWriterAccountTx(ctx context.Context, tx *sql.Tx, email, convID string) error {
	email = normalizeEmail(email)
	err := lockAccountTx(ctx, tx, email)
	if !errors.Is(err, ErrUserNotFound) {
		return err
	}
	var owner string
	err = tx.QueryRowContext(ctx, `SELECT user_email FROM chat.conversations WHERE id = $1`, convID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConversationNotFound
	}
	if err != nil {
		return err
	}
	if normalizeEmail(owner) != email {
		return ErrConversationNotFound
	}
	return nil
}

// DeleteUserPreflight reports whether DeleteUser would delete email now:
// ErrUserNotFound, an *OwnsSharedProjectsError, or nil. It changes nothing
// and decides nothing — DeleteUser checks again in its own transaction — but
// lets the server refuse before it stops the account's running work.
func (s *Store) DeleteUserPreflight(ctx context.Context, email string) error {
	email = normalizeEmail(email)
	// Rolled back, never committed: the guard query takes row locks
	// (teamSharedProjectsOwnedByTx), which last only this long.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM core.users WHERE email = $1`, email).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUserNotFound
	}
	if err != nil {
		return err
	}
	shared, err := teamSharedProjectsOwnedByTx(ctx, tx, email)
	if err != nil {
		return err
	}
	if len(shared) > 0 {
		return &OwnsSharedProjectsError{Projects: shared}
	}
	return nil
}

// externalSessionLifetime is the longest a fleet session minted from a central
// sign-in lives (websession.AbsoluteSeconds, ADR-0064). A central sign-in
// session generation not used for longer can be carried by no live session.
const externalSessionLifetime = 24 * time.Hour

// SweepDeletedAccountRows removes rows that name an address with no account,
// in tables that only a signed-in account writes: what accounts deleted
// before deletion reached these tables left behind, and anything written for
// an account after it was deleted (a turn or request still in flight, a
// deletion from the CLI while the server runs). The hourly maintenance pass
// runs it.
// It returns how many rows it removed.
//
// Only addresses stored in their normalized form are considered (the store's
// normalizeEmail, as every account is keyed): a value in any other form names no
// account by construction, and the identity merge left some on purpose — a
// losing duplicate username's prompts stay under that username, quarantined
// (docs/IDENTITY.md, "The merge") — so they are never taken for a deleted
// account's.
//
// Live queued messages, pending approvals and live chat schedules are swept
// only where the address is not the chat's owner: a participant in someone else's chat is
// always an account, while a chat a webhook started belongs to the trigger's
// address, which may have none. Central sign-in session generations are
// removed only once no session minted from them can still be alive: a person
// signed in through central Auth before their account exists keeps that
// session when the account is then created. Private prompts owned by an API
// key ("apikey:…"), the admin key ("admin") or no account (models.
// ReleasedPromptOwnerPrefix, which are workspace-shared) are not touched.
// Connector share grants are not swept: granting a connector to an address
// before its account exists is allowed.
func (s *Store) SweepDeletedAccountRows(ctx context.Context) (int64, error) {
	const noAccount = `NOT EXISTS (SELECT 1 FROM core.users u WHERE u.email = %s)`
	var total int64
	// Tables keyed by the address alone: find the addresses that name no
	// account, keep those already in the normalizer's form (normalizeEmail,
	// computed here rather than approximated in SQL), and delete their rows,
	// checking again at delete time that there is still no account.
	for _, st := range []struct {
		what, table, col, extra string
		args                    []any
	}{
		{"Autonomous-mode acceptances", "chat.autonomous_acceptances", "user_email", "", nil},
		{"push subscriptions", "chat.push_subscriptions", "user_email", "", nil},
		{"auto-submit countdowns", "chat.ui_autosubmits", "user_email", "", nil},
		{"memory relations", "chat.memory_relations", "user_email", "", nil},
		{"memory entities", "chat.memory_entities", "user_email", "", nil},
		{"central sign-in session generations", "core.external_auth_epochs", "email",
			"t.updated_at < $2", []any{time.Now().Add(-externalSessionLifetime).Unix()}},
		{"private prompts", "sched.prompt_library", "owner_username",
			`t.visibility = 'private' AND t.owner_username <> 'admin' AND t.owner_username NOT LIKE 'apikey:%' AND left(t.owner_username, length($2)) <> $2`,
			[]any{models.ReleasedPromptOwnerPrefix}},
	} {
		where := fmt.Sprintf(noAccount, "t."+st.col)
		if st.extra != "" {
			where = st.extra + " AND " + where
		}
		// Placeholder $1 is the candidate list in the DELETE; the SELECT uses
		// the same numbering so the extra condition reads the same, with a
		// dummy $1.
		emails, err := s.normalizedAddresses(ctx,
			`SELECT DISTINCT t.`+st.col+` FROM `+st.table+` t WHERE `+where+` AND $1::text[] IS NOT NULL`,
			append([]any{[]string{}}, st.args...)...)
		if err != nil {
			return total, fmt.Errorf("sweep %s: %w", st.what, err)
		}
		if len(emails) == 0 {
			continue
		}
		del := `DELETE FROM ` + st.table + ` t WHERE t.` + st.col + ` = ANY($1) AND ` + where //nolint:gosec // G202: table, column and conditions from the fixed list above; values are bound
		res, err := s.db.ExecContext(ctx, del, append([]any{emails}, st.args...)...)
		if err != nil {
			return total, fmt.Errorf("sweep %s: %w", st.what, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	// A participant's rows in someone else's chat: the address is compared
	// with the chat owner's in the normalizer's form on both sides (a chat a
	// webhook started stores its owner as configured), so the owner's own
	// rows are never taken for a participant's.
	//
	// Only live rows are read — a waiting message, a pending card, a live
	// schedule: what a later account would meet. Finished ones are the chat's
	// history (and the queue's retention removes its own), and each live state
	// has a partial index, so the pass reads little.
	for _, st := range []struct{ what, table, live string }{
		{"queued messages in other people's chats", "chat.chat_input_queue", "t.state IN ('queued', 'running')"},
		{"approvals in other people's chats", "chat.approvals", "t.status = 'pending'"},
		{"schedules in other people's chats", "sched.chat_schedules", "t.status IN ('active', 'paused')"},
	} {
		sel := `SELECT DISTINCT t.user_email, c.user_email FROM ` + st.table + ` t JOIN chat.conversations c ON c.id = t.conversation_id WHERE ` + st.live + ` AND ` + fmt.Sprintf(noAccount, "t.user_email") //nolint:gosec // G202: table and state from the fixed list above
		rows, err := s.db.QueryContext(ctx, sel)
		if err != nil {
			return total, fmt.Errorf("sweep %s: %w", st.what, err)
		}
		type pair struct{ user, owner string }
		var pairs []pair
		for rows.Next() {
			var p pair
			if err := rows.Scan(&p.user, &p.owner); err != nil {
				_ = rows.Close()
				return total, fmt.Errorf("sweep %s: %w", st.what, err)
			}
			if p.user != "" && normalizeEmail(p.user) == p.user && normalizeEmail(p.owner) != p.user {
				pairs = append(pairs, p)
			}
		}
		if err := rows.Close(); err != nil {
			return total, fmt.Errorf("sweep %s: %w", st.what, err)
		}
		del := `DELETE FROM ` + st.table + ` t USING chat.conversations c WHERE c.id = t.conversation_id AND t.user_email = $1 AND c.user_email = $2 AND ` + st.live + ` AND ` + fmt.Sprintf(noAccount, "t.user_email") //nolint:gosec // G202: table and state from the fixed list above; values are bound
		for _, p := range pairs {
			res, err := s.db.ExecContext(ctx, del, p.user, p.owner)
			if err != nil {
				return total, fmt.Errorf("sweep %s: %w", st.what, err)
			}
			n, _ := res.RowsAffected()
			total += n
		}
	}
	return total, nil
}

// normalizedAddresses runs q (one text column) and returns the values already
// in the normalizer's form (normalizeEmail(v) == v), the only ones an account
// can have.
func (s *Store) normalizedAddresses(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		if v != "" && normalizeEmail(v) == v {
			out = append(out, v)
		}
	}
	return out, rows.Err()
}
