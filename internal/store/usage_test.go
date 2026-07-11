package store

// DB-backed tests for the chat side of the usage-analytics read model (#601
// part 1): the turn_metrics roll-up. Gated on FLEET_TEST_DATABASE_URL like the
// rest of the package (newTestStore skips without it).

import (
	"context"
	"math"
	"testing"
	"time"
)

// seedChatUsage inserts one project ("Growth"), three conversations (alice on
// model-x in the project, alice on the default model, bob on model-x) and four
// turns:
//
//	turn1  alice / model-x / Growth   2026-06-01  $1.00  100/10, 5 cached
//	turn2  alice / model-x / Growth   2026-06-02  $2.00  200/20, 0 cached
//	turn3  alice / "" (default) / —   2026-06-08  $4.00  400/40, 0 cached
//	turn4  bob   / model-x / —        2026-06-08  $8.00  800/80, 7 cached, cancelled
func seedChatUsage(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	proj, err := s.CreateProject(ctx, &Project{OwnerEmail: "alice@example.com", Name: "Growth"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	convProj, err := s.CreateProjectConversation(ctx, "alice@example.com", "c1", "", "model-x", false, proj.ID, nil)
	if err != nil {
		t.Fatalf("CreateProjectConversation: %v", err)
	}
	convDefault, err := s.CreateConversation(ctx, "alice@example.com", "c2", "", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	convBob, err := s.CreateConversation(ctx, "bob@example.com", "c3", "", "model-x", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	day := func(d int) int64 { return time.Date(2026, 6, d, 12, 0, 0, 0, time.UTC).Unix() }
	turns := []TurnMetric{
		{ConversationID: convProj.ID, UserEmail: "alice@example.com", CompletedAt: day(1), CostUSD: 1.0, PromptTokens: 100, CompletionTokens: 10, CachedTokens: 5},
		{ConversationID: convProj.ID, UserEmail: "alice@example.com", CompletedAt: day(2), CostUSD: 2.0, PromptTokens: 200, CompletionTokens: 20},
		{ConversationID: convDefault.ID, UserEmail: "alice@example.com", CompletedAt: day(8), CostUSD: 4.0, PromptTokens: 400, CompletionTokens: 40},
		{ConversationID: convBob.ID, UserEmail: "bob@example.com", CompletedAt: day(8), CostUSD: 8.0, PromptTokens: 800, CompletionTokens: 80, CachedTokens: 7, Cancelled: true},
	}
	for _, m := range turns {
		if err := s.RecordTurn(ctx, m); err != nil {
			t.Fatalf("RecordTurn: %v", err)
		}
	}
}

func usageRowMap(rows []UsageRow) map[string]UsageRow {
	out := map[string]UsageRow{}
	for _, r := range rows {
		out[r.Key] = r
	}
	return out
}

func usageAlmostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

var (
	chatUsageFrom = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	chatUsageTo   = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
)

// TestUsageSummaryGroupByPrincipal covers the who/where dimensions: user email
// and project (id + name label).
func TestUsageSummaryGroupByPrincipal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedChatUsage(t, s)

	from, to := chatUsageFrom, chatUsageTo

	t.Run("user", func(t *testing.T) {
		rows, err := s.UsageSummary(ctx, from, to, "user")
		if err != nil {
			t.Fatalf("UsageSummary: %v", err)
		}
		m := usageRowMap(rows)
		if len(m) != 2 {
			t.Fatalf("want 2 user buckets, got %+v", rows)
		}
		a := m["alice@example.com"]
		if !usageAlmostEqual(a.CostUSD, 7.0) || a.PromptTokens != 700 || a.CompletionTokens != 70 || a.CachedTokens != 5 || a.Turns != 3 {
			t.Errorf("alice bucket wrong: %+v", a)
		}
		// Cancelled turns still count — their cost was spent.
		b := m["bob@example.com"]
		if !usageAlmostEqual(b.CostUSD, 8.0) || b.Turns != 1 || b.CachedTokens != 7 {
			t.Errorf("bob bucket wrong (cancelled turn must count): %+v", b)
		}
	})

	t.Run("project", func(t *testing.T) {
		rows, err := s.UsageSummary(ctx, from, to, "project")
		if err != nil {
			t.Fatalf("UsageSummary: %v", err)
		}
		m := usageRowMap(rows)
		if len(m) != 2 {
			t.Fatalf("want 2 project buckets, got %+v", rows)
		}
		var growth UsageRow
		for key, r := range m {
			if key != "" {
				growth = r
			}
		}
		if growth.Label != "Growth" || !usageAlmostEqual(growth.CostUSD, 3.0) || growth.Turns != 2 {
			t.Errorf("project bucket wrong: %+v", growth)
		}
		if noProj := m[""]; !usageAlmostEqual(noProj.CostUSD, 12.0) || noProj.Turns != 2 {
			t.Errorf("no-project bucket wrong: %+v", noProj)
		}
	})
}

// TestUsageSummarySurvivesConversationDeletion protects the accounting/content
// boundary: deleting chat content must not erase the cost and tokens already
// spent. Migration 038 intentionally removes turn_metrics' cascade for this.
func TestUsageSummarySurvivesConversationDeletion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.softDelete = false

	conv, err := s.CreateConversation(ctx, "admin@example.com", "temporary", "", "model-x", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	metric := TurnMetric{
		ConversationID:   conv.ID,
		UserEmail:        "admin@example.com",
		CompletedAt:      time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC).Unix(),
		CostUSD:          1.25,
		PromptTokens:     120,
		CompletionTokens: 30,
	}
	if err := s.RecordTurn(ctx, metric); err != nil {
		t.Fatalf("RecordTurn: %v", err)
	}
	if err := s.Delete(ctx, "admin@example.com", conv.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	rows, err := s.UsageSummary(ctx, chatUsageFrom, chatUsageTo, "user")
	if err != nil {
		t.Fatalf("UsageSummary: %v", err)
	}
	got := usageRowMap(rows)["admin@example.com"]
	if !usageAlmostEqual(got.CostUSD, 1.25) || got.PromptTokens != 120 || got.CompletionTokens != 30 || got.Turns != 1 {
		t.Fatalf("usage after conversation deletion = %+v, want preserved metric", got)
	}
}

// TestUsageSummaryGroupByDimensions covers the what/when dimensions: model,
// key (which chat doesn't have), day/week time bucketing, and the closed-set
// group_by validation.
func TestUsageSummaryGroupByDimensions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedChatUsage(t, s)

	from, to := chatUsageFrom, chatUsageTo

	t.Run("model", func(t *testing.T) {
		rows, err := s.UsageSummary(ctx, from, to, "model")
		if err != nil {
			t.Fatalf("UsageSummary: %v", err)
		}
		m := usageRowMap(rows)
		if !usageAlmostEqual(m["model-x"].CostUSD, 11.0) || m["model-x"].Turns != 3 {
			t.Errorf("model-x bucket wrong: %+v", m["model-x"])
		}
		if !usageAlmostEqual(m[""].CostUSD, 4.0) {
			t.Errorf("default-model bucket wrong: %+v", m[""])
		}
	})

	t.Run("key has no chat dimension", func(t *testing.T) {
		rows, err := s.UsageSummary(ctx, from, to, "key")
		if err != nil {
			t.Fatalf("UsageSummary: %v", err)
		}
		if len(rows) != 1 || rows[0].Key != "" || !usageAlmostEqual(rows[0].CostUSD, 15.0) {
			t.Errorf("want all chat spend under the empty key, got %+v", rows)
		}
	})

	t.Run("day", func(t *testing.T) {
		rows, err := s.UsageSummary(ctx, from, to, "day")
		if err != nil {
			t.Fatalf("UsageSummary: %v", err)
		}
		m := usageRowMap(rows)
		if len(m) != 3 {
			t.Fatalf("want 3 day buckets, got %+v", rows)
		}
		if !usageAlmostEqual(m["2026-06-01"].CostUSD, 1.0) || !usageAlmostEqual(m["2026-06-02"].CostUSD, 2.0) || !usageAlmostEqual(m["2026-06-08"].CostUSD, 12.0) {
			t.Errorf("day buckets wrong: %+v", m)
		}
	})

	t.Run("week", func(t *testing.T) {
		rows, err := s.UsageSummary(ctx, from, to, "week")
		if err != nil {
			t.Fatalf("UsageSummary: %v", err)
		}
		m := usageRowMap(rows)
		// 2026-06-01 and 2026-06-08 are both Mondays → two ISO weeks.
		if len(m) != 2 {
			t.Fatalf("want 2 week buckets, got %+v", rows)
		}
		if !usageAlmostEqual(m["2026-06-01"].CostUSD, 3.0) || !usageAlmostEqual(m["2026-06-08"].CostUSD, 12.0) {
			t.Errorf("week buckets wrong: %+v", m)
		}
	})

	t.Run("invalid group_by errors", func(t *testing.T) {
		if _, err := s.UsageSummary(ctx, from, to, "email; DROP TABLE users"); err == nil {
			t.Fatal("want error for invalid group_by")
		}
	})
}

func TestUsageSummaryRangeFiltering(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedChatUsage(t, s)

	// [June 2, June 8): keeps turn2 only.
	rows, err := s.UsageSummary(ctx,
		time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
		"user")
	if err != nil {
		t.Fatalf("UsageSummary: %v", err)
	}
	if len(rows) != 1 || !usageAlmostEqual(rows[0].CostUSD, 2.0) || rows[0].Turns != 1 {
		t.Errorf("range filter wrong: %+v", rows)
	}

	rows, err = s.UsageSummary(ctx,
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC),
		"user")
	if err != nil {
		t.Fatalf("UsageSummary: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("want no buckets outside the range, got %+v", rows)
	}
}
