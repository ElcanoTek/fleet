package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestWebhookDeliveryClaimLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c, err := s.ClaimWebhookDelivery(ctx, "gh", "github:a")
	if err != nil || !c.Claimed || c.Token == "" {
		t.Fatalf("first claim = %+v, %v; want claimed with a token", c, err)
	}
	tok := c.Token
	// Unfinished: a repeat is refused with no conversation yet.
	if c, err = s.ClaimWebhookDelivery(ctx, "gh", "github:a"); err != nil || c.Claimed || c.ConversationID != "" {
		t.Fatalf("repeat of an unfinished claim = %+v, %v; want refused, no conversation", c, err)
	}
	// The same key under another slug is a different delivery.
	other, err := s.ClaimWebhookDelivery(ctx, "other", "github:a")
	if err != nil || !other.Claimed {
		t.Fatalf("same key, other slug = %+v, %v; want claimed", other, err)
	}

	// A wrong token binds nothing and creates no conversation.
	if _, err := s.CreateWebhookConversation(ctx, "gh", "github:a", "not-the-token", "u@x.com", "t", "victoria", "", false); !errors.Is(err, ErrWebhookClaimLost) {
		t.Fatalf("bind with a wrong token: err = %v, want ErrWebhookClaimLost", err)
	}
	conv, err := s.CreateWebhookConversation(ctx, "gh", "github:a", tok, "u@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "u@x.com", conv.ID); got == nil {
		t.Fatal("the bound conversation was not created")
	}
	if n := countConversations(t, s, "u@x.com"); n != 1 {
		t.Fatalf("conversations = %d, want 1", n)
	}
	if c, err = s.ClaimWebhookDelivery(ctx, "gh", "github:a"); err != nil || c.Claimed || c.ConversationID != conv.ID {
		t.Fatalf("repeat of an accepted delivery = %+v, %v; want refused with %s", c, err, conv.ID)
	}
	// Binding twice is refused: the claim is complete.
	if _, err := s.CreateWebhookConversation(ctx, "gh", "github:a", tok, "u@x.com", "t", "victoria", "", false); !errors.Is(err, ErrWebhookClaimLost) {
		t.Fatalf("second bind: err = %v, want ErrWebhookClaimLost", err)
	}
	// Release only drops unfinished claims.
	if err := s.ReleaseWebhookDelivery(ctx, "gh", "github:a", tok); err != nil {
		t.Fatal(err)
	}
	if c, _ = s.ClaimWebhookDelivery(ctx, "gh", "github:a"); c.Claimed {
		t.Fatal("release removed an accepted delivery")
	}
	// Release needs the token.
	if err := s.ReleaseWebhookDelivery(ctx, "other", "github:a", "not-the-token"); err != nil {
		t.Fatal(err)
	}
	if c, _ = s.ClaimWebhookDelivery(ctx, "other", "github:a"); c.Claimed {
		t.Fatal("a release with the wrong token dropped the claim")
	}
	if err := s.ReleaseWebhookDelivery(ctx, "other", "github:a", other.Token); err != nil {
		t.Fatal(err)
	}
	if c, _ = s.ClaimWebhookDelivery(ctx, "other", "github:a"); !c.Claimed {
		t.Fatal("a released claim could not be claimed again")
	}

	// Abandon removes the record and the conversation, with the right token only.
	if err := s.AbandonWebhookConversation(ctx, "gh", "github:a", "not-the-token", conv.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "u@x.com", conv.ID); got == nil {
		t.Fatal("abandon with the wrong token deleted the conversation")
	}
	if err := s.AbandonWebhookConversation(ctx, "gh", "github:a", tok, conv.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "u@x.com", conv.ID); got != nil {
		t.Fatal("abandon left the conversation behind")
	}
	if c, _ = s.ClaimWebhookDelivery(ctx, "gh", "github:a"); !c.Claimed {
		t.Fatal("an abandoned delivery could not be claimed again")
	}
}

// A request that outlives its claim: a repeat takes the stale claim over under
// a new token, and the original can then neither bind a conversation nor drop
// the repeat's claim.
func TestWebhookDeliveryTakeoverRotatesToken(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	orig, err := s.ClaimWebhookDelivery(ctx, "gh", "github:slow")
	if err != nil || !orig.Claimed {
		t.Fatalf("claim: %+v %v", orig, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE webhook_deliveries SET created_at = $1 WHERE delivery_key = 'github:slow'`,
		time.Now().Add(-2*webhookClaimStaleAfter).Unix()); err != nil {
		t.Fatal(err)
	}
	retry, err := s.ClaimWebhookDelivery(ctx, "gh", "github:slow")
	if err != nil || !retry.Claimed || retry.Token == orig.Token {
		t.Fatalf("takeover = %+v, %v; want claimed under a new token", retry, err)
	}

	// The original's release is a no-op...
	if err := s.ReleaseWebhookDelivery(ctx, "gh", "github:slow", orig.Token); err != nil {
		t.Fatal(err)
	}
	// ...and so is its bind: no conversation is created.
	if _, err := s.CreateWebhookConversation(ctx, "gh", "github:slow", orig.Token, "u@x.com", "t", "victoria", "", false); !errors.Is(err, ErrWebhookClaimLost) {
		t.Fatalf("stale original's bind: err = %v, want ErrWebhookClaimLost", err)
	}
	if n := countConversations(t, s, "u@x.com"); n != 0 {
		t.Fatalf("the stale original created %d conversations", n)
	}
	conv, err := s.CreateWebhookConversation(ctx, "gh", "github:slow", retry.Token, "u@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("the retry's bind after the original's release: %v", err)
	}
	if c, _ := s.ClaimWebhookDelivery(ctx, "gh", "github:slow"); c.Claimed || c.ConversationID != conv.ID {
		t.Fatalf("delivery record = %+v, want bound to the retry's %s", c, conv.ID)
	}
}

func countConversations(t *testing.T, s *Store, user string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM conversations WHERE user_email = $1`, user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
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
