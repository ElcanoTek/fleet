package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

func bindWebhook(t *testing.T, s *Store, key, token string) *Conversation {
	t.Helper()
	conv, err := s.CreateWebhookConversation(context.Background(), "gh", key, token, "u@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("bind %s: %v", key, err)
	}
	return conv
}

// completeWebhook does what the turn does: its row, then its input commit.
func completeWebhook(t *testing.T, s *Store, key, token, convID, turnID string) error {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateTurn(ctx, turnID, convID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	_, err := s.CommitWebhookUserMessage(ctx, "gh", key, token, convID, turnID, textEntry("user", "prompt"))
	return err
}

func ageWebhook(t *testing.T, s *Store, key string, by time.Duration) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE webhook_deliveries SET created_at = $1 WHERE delivery_key = $2`,
		time.Now().Add(-by).Unix(), key); err != nil {
		t.Fatal(err)
	}
}

func conversationExists(t *testing.T, s *Store, id string) bool {
	t.Helper()
	got, err := s.Get(context.Background(), "u@x.com", id)
	if err != nil {
		t.Fatal(err)
	}
	return got != nil
}

func TestWebhookDeliveryClaimLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c, err := s.ClaimWebhookDelivery(ctx, "gh", "github:a")
	if err != nil || !c.Claimed || c.Token == "" {
		t.Fatalf("first claim = %+v, %v; want claimed with a token", c, err)
	}
	tok := c.Token
	// Unfinished: a repeat is refused, not a duplicate.
	if c, err = s.ClaimWebhookDelivery(ctx, "gh", "github:a"); err != nil || c.Claimed || c.Completed {
		t.Fatalf("repeat of an unfinished claim = %+v, %v; want refused, not completed", c, err)
	}
	// The same key under another slug is a different delivery.
	other, err := s.ClaimWebhookDelivery(ctx, "other", "github:a")
	if err != nil || !other.Claimed {
		t.Fatalf("same key, other slug = %+v, %v; want claimed", other, err)
	}
	// Release needs the token, and gives an unbound claim back.
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

	// A wrong token binds nothing and creates no conversation.
	if _, err := s.CreateWebhookConversation(ctx, "gh", "github:a", "not-the-token", "u@x.com", "t", "victoria", "", false); !errors.Is(err, ErrWebhookClaimLost) {
		t.Fatalf("bind with a wrong token: err = %v, want ErrWebhookClaimLost", err)
	}
	if n := countConversations(t, s, "u@x.com"); n != 0 {
		t.Fatalf("conversations = %d, want 0", n)
	}
	conv := bindWebhook(t, s, "github:a", tok)
	if c, _ = s.ClaimWebhookDelivery(ctx, "gh", "github:a"); c.Claimed || c.Completed || c.ConversationID != conv.ID {
		t.Fatalf("repeat of a bound delivery = %+v; want refused, not completed", c)
	}
	if _, err := s.CreateWebhookConversation(ctx, "gh", "github:a", tok, "u@x.com", "t", "victoria", "", false); !errors.Is(err, ErrWebhookClaimLost) {
		t.Fatalf("second bind: err = %v, want ErrWebhookClaimLost", err)
	}

	// Abandon needs the token; with it, the empty conversation goes and the
	// delivery is claimable afresh.
	if _, err := s.AbandonWebhookConversation(ctx, "gh", "github:a", "not-the-token", ""); !errors.Is(err, ErrWebhookClaimLost) {
		t.Fatalf("abandon with a wrong token: err = %v, want ErrWebhookClaimLost", err)
	}
	if !conversationExists(t, s, conv.ID) {
		t.Fatal("abandon with the wrong token deleted the conversation")
	}
	if completed, err := s.AbandonWebhookConversation(ctx, "gh", "github:a", tok, ""); err != nil || completed {
		t.Fatalf("abandon = %v, %v", completed, err)
	}
	if conversationExists(t, s, conv.ID) {
		t.Fatal("abandon left the empty conversation behind")
	}
	// The abandoned token can no longer complete anything.
	if _, err := s.CommitWebhookUserMessage(ctx, "gh", "github:a", tok, conv.ID, "t0", textEntry("user", "x")); !errors.Is(err, ErrWebhookClaimLost) {
		t.Fatalf("input commit after abandon: err = %v, want ErrWebhookClaimLost", err)
	}

	// Completion comes with the turn's input; then the delivery is a
	// duplicate, and abandon reports it completed and touches nothing.
	c, _ = s.ClaimWebhookDelivery(ctx, "gh", "github:a")
	if !c.Claimed {
		t.Fatal("an abandoned delivery could not be claimed again")
	}
	conv = bindWebhook(t, s, "github:a", c.Token)
	if err := completeWebhook(t, s, "github:a", c.Token, conv.ID, "turn-a"); err != nil {
		t.Fatal(err)
	}
	if completed, err := s.AbandonWebhookConversation(ctx, "gh", "github:a", c.Token, "turn-a"); err != nil || !completed {
		t.Fatalf("abandon after the input committed = %v, %v; want completed", completed, err)
	}
	if err := s.ReleaseWebhookDelivery(ctx, "gh", "github:a", c.Token); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ClaimWebhookDelivery(ctx, "gh", "github:a"); got.Claimed || !got.Completed || got.ConversationID != conv.ID {
		t.Fatalf("a completed delivery = %+v; want a duplicate of %s", got, conv.ID)
	}
	if !conversationExists(t, s, conv.ID) {
		t.Fatal("the completed delivery's conversation was deleted")
	}
}

// A request that bound a conversation and then died before its turn's input
// committed: a repeat inside the window is refused, one after it takes the
// delivery over under a new token and starts afresh — the dead request's empty
// conversation is deleted, and its token can do nothing more.
func TestWebhookDeliveryTakeoverStartsAfresh(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	orig, _ := s.ClaimWebhookDelivery(ctx, "gh", "github:crash")
	dead := bindWebhook(t, s, "github:crash", orig.Token)

	ageWebhook(t, s, "github:crash", 2*webhookClaimStaleAfter)
	retry, err := s.ClaimWebhookDelivery(ctx, "gh", "github:crash")
	if err != nil || !retry.Claimed || retry.Token == orig.Token || retry.ConversationID != "" {
		t.Fatalf("takeover = %+v, %v; want claimed under a new token, nothing inherited", retry, err)
	}
	if conversationExists(t, s, dead.ID) {
		t.Fatal("the dead request's empty conversation was left behind")
	}
	if _, err := s.CommitWebhookUserMessage(ctx, "gh", "github:crash", orig.Token, dead.ID, "t-dead", textEntry("user", "x")); !errors.Is(err, ErrWebhookClaimLost) {
		t.Fatalf("stale token's input commit: err = %v, want ErrWebhookClaimLost", err)
	}
	if _, err := s.AbandonWebhookConversation(ctx, "gh", "github:crash", orig.Token, ""); !errors.Is(err, ErrWebhookClaimLost) {
		t.Fatalf("stale token's abandon: err = %v, want ErrWebhookClaimLost", err)
	}
	if err := s.ReleaseWebhookDelivery(ctx, "gh", "github:crash", orig.Token); err != nil {
		t.Fatal(err)
	}

	fresh := bindWebhook(t, s, "github:crash", retry.Token)
	if err := completeWebhook(t, s, "github:crash", retry.Token, fresh.ID, "turn-retry"); err != nil {
		t.Fatal(err)
	}
	hist, _ := s.LoadHistory(ctx, fresh.ID)
	if len(hist) != 1 || countConversations(t, s, "u@x.com") != 1 {
		t.Fatalf("history=%d conversations=%d, want 1/1", len(hist), countConversations(t, s, "u@x.com"))
	}
	// Completed: a duplicate, even after the stale window.
	ageWebhook(t, s, "github:crash", 2*webhookClaimStaleAfter)
	if c, _ := s.ClaimWebhookDelivery(ctx, "gh", "github:crash"); c.Claimed || !c.Completed || c.ConversationID != fresh.ID {
		t.Fatalf("repeat of a completed delivery = %+v; want a duplicate of %s", c, fresh.ID)
	}
}

// A takeover keeps the earlier conversation while it has a turn that is still
// running (a live, slow claimant), and deletes it once that turn is finished
// with no saved input — the dead claimant's turn after startup recovery.
func TestWebhookDeliveryTakeoverKeepsALiveTurnDropsAFinishedOne(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		finished bool
	}{{"live", false}, {"recovered", true}} {
		key := "github:dead-turn-" + tc.name
		orig, _ := s.ClaimWebhookDelivery(ctx, "gh", key)
		dead := bindWebhook(t, s, key, orig.Token)
		turnID := "dead-turn-" + tc.name
		if err := s.CreateTurn(ctx, turnID, dead.ID, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		if tc.finished {
			if err := s.FinishTurn(ctx, turnID, TurnStatusError, time.Now().Unix(), false); err != nil {
				t.Fatal(err)
			}
		}
		ageWebhook(t, s, key, 2*webhookClaimStaleAfter)
		if c, _ := s.ClaimWebhookDelivery(ctx, "gh", key); !c.Claimed {
			t.Fatalf("%s: takeover refused: %+v", tc.name, c)
		}
		if got, want := conversationExists(t, s, dead.ID), !tc.finished; got != want {
			t.Fatalf("%s: conversation kept = %v, want %v", tc.name, got, want)
		}
	}
}

// Abandon deletes the conversation only while it is empty: no messages and
// no turns other than the abandoning request's own.
func TestWebhookAbandonDeletesOnlyEmptyConversations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		content func(convID string)
		kept    bool
	}{
		{"empty", func(string) {}, false},
		{"own turn only", func(convID string) {
			if err := s.CreateTurn(ctx, "own-"+convID, convID, time.Now().Unix()); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"with a message", func(convID string) {
			if _, err := s.AppendHistory(ctx, convID, []agent.HistoryEntry{textEntry("user", "mine now")}); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"with another turn", func(convID string) {
			if err := s.CreateTurn(ctx, "other-"+convID, convID, time.Now().Unix()); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		key := "github:" + tc.name
		c, err := s.ClaimWebhookDelivery(ctx, "gh", key)
		if err != nil || !c.Claimed {
			t.Fatalf("%s: claim %+v %v", tc.name, c, err)
		}
		conv := bindWebhook(t, s, key, c.Token)
		tc.content(conv.ID)
		if _, err := s.AbandonWebhookConversation(ctx, "gh", key, c.Token, "own-"+conv.ID); err != nil {
			t.Fatalf("%s: abandon: %v", tc.name, err)
		}
		if got := conversationExists(t, s, conv.ID); got != tc.kept {
			t.Fatalf("%s: conversation kept = %v, want %v", tc.name, got, tc.kept)
		}
		if c, _ := s.ClaimWebhookDelivery(ctx, "gh", key); !c.Claimed {
			t.Fatalf("%s: after abandon the delivery should be claimable afresh, got %+v", tc.name, c)
		}
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
