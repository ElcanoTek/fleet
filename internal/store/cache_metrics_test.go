package store

import (
	"context"
	"testing"
)

// TestRecordTurn_PersistsCacheCreationTokens verifies the cache-write token
// count survives the RecordTurn → turn_metrics → AdminStats round trip, so net
// prompt-cache savings can be computed from the DB alone (#259).
func TestRecordTurn_PersistsCacheCreationTokens(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, "alice@example.com", "password123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	conv, err := s.CreateConversation(ctx, "alice@example.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Two turns: a cache-write turn then a cache-hit turn.
	turns := []TurnMetric{
		{ConversationID: conv.ID, UserEmail: "alice@example.com", CompletedAt: 1, CostUSD: 0.5,
			PromptTokens: 1000, CompletionTokens: 100, CachedTokens: 0, CacheCreationTokens: 1000},
		{ConversationID: conv.ID, UserEmail: "alice@example.com", CompletedAt: 2, CostUSD: 0.1,
			PromptTokens: 1000, CompletionTokens: 100, CachedTokens: 900, CacheCreationTokens: 0},
	}
	for _, m := range turns {
		if err := s.RecordTurn(ctx, m); err != nil {
			t.Fatalf("RecordTurn: %v", err)
		}
	}

	rows, err := s.AdminStats(ctx)
	if err != nil {
		t.Fatalf("AdminStats: %v", err)
	}
	var got *AdminRow
	for i := range rows {
		if rows[i].Email == "alice@example.com" {
			got = &rows[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no admin row for alice")
	}
	if got.TotalTurns != 2 {
		t.Errorf("TotalTurns = %d, want 2", got.TotalTurns)
	}
	if got.TotalPromptTokens != 2000 {
		t.Errorf("TotalPromptTokens = %d, want 2000", got.TotalPromptTokens)
	}
	if got.TotalCachedTokens != 900 {
		t.Errorf("TotalCachedTokens = %d, want 900", got.TotalCachedTokens)
	}
	if got.TotalCacheCreationTokens != 1000 {
		t.Errorf("TotalCacheCreationTokens = %d, want 1000", got.TotalCacheCreationTokens)
	}
}
