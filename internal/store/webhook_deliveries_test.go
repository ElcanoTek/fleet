package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestWebhookDeliveryClaimLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c, err := s.ClaimWebhookDelivery(ctx, "gh", "github:a")
	if err != nil || !c.Claimed {
		t.Fatalf("first claim = %+v, %v; want claimed", c, err)
	}
	// Unfinished: a repeat is refused with no conversation yet.
	if c, err = s.ClaimWebhookDelivery(ctx, "gh", "github:a"); err != nil || c.Claimed || c.ConversationID != "" {
		t.Fatalf("repeat of an unfinished claim = %+v, %v; want refused, no conversation", c, err)
	}
	// The same key under another slug is a different delivery.
	if c, err = s.ClaimWebhookDelivery(ctx, "other", "github:a"); err != nil || !c.Claimed {
		t.Fatalf("same key, other slug = %+v, %v; want claimed", c, err)
	}

	if err := s.SetWebhookDeliveryConversation(ctx, "gh", "github:a", "conv-1"); err != nil {
		t.Fatal(err)
	}
	if c, err = s.ClaimWebhookDelivery(ctx, "gh", "github:a"); err != nil || c.Claimed || c.ConversationID != "conv-1" {
		t.Fatalf("repeat of an accepted delivery = %+v, %v; want refused with conv-1", c, err)
	}
	// Release only drops unfinished claims.
	if err := s.ReleaseWebhookDelivery(ctx, "gh", "github:a"); err != nil {
		t.Fatal(err)
	}
	if c, _ = s.ClaimWebhookDelivery(ctx, "gh", "github:a"); c.Claimed {
		t.Fatal("release removed an accepted delivery")
	}
	if err := s.ReleaseWebhookDelivery(ctx, "other", "github:a"); err != nil {
		t.Fatal(err)
	}
	if c, _ = s.ClaimWebhookDelivery(ctx, "other", "github:a"); !c.Claimed {
		t.Fatal("a released claim could not be claimed again")
	}
}

func TestWebhookDeliveryExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, row := range []struct {
		key, conv string
		age       time.Duration
	}{
		{"expired", "conv-old", WebhookDeliveryTTL + time.Minute},
		{"fresh", "conv-new", time.Hour},
		{"stale-claim", "", 2 * webhookClaimStaleAfter},
	} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO webhook_deliveries (slug, delivery_key, conversation_id, created_at) VALUES ('gh', $1, $2, $3)`,
			row.key, row.conv, now.Add(-row.age).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	if c, _ := s.ClaimWebhookDelivery(ctx, "gh", "fresh"); c.Claimed || c.ConversationID != "conv-new" {
		t.Fatalf("a delivery inside the window = %+v, want refused with conv-new", c)
	}
	// A claim whose request died is taken over rather than refused for a day.
	if c, _ := s.ClaimWebhookDelivery(ctx, "gh", "stale-claim"); !c.Claimed {
		t.Fatalf("a stale unfinished claim was not taken over: %+v", c)
	}

	n, err := s.SweepWebhookDeliveries(ctx, WebhookDeliveryTTL)
	if err != nil || n != 1 {
		t.Fatalf("sweep removed %d (%v), want 1", n, err)
	}
	// Past the window a repeat is a new delivery (the sweep removed it; the
	// claim's own TTL check covers a sweep that has not run yet).
	if c, _ := s.ClaimWebhookDelivery(ctx, "gh", "expired"); !c.Claimed {
		t.Fatalf("an expired delivery was not claimable: %+v", c)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE webhook_deliveries SET conversation_id = 'x', created_at = $1 WHERE delivery_key = 'fresh'`,
		now.Add(-WebhookDeliveryTTL-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.ClaimWebhookDelivery(ctx, "gh", "fresh"); !c.Claimed {
		t.Fatalf("an expired-but-unswept delivery was not claimable: %+v", c)
	}
}

// Concurrent repeats of one delivery: exactly one claims it.
func TestWebhookDeliveryConcurrentClaims(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const n = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed int
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.ClaimWebhookDelivery(ctx, "gh", "slack:Ev1")
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if c.Claimed {
				mu.Lock()
				claimed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claimed != 1 {
		t.Fatalf("%d concurrent claims succeeded, want exactly 1", claimed)
	}
}
