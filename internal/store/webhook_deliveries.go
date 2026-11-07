package store

import (
	"context"
	"database/sql"
	"errors"
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

// WebhookClaim is the result of ClaimWebhookDelivery.
type WebhookClaim struct {
	// Claimed is true when this caller now owns the delivery and should start
	// the conversation (then SetWebhookDeliveryConversation, or
	// ReleaseWebhookDelivery if it gives up).
	Claimed bool
	// ConversationID is, when Claimed is false, the conversation an earlier
	// request started for the same delivery — "" while that request is still
	// creating it.
	ConversationID string
}

// ClaimWebhookDelivery atomically claims (slug, deliveryKey). A row older than
// WebhookDeliveryTTL, or an unfinished claim older than a minute, is taken
// over as if absent. Concurrent claims of one key serialize on the primary
// key: exactly one of them gets Claimed.
func (s *Store) ClaimWebhookDelivery(ctx context.Context, slug, deliveryKey string) (WebhookClaim, error) {
	now := time.Now()
	var one int
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO webhook_deliveries (slug, delivery_key, conversation_id, created_at)
		 VALUES ($1, $2, '', $3)
		 ON CONFLICT (slug, delivery_key) DO UPDATE
		    SET conversation_id = '', created_at = EXCLUDED.created_at
		  WHERE webhook_deliveries.created_at < $4
		     OR (webhook_deliveries.conversation_id = '' AND webhook_deliveries.created_at < $5)
		 RETURNING 1`,
		slug, deliveryKey, now.Unix(),
		now.Add(-WebhookDeliveryTTL).Unix(), now.Add(-webhookClaimStaleAfter).Unix(),
	).Scan(&one)
	if err == nil {
		return WebhookClaim{Claimed: true}, nil
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

// SetWebhookDeliveryConversation records the conversation a claimed delivery
// started, completing the claim.
func (s *Store) SetWebhookDeliveryConversation(ctx context.Context, slug, deliveryKey, convID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webhook_deliveries SET conversation_id = $3 WHERE slug = $1 AND delivery_key = $2`,
		slug, deliveryKey, convID)
	return err
}

// ReleaseWebhookDelivery drops an unfinished claim, so a repeat of a delivery
// that was not accepted (throttled, refused, failed) is processed afresh.
func (s *Store) ReleaseWebhookDelivery(ctx context.Context, slug, deliveryKey string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM webhook_deliveries WHERE slug = $1 AND delivery_key = $2 AND conversation_id = ''`,
		slug, deliveryKey)
	return err
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
