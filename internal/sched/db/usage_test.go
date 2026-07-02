package db

// DB-backed tests for the usage-analytics read model (#601 part 1): the
// task_iterations ⋈ tasks roll-up. Gated on DATABASE_URL like the rest of the
// package (setupTestDB skips without it).

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// seedUsageFixture inserts two users, three tasks (alice + key-1 on model-a,
// alice without a key on model-b, an orphan task with no creator on model-a)
// and four iterations spread across two ISO weeks:
//
//	it1  task1 (alice/key-1/model-a)  2026-06-01 (Mon)  $1.00  100/10
//	it2  task1 (alice/key-1/model-a)  2026-06-02 (Tue)  $2.00  200/20
//	it3  task2 (alice/—/model-b)      2026-06-08 (Mon)  $4.00  400/40
//	it4  task3 (—/—/model-a)          2026-06-08 (Mon)  $8.00  800/80
func seedUsageFixture(t *testing.T, db *Database) (alice uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	alice = uuid.New()
	if err := db.AddUser(ctx, &models.User{ID: alice, Username: "alice@example.com", Role: "client", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("AddUser: %v", err)
	}

	modelA, modelB, key1 := "model-a", "model-b", "key-1"
	task1 := &models.Task{ID: uuid.New(), Prompt: "t1", Status: models.TaskStatusSuccess, CreatedAt: time.Now().UTC(), CreatedBy: &alice, CreatedByKeyID: &key1, Model: &modelA}
	task2 := &models.Task{ID: uuid.New(), Prompt: "t2", Status: models.TaskStatusSuccess, CreatedAt: time.Now().UTC(), CreatedBy: &alice, Model: &modelB}
	task3 := &models.Task{ID: uuid.New(), Prompt: "t3", Status: models.TaskStatusSuccess, CreatedAt: time.Now().UTC(), Model: &modelA}
	for _, task := range []*models.Task{task1, task2, task3} {
		if err := db.AddTask(ctx, task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
	}

	day := func(d int) time.Time { return time.Date(2026, 6, d, 12, 0, 0, 0, time.UTC) }
	iters := []*models.TaskIteration{
		{ID: uuid.New(), TaskID: task1.ID, IterationNumber: 1, StartedAt: day(1), CostUSD: 1.0, PromptTokens: 100, CompletionTokens: 10, Status: "completed"},
		{ID: uuid.New(), TaskID: task1.ID, IterationNumber: 2, StartedAt: day(2), CostUSD: 2.0, PromptTokens: 200, CompletionTokens: 20, Status: "completed"},
		{ID: uuid.New(), TaskID: task2.ID, IterationNumber: 1, StartedAt: day(8), CostUSD: 4.0, PromptTokens: 400, CompletionTokens: 40, Status: "failed"},
		{ID: uuid.New(), TaskID: task3.ID, IterationNumber: 1, StartedAt: day(8), CostUSD: 8.0, PromptTokens: 800, CompletionTokens: 80, Status: "completed"},
	}
	for _, it := range iters {
		if err := db.AddTaskIteration(ctx, it); err != nil {
			t.Fatalf("AddTaskIteration: %v", err)
		}
	}
	return alice
}

func bucketMap(buckets []models.UsageBucket) map[string]models.UsageBucket {
	out := map[string]models.UsageBucket{}
	for _, b := range buckets {
		out[b.Key] = b
	}
	return out
}

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

var (
	usageFrom = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	usageTo   = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
)

// TestTaskUsageGroupByPrincipal covers the who-spent-it dimensions: user
// (including the deleted-user UUID fallback) and API key.
func TestTaskUsageGroupByPrincipal(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	alice := seedUsageFixture(t, db)

	from, to := usageFrom, usageTo

	t.Run("user", func(t *testing.T) {
		rows, err := db.TaskUsage(ctx, from, to, "user")
		if err != nil {
			t.Fatalf("TaskUsage: %v", err)
		}
		m := bucketMap(rows)
		if len(m) != 2 {
			t.Fatalf("want 2 user buckets, got %d: %+v", len(m), rows)
		}
		a := m["alice@example.com"]
		if !almostEqual(a.TaskCostUSD, 7.0) || a.PromptTokens != 700 || a.CompletionTokens != 70 || a.TaskIterations != 3 {
			t.Errorf("alice bucket wrong: %+v", a)
		}
		if !almostEqual(a.CostUSD, a.TaskCostUSD) {
			t.Errorf("CostUSD should mirror TaskCostUSD on the task side: %+v", a)
		}
		orphan := m[""]
		if !almostEqual(orphan.TaskCostUSD, 8.0) || orphan.TaskIterations != 1 {
			t.Errorf("orphan (no creator) bucket wrong: %+v", orphan)
		}
		_ = alice
	})

	t.Run("user falls back to UUID when the user row is gone", func(t *testing.T) {
		if _, err := db.conn.ExecContext(ctx, "DELETE FROM users WHERE id = $1", alice); err != nil {
			t.Fatalf("delete user: %v", err)
		}
		t.Cleanup(func() {
			if err := db.AddUser(ctx, &models.User{ID: alice, Username: "alice@example.com", Role: "client", CreatedAt: time.Now().UTC()}); err != nil {
				t.Fatalf("re-add user: %v", err)
			}
		})
		rows, err := db.TaskUsage(ctx, from, to, "user")
		if err != nil {
			t.Fatalf("TaskUsage: %v", err)
		}
		m := bucketMap(rows)
		if b, ok := m[alice.String()]; !ok || b.TaskIterations != 3 {
			t.Errorf("want deleted user's spend under its UUID, got %+v", rows)
		}
	})

	t.Run("key", func(t *testing.T) {
		rows, err := db.TaskUsage(ctx, from, to, "key")
		if err != nil {
			t.Fatalf("TaskUsage: %v", err)
		}
		m := bucketMap(rows)
		if !almostEqual(m["key-1"].TaskCostUSD, 3.0) || m["key-1"].TaskIterations != 2 {
			t.Errorf("key-1 bucket wrong: %+v", m["key-1"])
		}
		if !almostEqual(m[""].TaskCostUSD, 12.0) {
			t.Errorf("keyless bucket wrong: %+v", m[""])
		}
	})
}

// TestTaskUsageGroupByDimensions covers the what/when dimensions: model,
// project (which tasks don't have), day/week time bucketing, and the
// closed-set group_by validation.
func TestTaskUsageGroupByDimensions(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	seedUsageFixture(t, db)

	from, to := usageFrom, usageTo

	t.Run("model", func(t *testing.T) {
		rows, err := db.TaskUsage(ctx, from, to, "model")
		if err != nil {
			t.Fatalf("TaskUsage: %v", err)
		}
		m := bucketMap(rows)
		if !almostEqual(m["model-a"].TaskCostUSD, 11.0) || m["model-a"].TaskIterations != 3 {
			t.Errorf("model-a bucket wrong: %+v", m["model-a"])
		}
		if !almostEqual(m["model-b"].TaskCostUSD, 4.0) {
			t.Errorf("model-b bucket wrong: %+v", m["model-b"])
		}
	})

	t.Run("project rolls every task into the empty key", func(t *testing.T) {
		rows, err := db.TaskUsage(ctx, from, to, "project")
		if err != nil {
			t.Fatalf("TaskUsage: %v", err)
		}
		if len(rows) != 1 || rows[0].Key != "" || !almostEqual(rows[0].TaskCostUSD, 15.0) {
			t.Errorf("want one empty-key bucket with all task spend, got %+v", rows)
		}
	})

	t.Run("day", func(t *testing.T) {
		rows, err := db.TaskUsage(ctx, from, to, "day")
		if err != nil {
			t.Fatalf("TaskUsage: %v", err)
		}
		m := bucketMap(rows)
		if len(m) != 3 {
			t.Fatalf("want 3 day buckets, got %+v", rows)
		}
		if !almostEqual(m["2026-06-01"].TaskCostUSD, 1.0) || !almostEqual(m["2026-06-02"].TaskCostUSD, 2.0) || !almostEqual(m["2026-06-08"].TaskCostUSD, 12.0) {
			t.Errorf("day buckets wrong: %+v", m)
		}
	})

	t.Run("week", func(t *testing.T) {
		rows, err := db.TaskUsage(ctx, from, to, "week")
		if err != nil {
			t.Fatalf("TaskUsage: %v", err)
		}
		m := bucketMap(rows)
		// 2026-06-01 and 2026-06-08 are both Mondays → two ISO weeks.
		if len(m) != 2 {
			t.Fatalf("want 2 week buckets, got %+v", rows)
		}
		if !almostEqual(m["2026-06-01"].TaskCostUSD, 3.0) || !almostEqual(m["2026-06-08"].TaskCostUSD, 12.0) {
			t.Errorf("week buckets wrong: %+v", m)
		}
	})

	t.Run("invalid group_by errors", func(t *testing.T) {
		if _, err := db.TaskUsage(ctx, from, to, "prompt; DROP TABLE tasks"); err == nil {
			t.Fatal("want error for invalid group_by")
		}
	})
}

func TestTaskUsageRangeFiltering(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	seedUsageFixture(t, db)

	// [June 2, June 8): keeps it2 only — from is inclusive, to exclusive, so
	// it3/it4 (started exactly June 8 12:00) are out when to is June 8 00:00.
	rows, err := db.TaskUsage(ctx,
		time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
		"user")
	if err != nil {
		t.Fatalf("TaskUsage: %v", err)
	}
	if len(rows) != 1 || !almostEqual(rows[0].TaskCostUSD, 2.0) || rows[0].TaskIterations != 1 {
		t.Errorf("range filter wrong: %+v", rows)
	}

	// Empty window → no buckets.
	rows, err = db.TaskUsage(ctx,
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC),
		"user")
	if err != nil {
		t.Fatalf("TaskUsage: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("want no buckets outside the range, got %+v", rows)
	}
}
