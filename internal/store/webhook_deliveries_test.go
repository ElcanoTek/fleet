package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
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
	if _, err := s.CreateWebhookConversation(ctx, "gh", "github:a", "not-the-token", "", "u@x.com", "t", "victoria", "", false); !errors.Is(err, ErrWebhookClaimLost) {
		t.Fatalf("bind with a wrong token: err = %v, want ErrWebhookClaimLost", err)
	}
	conv, err := s.CreateWebhookConversation(ctx, "gh", "github:a", tok, "", "u@x.com", "t", "victoria", "", false)
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
	if _, err := s.CreateWebhookConversation(ctx, "gh", "github:a", tok, "", "u@x.com", "t", "victoria", "", false); !errors.Is(err, ErrWebhookClaimLost) {
		t.Fatalf("second bind: err = %v, want ErrWebhookClaimLost", err)
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
	c, _ = s.ClaimWebhookDelivery(ctx, "gh", "github:a")
	if !c.Claimed {
		t.Fatal("an abandoned delivery could not be claimed again")
	}

	// Completion comes with the turn's user message; a completed delivery is
	// a duplicate, and neither release nor abandon touches it.
	conv, err = s.CreateWebhookConversation(ctx, "gh", "github:a", c.Token, "", "u@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTurn(ctx, "turn-a", conv.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitWebhookUserMessage(ctx, "gh", "github:a", c.Token, conv.ID, "turn-a", textEntry("user", "prompt")); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseWebhookDelivery(ctx, "gh", "github:a", c.Token); err != nil {
		t.Fatal(err)
	}
	if err := s.AbandonWebhookConversation(ctx, "gh", "github:a", c.Token, conv.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ClaimWebhookDelivery(ctx, "gh", "github:a"); got.Claimed || !got.Completed || got.ConversationID != conv.ID {
		t.Fatalf("a completed delivery = %+v; want a duplicate of %s", got, conv.ID)
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
	if _, err := s.CreateWebhookConversation(ctx, "gh", "github:slow", orig.Token, "", "u@x.com", "t", "victoria", "", false); !errors.Is(err, ErrWebhookClaimLost) {
		t.Fatalf("stale original's bind: err = %v, want ErrWebhookClaimLost", err)
	}
	if n := countConversations(t, s, "u@x.com"); n != 0 {
		t.Fatalf("the stale original created %d conversations", n)
	}
	conv, err := s.CreateWebhookConversation(ctx, "gh", "github:slow", retry.Token, "", "u@x.com", "t", "victoria", "", false)
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
			`INSERT INTO webhook_deliveries (slug, delivery_key, conversation_id, completed_at, created_at)
			 VALUES ('gh', $1, $2, CASE WHEN $2 = '' THEN 0 ELSE $3::bigint END, $3)`,
			row.key, row.conv, now.Add(-row.age).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	if c, _ := s.ClaimWebhookDelivery(ctx, "gh", "fresh"); c.Claimed || !c.Completed || c.ConversationID != "conv-new" {
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

// A delivery is a duplicate only once its turn's input is durable: a request
// that bound a conversation, created its turn row and then died before the
// user message committed leaves the delivery unfinished; a retry after the
// stale window takes it over, gets the bound conversation back to reuse, and
// completes it with its own turn's user message. The dead request's token can
// complete nothing.
func TestWebhookDeliveryCrashBeforeTurnIsRetryable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	orig, err := s.ClaimWebhookDelivery(ctx, "gh", "github:crash")
	if err != nil || !orig.Claimed {
		t.Fatalf("claim: %+v %v", orig, err)
	}
	conv, err := s.CreateWebhookConversation(ctx, "gh", "github:crash", orig.Token, "", "u@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	// "Crash" after the turn row, before the user message.
	if err := s.CreateTurn(ctx, "turn-orig", conv.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	// A repeat inside the window is refused as in progress, not answered as
	// a duplicate.
	if c, _ := s.ClaimWebhookDelivery(ctx, "gh", "github:crash"); c.Claimed || c.Completed {
		t.Fatalf("repeat of a bound, uncompleted delivery = %+v; want refused, not completed", c)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE webhook_deliveries SET created_at = $1 WHERE delivery_key = 'github:crash'`,
		time.Now().Add(-2*webhookClaimStaleAfter).Unix()); err != nil {
		t.Fatal(err)
	}
	retry, err := s.ClaimWebhookDelivery(ctx, "gh", "github:crash")
	if err != nil || !retry.Claimed || retry.Token == orig.Token || retry.ConversationID != conv.ID {
		t.Fatalf("takeover = %+v, %v; want claimed under a new token, reusing %s", retry, err, conv.ID)
	}
	if _, err := s.CommitWebhookUserMessage(ctx, "gh", "github:crash", orig.Token, conv.ID, "turn-orig", textEntry("user", "prompt")); !errors.Is(err, ErrWebhookClaimLost) {
		t.Fatalf("stale token's user message: err = %v, want ErrWebhookClaimLost", err)
	}
	if err := s.CreateTurn(ctx, "turn-retry", conv.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitWebhookUserMessage(ctx, "gh", "github:crash", retry.Token, conv.ID, "turn-retry", textEntry("user", "prompt")); err != nil {
		t.Fatal(err)
	}
	hist, err := s.LoadHistory(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Fatalf("history = %d entries, want exactly the retry's user message", len(hist))
	}
	if n := countConversations(t, s, "u@x.com"); n != 1 {
		t.Fatalf("conversations = %d, want 1 (the bound one reused)", n)
	}
	// Completed: a repeat is a duplicate, even after the stale window.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE webhook_deliveries SET created_at = $1 WHERE delivery_key = 'github:crash'`,
		time.Now().Add(-2*webhookClaimStaleAfter).Unix()); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.ClaimWebhookDelivery(ctx, "gh", "github:crash"); c.Claimed || !c.Completed || c.ConversationID != conv.ID {
		t.Fatalf("repeat of a completed delivery = %+v; want a duplicate of %s", c, conv.ID)
	}
}

// A reused conversation that no longer exists is replaced: the bind names the
// conversation the claim currently holds.
func TestWebhookDeliveryRebindsAMissingConversation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c, _ := s.ClaimWebhookDelivery(ctx, "gh", "github:gone")
	conv, err := s.CreateWebhookConversation(ctx, "gh", "github:gone", c.Token, "", "u@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE id = $1`, conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWebhookConversation(ctx, "gh", "github:gone", c.Token, "", "u@x.com", "t", "victoria", "", false); !errors.Is(err, ErrWebhookClaimLost) {
		t.Fatalf("bind with the wrong previous conversation: err = %v, want ErrWebhookClaimLost", err)
	}
	conv2, err := s.CreateWebhookConversation(ctx, "gh", "github:gone", c.Token, conv.ID, "u@x.com", "t", "victoria", "", false)
	if err != nil || conv2.ID == conv.ID {
		t.Fatalf("rebind = %v, %v; want a new conversation", conv2, err)
	}
}

// Abandon removes a conversation only while it is empty: one that has
// messages (or a turn) is kept, and only the claim is released.
func TestWebhookAbandonKeepsAConversationWithContent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		content func(convID string)
		kept    bool
	}{
		{"empty", func(string) {}, false},
		{"with a message", func(convID string) {
			if _, err := s.AppendHistory(ctx, convID, []agent.HistoryEntry{textEntry("user", "mine now")}); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"with a turn", func(convID string) {
			if err := s.CreateTurn(ctx, "turn-"+convID, convID, time.Now().Unix()); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		key := "github:" + tc.name
		c, err := s.ClaimWebhookDelivery(ctx, "gh", key)
		if err != nil || !c.Claimed {
			t.Fatalf("%s: claim %+v %v", tc.name, c, err)
		}
		conv, err := s.CreateWebhookConversation(ctx, "gh", key, c.Token, "", "u@x.com", "t", "victoria", "", false)
		if err != nil {
			t.Fatal(err)
		}
		tc.content(conv.ID)
		if err := s.AbandonWebhookConversation(ctx, "gh", key, c.Token, conv.ID); err != nil {
			t.Fatalf("%s: abandon: %v", tc.name, err)
		}
		got, _ := s.Get(ctx, "u@x.com", conv.ID)
		if (got != nil) != tc.kept {
			t.Fatalf("%s: conversation kept = %v, want %v", tc.name, got != nil, tc.kept)
		}
		if c, _ := s.ClaimWebhookDelivery(ctx, "gh", key); !c.Claimed || c.ConversationID != "" {
			t.Fatalf("%s: after abandon the delivery should be claimable afresh, got %+v", tc.name, c)
		}
	}
}

// Releasing a claim that inherited a conversation through a takeover drops
// the claim and leaves the conversation alone.
func TestWebhookReleaseLeavesAnInheritedConversation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	orig, _ := s.ClaimWebhookDelivery(ctx, "gh", "github:inherit")
	conv, err := s.CreateWebhookConversation(ctx, "gh", "github:inherit", orig.Token, "", "u@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE webhook_deliveries SET created_at = $1 WHERE delivery_key = 'github:inherit'`,
		time.Now().Add(-2*webhookClaimStaleAfter).Unix()); err != nil {
		t.Fatal(err)
	}
	retry, _ := s.ClaimWebhookDelivery(ctx, "gh", "github:inherit")
	if !retry.Claimed || retry.ConversationID != conv.ID {
		t.Fatalf("takeover = %+v", retry)
	}
	if err := s.ReleaseWebhookDelivery(ctx, "gh", "github:inherit", retry.Token); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "u@x.com", conv.ID); got == nil {
		t.Fatal("release deleted the inherited conversation")
	}
	if c, _ := s.ClaimWebhookDelivery(ctx, "gh", "github:inherit"); !c.Claimed || c.ConversationID != "" {
		t.Fatalf("after release the delivery should be claimable afresh, got %+v", c)
	}
}
