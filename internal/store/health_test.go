package store

import (
	"context"
	"testing"
	"time"
)

func TestLLMUsageSince(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c, err := s.CreateConversation(ctx, "a@e.com", "t", "assistant", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	now := time.Now().Unix()
	rows := []struct {
		ts   int64
		cost float64
	}{
		{now, 0.5},
		{now, 1.0},
		{now - 86400, 9.0}, // yesterday — excluded by the since cutoff
	}
	for _, r := range rows {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO turn_metrics (conversation_id, user_email, completed_at, cost_usd) VALUES ($1,$2,$3,$4)`,
			c.ID, "a@e.com", r.ts, r.cost); err != nil {
			t.Fatalf("insert metric: %v", err)
		}
	}

	calls, cost, err := s.LLMUsageSince(ctx, now-3600)
	if err != nil {
		t.Fatalf("LLMUsageSince: %v", err)
	}
	if calls != 2 || cost != 1.5 {
		t.Errorf("LLMUsageSince = (calls=%d, cost=%v), want (2, 1.5)", calls, cost)
	}

	// Ping + PoolStats are live against the test DB.
	if err := s.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if st := s.PoolStats(); st.OpenConnections < 0 {
		t.Errorf("PoolStats returned nonsense: %+v", st)
	}
}
