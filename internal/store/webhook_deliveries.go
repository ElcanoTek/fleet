package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
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
// completed). The claiming request gets from claim to a durable turn row within
// milliseconds; a row unfinished for longer belongs to a request that died or
// stalled, and a repeat may take it over rather than be refused for the rest
// of the TTL.
const webhookClaimStaleAfter = time.Minute

// ErrWebhookClaimLost is returned by CreateWebhookConversation and
// CreateWebhookTurn when the caller no longer holds the delivery claim: it went
// stale and a repeat took it over (rotating the token), or the row moved on
// without it. The caller must not start a turn.
var ErrWebhookClaimLost = errors.New("webhook delivery claim is no longer held")

// WebhookClaim is the result of ClaimWebhookDelivery.
type WebhookClaim struct {
	// Claimed is true when this caller now owns the delivery: it starts the
	// turn (CreateWebhookConversation unless ConversationID names one to
	// reuse, then CreateWebhookTurn), or gives the claim back.
	Claimed bool
	// Token proves ownership of a claim; every later call on it needs it.
	Token string
	// ConversationID is, when Claimed, a conversation an earlier request for
	// the same delivery created and bound but never started a turn in — reuse
	// it (CreateWebhookConversation only if it no longer exists). When not
	// Claimed, it is the conversation of the earlier request ("" while that
	// request has not created it yet).
	ConversationID string
	// Completed is, when not Claimed, whether the earlier request's turn is
	// durable — only then is this a duplicate of an accepted delivery.
	Completed bool
}

// ClaimWebhookDelivery atomically claims (slug, deliveryKey). A completed row
// older than WebhookDeliveryTTL is a new delivery; an unfinished row older
// than a minute is taken over under a new token — so the request that held it
// can no longer act on it — keeping any conversation it had bound, so the new
// owner starts the turn there instead of leaving it empty. Concurrent claims of
// one key serialize on the primary key: exactly one of them gets Claimed.
func (s *Store) ClaimWebhookDelivery(ctx context.Context, slug, deliveryKey string) (WebhookClaim, error) {
	token, err := newClaimToken()
	if err != nil {
		return WebhookClaim{}, err
	}
	now := time.Now()
	var convID string
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO webhook_deliveries (slug, delivery_key, conversation_id, claim_token, completed_at, created_at)
		 VALUES ($1, $2, '', $3, 0, $4)
		 ON CONFLICT (slug, delivery_key) DO UPDATE
		    SET conversation_id = CASE WHEN webhook_deliveries.completed_at = 0
		                               THEN webhook_deliveries.conversation_id ELSE '' END,
		        claim_token = EXCLUDED.claim_token,
		        completed_at = 0,
		        created_at = EXCLUDED.created_at
		  WHERE (webhook_deliveries.completed_at <> 0 AND webhook_deliveries.created_at < $5)
		     OR (webhook_deliveries.completed_at = 0 AND webhook_deliveries.created_at < $6)
		 RETURNING conversation_id`,
		slug, deliveryKey, token, now.Unix(),
		now.Add(-WebhookDeliveryTTL).Unix(), now.Add(-webhookClaimStaleAfter).Unix(),
	).Scan(&convID)
	if err == nil {
		return WebhookClaim{Claimed: true, Token: token, ConversationID: convID}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WebhookClaim{}, err
	}
	var completedAt int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT conversation_id, completed_at FROM webhook_deliveries WHERE slug = $1 AND delivery_key = $2`,
		slug, deliveryKey,
	).Scan(&convID, &completedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Released between the two statements: the next repeat claims it.
			return WebhookClaim{}, nil
		}
		return WebhookClaim{}, err
	}
	return WebhookClaim{ConversationID: convID, Completed: completedAt != 0}, nil
}

// CreateWebhookConversation creates the conversation for a claimed delivery
// and binds it to the claim in ONE transaction, so a conversation never exists
// for a delivery whose row does not name it. prevConvID is the conversation
// the claim currently names: "" for a fresh claim, or a reused conversation
// that turned out to be gone. The claim row is locked first: a concurrent
// takeover waits for this commit. Returns ErrWebhookClaimLost, creating
// nothing, when the token no longer holds the claim.
func (s *Store) CreateWebhookConversation(ctx context.Context, slug, deliveryKey, token, prevConvID, userEmail, title, persona, model string, lockdown bool) (*Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	id := newConversationID(ctx)
	res, err := tx.ExecContext(ctx,
		`UPDATE webhook_deliveries SET conversation_id = $5
		  WHERE slug = $1 AND delivery_key = $2 AND claim_token = $3
		    AND conversation_id = $4 AND completed_at = 0`,
		slug, deliveryKey, token, prevConvID, id)
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

// CreateWebhookTurn inserts the turn row for a bound delivery (as CreateTurn
// does) and marks the delivery completed, in ONE transaction. Completion is
// what makes a repeat a duplicate, so it must not precede a durable turn: a
// crash before this commit leaves the row unfinished, and a retry after the
// stale window takes it over and starts the turn in the bound conversation.
// After it commits, startup recovery owns the turn like any other. Returns
// ErrWebhookClaimLost, inserting nothing, when the token no longer holds the
// claim on convID.
func (s *Store) CreateWebhookTurn(ctx context.Context, slug, deliveryKey, token, turnID, convID string, startedAt int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE webhook_deliveries SET completed_at = $5
		  WHERE slug = $1 AND delivery_key = $2 AND claim_token = $3
		    AND conversation_id = $4 AND completed_at = 0`,
		slug, deliveryKey, token, convID, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("complete webhook delivery: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrWebhookClaimLost
	}
	if err := createTurn(ctx, tx, turnID, convID, startedAt); err != nil {
		return err
	}
	return tx.Commit()
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

// AbandonWebhookConversation undoes a bound, uncompleted delivery whose turn
// could not be started: it removes the delivery row held by token and
// hard-deletes the conversation it names (created for this delivery, with no
// turn and no messages, never answered to anyone), in one transaction, so the
// sender's retry is processed afresh. A no-op when the token no longer holds
// the row or it is completed.
func (s *Store) AbandonWebhookConversation(ctx context.Context, slug, deliveryKey, token, convID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`DELETE FROM webhook_deliveries
		  WHERE slug = $1 AND delivery_key = $2 AND claim_token = $3
		    AND conversation_id = $4 AND completed_at = 0`,
		slug, deliveryKey, token, convID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE id = $1`, convID); err != nil {
		return err
	}
	return tx.Commit()
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
