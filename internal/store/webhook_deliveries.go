package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// Webhook delivery idempotency (docs/WEBHOOKS.md, migration 098). Senders
// deliver at least once — Slack retries an event it did not see answered in
// time, and GitHub's "Redeliver" re-sends a delivery under the same id — so
// POST /webhooks/{slug} claims each delivery before it starts a conversation
// and answers a repeat with the conversation the first one started.
//
// The table lives in the chat database rather than process memory because
// every green push to main is a release: a restart inside a sender's retry
// window would otherwise forget what it had already accepted.

// WebhookDeliveryTTL is how long an accepted delivery is remembered.
const WebhookDeliveryTTL = 24 * time.Hour

// webhookClaimStaleAfter bounds an unfinished row (claimed or bound, not yet
// completed). The claiming request gets from claim to a durable turn input
// within seconds (the webhook handler waits for it, bounded at 10 s);
// a row unfinished for longer belongs to a request that died or stalled, and a
// repeat may take it over rather than be refused for the rest of the TTL.
const webhookClaimStaleAfter = time.Minute

// ErrWebhookClaimLost is returned by CreateWebhookConversation,
// CommitWebhookUserMessage and AbandonWebhookConversation when the caller no
// longer holds the delivery claim: it went stale and a repeat took it over
// (rotating the token). The caller must not start, or answer for, a turn.
var ErrWebhookClaimLost = errors.New("webhook delivery claim is no longer held")

// WebhookClaim is the result of ClaimWebhookDelivery.
type WebhookClaim struct {
	// Claimed is true when this caller now owns the delivery: it creates the
	// conversation (CreateWebhookConversation) and the turn's
	// CommitWebhookUserMessage completes the delivery — or it gives the claim
	// back (ReleaseWebhookDelivery / AbandonWebhookConversation).
	Claimed bool
	// Token proves ownership of a claim; every later call on it needs it.
	Token string
	// ConversationID and Completed describe, when not Claimed, the earlier
	// request for the same delivery: its conversation ("" until created) and
	// whether its turn's input is durable — only then is this a duplicate.
	ConversationID string
	Completed      bool
}

// ClaimWebhookDelivery atomically claims (slug, deliveryKey). A completed row
// older than WebhookDeliveryTTL is a new delivery; an unfinished row older
// than a minute is taken over under a new token, so the request that held it
// can no longer act on it. A takeover always starts afresh: the conversation
// the stale row named (which, its input never having committed, holds nothing
// of the delivery) is deleted in the same transaction if it is still empty,
// and otherwise left alone. The new owner creates its conversation under the
// current settings like any first delivery. Concurrent claims of one key
// serialize on the row: exactly one of them gets Claimed.
func (s *Store) ClaimWebhookDelivery(ctx context.Context, slug, deliveryKey string) (WebhookClaim, error) {
	token, err := newClaimToken()
	if err != nil {
		return WebhookClaim{}, err
	}
	// A row can vanish between the insert and the read (released, swept);
	// the next attempt then inserts it.
	for range 3 {
		claim, retry, err := s.claimWebhookDeliveryOnce(ctx, slug, deliveryKey, token)
		if !retry {
			return claim, err
		}
	}
	return WebhookClaim{}, errors.New("webhook delivery claim: row kept changing")
}

func (s *Store) claimWebhookDeliveryOnce(ctx context.Context, slug, deliveryKey, token string) (claim WebhookClaim, retry bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WebhookClaim{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO webhook_deliveries (slug, delivery_key, conversation_id, claim_token, completed_at, created_at)
		 VALUES ($1, $2, '', $3, 0, $4)
		 ON CONFLICT (slug, delivery_key) DO NOTHING`,
		slug, deliveryKey, token, now.Unix())
	if err != nil {
		return WebhookClaim{}, false, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return WebhookClaim{}, false, err
	} else if n == 1 {
		return WebhookClaim{Claimed: true, Token: token}, false, tx.Commit()
	}

	var (
		convID                 string
		completedAt, createdAt int64
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT conversation_id, completed_at, created_at FROM webhook_deliveries
		  WHERE slug = $1 AND delivery_key = $2 FOR UPDATE`,
		slug, deliveryKey,
	).Scan(&convID, &completedAt, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WebhookClaim{}, true, nil
		}
		return WebhookClaim{}, false, err
	}
	completed := completedAt != 0
	takeover := (completed && createdAt < now.Add(-WebhookDeliveryTTL).Unix()) ||
		(!completed && createdAt < now.Add(-webhookClaimStaleAfter).Unix())
	if !takeover {
		return WebhookClaim{ConversationID: convID, Completed: completed}, false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE webhook_deliveries
		    SET conversation_id = '', claim_token = $3, completed_at = 0, created_at = $4
		  WHERE slug = $1 AND delivery_key = $2`,
		slug, deliveryKey, token, now.Unix()); err != nil {
		return WebhookClaim{}, false, err
	}
	if !completed && convID != "" {
		// The stale claimant's own turn row, if any, no longer counts: a
		// claimant that crashed left it 'running' only until startup
		// recovery finished it, and that runs before traffic. A turn that is
		// still running belongs to a live owner and keeps the conversation.
		if err := discardEmptyConversation(ctx, tx, convID, "", true); err != nil {
			return WebhookClaim{}, false, err
		}
	}
	return WebhookClaim{Claimed: true, Token: token}, false, tx.Commit()
}

// CreateWebhookConversation creates the conversation for a claimed delivery
// and binds it to the claim in ONE transaction, so a conversation never exists
// for a delivery whose row does not name it. The claim row is locked first: a
// concurrent takeover waits for this commit. Returns ErrWebhookClaimLost,
// creating nothing, when the token no longer holds an unbound claim.
func (s *Store) CreateWebhookConversation(ctx context.Context, slug, deliveryKey, token, userEmail, title, persona, model string, lockdown bool) (*Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	id := newConversationID(ctx)
	res, err := tx.ExecContext(ctx,
		`UPDATE webhook_deliveries SET conversation_id = $4
		  WHERE slug = $1 AND delivery_key = $2 AND claim_token = $3
		    AND conversation_id = '' AND completed_at = 0`,
		slug, deliveryKey, token, id)
	if err != nil {
		return nil, fmt.Errorf("bind webhook delivery: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, ErrWebhookClaimLost
	}
	conv, err := insertConversation(ctx, tx, id, userEmail, title, persona, model, lockdown)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return conv, nil
}

// CommitWebhookUserMessage is CommitUserMessage for a webhook turn: it
// persists the turn's user entry (the rendered trigger prompt) and marks the
// delivery completed, in ONE transaction. Completion is what makes a repeat a
// duplicate, so it must not precede a durable input: before this commits a
// crash leaves the delivery unfinished, and a retry after the stale window
// takes it over and starts afresh; after it, the
// turn's input is in history and startup recovery owns the turn like any
// other. Returns ErrWebhookClaimLost, writing nothing, when the token no
// longer holds the claim on convID.
func (s *Store) CommitWebhookUserMessage(ctx context.Context, slug, deliveryKey, token, convID, turnID string, entry agent.HistoryEntry) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE webhook_deliveries SET completed_at = $5
		  WHERE slug = $1 AND delivery_key = $2 AND claim_token = $3
		    AND conversation_id = $4 AND completed_at = 0`,
		slug, deliveryKey, token, convID, time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("complete webhook delivery: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, err
	} else if n == 0 {
		return 0, ErrWebhookClaimLost
	}
	ids, err := s.appendHistoryProvenanceTx(ctx, tx, convID, turnID, 1, []agent.HistoryEntry{entry})
	if err != nil {
		return 0, err
	}
	return ids[0], tx.Commit()
}

// ReleaseWebhookDelivery drops a claim held by token that has no conversation
// yet, so a repeat of a delivery that was not accepted (throttled, refused,
// failed) is processed afresh. A no-op when the token no longer holds it.
func (s *Store) ReleaseWebhookDelivery(ctx context.Context, slug, deliveryKey, token string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM webhook_deliveries
		  WHERE slug = $1 AND delivery_key = $2 AND claim_token = $3
		    AND conversation_id = '' AND completed_at = 0`,
		slug, deliveryKey, token)
	return err
}

// AbandonWebhookConversation gives back a bound delivery whose turn input did
// not commit: in one transaction it drops the delivery row held by token and
// deletes the conversation the row names while it is still empty — no
// messages, and no turns other than ownTurnID (the abandoning request's own,
// which the delete then removes; "" for none). A conversation with any other
// content is left alone.
//
// It serializes with CommitWebhookUserMessage on the delivery row, so exactly
// one of them wins: if the input committed first, the delivery is accepted,
// nothing is touched, and completed is true; once this has committed, the
// input commit finds no claim and the turn runs nothing. ErrWebhookClaimLost
// when the token no longer holds the row.
func (s *Store) AbandonWebhookConversation(ctx context.Context, slug, deliveryKey, token, ownTurnID string) (completed bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var (
		convID      string
		completedAt int64
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT conversation_id, completed_at FROM webhook_deliveries
		  WHERE slug = $1 AND delivery_key = $2 AND claim_token = $3 FOR UPDATE`,
		slug, deliveryKey, token,
	).Scan(&convID, &completedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrWebhookClaimLost
		}
		return false, err
	}
	if completedAt != 0 {
		return true, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM webhook_deliveries WHERE slug = $1 AND delivery_key = $2`,
		slug, deliveryKey); err != nil {
		return false, err
	}
	if convID != "" {
		if err := discardEmptyConversation(ctx, tx, convID, ownTurnID, false); err != nil {
			return false, err
		}
	}
	return false, tx.Commit()
}

// discardEmptyConversation deletes convID inside tx only while it has no
// messages and no turns other than exceptTurnID (and, when ignoreFinished is
// set, other than turns that already reached a terminal status — a dead
// claimant's turn with no saved input). The conversation row is
// locked first, as every history writer locks it (lockConversationForAppend),
// so the emptiness check cannot race a write.
func discardEmptyConversation(ctx context.Context, tx *sql.Tx, convID, exceptTurnID string, ignoreFinished bool) error {
	if _, err := tx.ExecContext(ctx,
		`SELECT 1 FROM conversations WHERE id = $1 FOR UPDATE`, convID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`DELETE FROM conversations c
		  WHERE c.id = $1
		    AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = c.id)
		    AND NOT EXISTS (SELECT 1 FROM turns t WHERE t.conversation_id = c.id AND t.turn_id <> $2
		                      AND (NOT $3 OR t.status = 'running'))`,
		convID, exceptTurnID, ignoreFinished)
	return err
}

// newClaimToken is 128 random bits, hex-encoded.
func newClaimToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// SweepWebhookDeliveries deletes delivery records older than ttl. Part of the
// retention sweep; a non-positive ttl disables it.
func (s *Store) SweepWebhookDeliveries(ctx context.Context, ttl time.Duration) (int, error) {
	if ttl <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM webhook_deliveries WHERE created_at < $1`, time.Now().Add(-ttl).Unix())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
