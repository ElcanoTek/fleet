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

// webhookClaimStaleAfter bounds an unfinished claim (conversation_id still empty).
// The claiming request creates the conversation within milliseconds; a claim
// older than this belongs to a request that died before finishing, and a
// repeat may take it over rather than be refused for the rest of the TTL.
const webhookClaimStaleAfter = time.Minute

// ErrWebhookClaimLost is returned by CreateWebhookConversation when the
// caller no longer holds the delivery claim: it went stale and a repeat took
// it over (rotating the token), or the claim was completed or released. The
// caller must not start a turn.
var ErrWebhookClaimLost = errors.New("webhook delivery claim is no longer held")

// WebhookClaim is the result of ClaimWebhookDelivery.
type WebhookClaim struct {
	// Claimed is true when this caller now owns the delivery and should start
	// the conversation (CreateWebhookConversation), or give the claim back
	// (ReleaseWebhookDelivery) if it does not.
	Claimed bool
	// Token proves ownership of a claim; every later call on it needs it.
	Token string
	// ConversationID is, when Claimed is false, the conversation an earlier
	// request started for the same delivery — "" while that request is still
	// creating it.
	ConversationID string
}

// ClaimWebhookDelivery atomically claims (slug, deliveryKey). A row older than
// WebhookDeliveryTTL, or an unfinished claim older than a minute, is taken
// over as if absent, under a new token — so the request that held it before
// can no longer complete or release it. Concurrent claims of one key
// serialize on the primary key: exactly one of them gets Claimed.
func (s *Store) ClaimWebhookDelivery(ctx context.Context, slug, deliveryKey string) (WebhookClaim, error) {
	token, err := newClaimToken()
	if err != nil {
		return WebhookClaim{}, err
	}
	now := time.Now()
	var one int
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO webhook_deliveries (slug, delivery_key, conversation_id, claim_token, created_at)
		 VALUES ($1, $2, '', $3, $4)
		 ON CONFLICT (slug, delivery_key) DO UPDATE
		    SET conversation_id = '', claim_token = EXCLUDED.claim_token, created_at = EXCLUDED.created_at
		  WHERE webhook_deliveries.created_at < $5
		     OR (webhook_deliveries.conversation_id = '' AND webhook_deliveries.created_at < $6)
		 RETURNING 1`,
		slug, deliveryKey, token, now.Unix(),
		now.Add(-WebhookDeliveryTTL).Unix(), now.Add(-webhookClaimStaleAfter).Unix(),
	).Scan(&one)
	if err == nil {
		return WebhookClaim{Claimed: true, Token: token}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WebhookClaim{}, err
	}
	var convID string
	if err := s.db.QueryRowContext(ctx,
		`SELECT conversation_id FROM webhook_deliveries WHERE slug = $1 AND delivery_key = $2`,
		slug, deliveryKey,
	).Scan(&convID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Released between the two statements: the next repeat claims it.
			return WebhookClaim{}, nil
		}
		return WebhookClaim{}, err
	}
	return WebhookClaim{ConversationID: convID}, nil
}

// CreateWebhookConversation creates the conversation for a claimed delivery
// and binds it to the claim in ONE transaction, so there is no state in which
// a conversation exists for a delivery whose claim still reads as unfinished
// (a repeat could take that over and start a second turn). The claim row is
// locked first: a concurrent takeover waits for this commit and then finds
// the claim completed. Returns ErrWebhookClaimLost, creating nothing, when
// the token no longer holds the claim.
func (s *Store) CreateWebhookConversation(ctx context.Context, slug, deliveryKey, token, userEmail, title, persona, model string, lockdown bool) (*Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	id := newConversationID(ctx)
	res, err := tx.ExecContext(ctx,
		`UPDATE webhook_deliveries SET conversation_id = $4
		  WHERE slug = $1 AND delivery_key = $2 AND claim_token = $3 AND conversation_id = ''`,
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

// ReleaseWebhookDelivery drops an unfinished claim held by token, so a repeat
// of a delivery that was not accepted (throttled, refused, failed) is
// processed afresh. A no-op when the token no longer holds the claim.
func (s *Store) ReleaseWebhookDelivery(ctx context.Context, slug, deliveryKey, token string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM webhook_deliveries
		  WHERE slug = $1 AND delivery_key = $2 AND claim_token = $3 AND conversation_id = ''`,
		slug, deliveryKey, token)
	return err
}

// AbandonWebhookConversation undoes CreateWebhookConversation when the turn
// could not be started: it removes the delivery record held by token and
// hard-deletes the conversation it was bound to (created moments earlier, with
// no messages and never shown to anyone), in one transaction, so the sender's
// retry is processed afresh. A no-op when the token no longer holds the
// record.
func (s *Store) AbandonWebhookConversation(ctx context.Context, slug, deliveryKey, token, convID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`DELETE FROM webhook_deliveries
		  WHERE slug = $1 AND delivery_key = $2 AND claim_token = $3 AND conversation_id = $4`,
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
