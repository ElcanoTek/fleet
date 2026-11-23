package db

// Spend recorded for a task stays in the usage read model after the task row
// is gone. task_iterations is the task-side spend record the usage report,
// the adoption report and the budgets aggregate; it carries the attribution
// they group by (creator, API key, model) and outlives the task, the way chat
// turn_metrics outlives its conversation (chat migration 038).

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// usageSnapshot is every task-usage aggregation over [from, to): one bucket
// map per group_by, plus the adoption report's (user, day) rows.
type usageSnapshot struct {
	byGroup map[string]map[string]models.UsageBucket
	userDay []models.UserDayUsage
}

func takeUsageSnapshot(t *testing.T, db *Database, from, to time.Time) usageSnapshot {
	t.Helper()
	ctx := context.Background()
	s := usageSnapshot{byGroup: map[string]map[string]models.UsageBucket{}}
	for groupBy := range taskUsageGroupExpr {
		rows, err := db.TaskUsage(ctx, from, to, groupBy)
		if err != nil {
			t.Fatalf("TaskUsage(%s): %v", groupBy, err)
		}
		s.byGroup[groupBy] = bucketMap(rows)
	}
	rows, err := db.TaskUsageByUserDay(ctx, from, to)
	if err != nil {
		t.Fatalf("TaskUsageByUserDay: %v", err)
	}
	s.userDay = rows
	return s
}

func allTaskIDs(t *testing.T, db *Database) []uuid.UUID {
	t.Helper()
	rows, err := db.conn.QueryContext(context.Background(), `SELECT id FROM tasks ORDER BY id`)
	if err != nil {
		t.Fatalf("list task ids: %v", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// TestTaskUsageCountsSpendOfDeletedTasks: deleting every task leaves every
// usage aggregation — by user, key, model, project, day and week, and the
// adoption report's per-user days — exactly as it was, attribution included.
func TestTaskUsageCountsSpendOfDeletedTasks(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()
	seedUsageFixture(t, db)

	before := takeUsageSnapshot(t, db, usageFrom, usageTo)
	if got := before.byGroup["user"]["alice@example.com"]; got.TaskIterations != 3 {
		t.Fatalf("fixture sanity: alice has %d iterations, want 3", got.TaskIterations)
	}

	ids := allTaskIDs(t, db)
	if len(ids) != 3 {
		t.Fatalf("fixture sanity: %d tasks, want 3", len(ids))
	}
	for _, id := range ids {
		deleted, err := db.DeleteTask(ctx, id)
		if err != nil || !deleted {
			t.Fatalf("DeleteTask(%s) = %v, %v", id, deleted, err)
		}
	}
	if left := allTaskIDs(t, db); len(left) != 0 {
		t.Fatalf("tasks left after delete: %v", left)
	}

	after := takeUsageSnapshot(t, db, usageFrom, usageTo)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("usage changed when the tasks were deleted:\nbefore %+v\nafter  %+v", before, after)
	}
}

// TestTaskUsageCountsSpendOfPrunedTasks: the retention sweeps remove old task
// rows (the daily CleanupOldRuns under FLEET_RUN_LOG_RETENTION_DAYS, and the
// POST /tasks/cleanup DeleteOldHistory), and the spend those tasks recorded
// still counts, under the creator who incurred it.
func TestTaskUsageCountsSpendOfPrunedTasks(t *testing.T) {
	t.Parallel()

	sweeps := map[string]func(ctx context.Context, db *Database) (int, error){
		"CleanupOldRuns": func(ctx context.Context, db *Database) (int, error) {
			return db.CleanupOldRuns(ctx, 7, 0)
		},
		"DeleteOldHistory": func(ctx context.Context, db *Database) (int, error) {
			return db.DeleteOldHistory(ctx, 7)
		},
	}
	for name, sweep := range sweeps {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := setupTestDB(t)
			ctx := context.Background()

			owner := uuid.New()
			seedAccount(t, db, owner, "pat@example.com", "client")
			key, model := "key-pat", "model-p"
			ran := time.Now().UTC().AddDate(0, 0, -10)
			task := &models.Task{ID: uuid.New(), Prompt: "old run", Status: models.TaskStatusSuccess,
				CreatedAt: ran, CompletedAt: &ran, CreatedBy: &owner, CreatedByKeyID: &key, Model: &model}
			if err := db.AddTask(ctx, task); err != nil {
				t.Fatalf("AddTask: %v", err)
			}
			if err := db.AddTaskIteration(ctx, &models.TaskIteration{ID: uuid.New(), TaskID: task.ID, IterationNumber: 1,
				StartedAt: ran, CostUSD: 3, PromptTokens: 300, CompletionTokens: 30, Status: models.IterationStatusPassed}); err != nil {
				t.Fatalf("AddTaskIteration: %v", err)
			}

			from, to := time.Now().UTC().AddDate(0, 0, -30), time.Now().UTC().Add(time.Hour)
			before := takeUsageSnapshot(t, db, from, to)

			pruned, err := sweep(ctx, db)
			if err != nil || pruned != 1 {
				t.Fatalf("%s = %d, %v; want the one old task pruned", name, pruned, err)
			}
			if exists, err := db.TaskExists(ctx, task.ID); err != nil || exists {
				t.Fatalf("task should be pruned: exists=%v, err=%v", exists, err)
			}

			after := takeUsageSnapshot(t, db, from, to)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("usage changed when the task was pruned:\nbefore %+v\nafter  %+v", before, after)
			}
			for groupBy, want := range map[string]string{"user": "pat@example.com", "key": key, "model": model} {
				if b := after.byGroup[groupBy][want]; !almostEqual(b.TaskCostUSD, 3) || b.PromptTokens != 300 || b.TaskIterations != 1 {
					t.Errorf("group_by=%s bucket %q = %+v, want the pruned task's spend", groupBy, want, b)
				}
			}
		})
	}
}

// TestTopTaskOutliersAttributesLikeTheUsageReport: the Analytics top tasks
// list shows a task's model and owner the way the usage report attributes its
// spend — from the task's iterations, so a model edited after the run does
// not relabel it — falls back to the task row for a failing task with no
// iteration in the window, and lists only tasks that still exist.
func TestTopTaskOutliersAttributesLikeTheUsageReport(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()
	owner := uuid.New()
	seedAccount(t, db, owner, "olga@example.com", "client")
	ran := time.Now().UTC().Add(-time.Hour)

	add := func(status models.TaskStatus, model string, costUSD float64) uuid.UUID {
		t.Helper()
		m := model
		task := &models.Task{ID: uuid.New(), Prompt: "job " + model, Status: status,
			CreatedAt: ran, CompletedAt: &ran, CreatedBy: &owner, Model: &m}
		if err := db.AddTask(ctx, task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		if costUSD > 0 {
			if err := db.AddTaskIteration(ctx, &models.TaskIteration{ID: uuid.New(), TaskID: task.ID, IterationNumber: 1,
				StartedAt: ran, CostUSD: costUSD, PromptTokens: 10, Status: models.IterationStatusPassed}); err != nil {
				t.Fatalf("AddTaskIteration: %v", err)
			}
		}
		return task.ID
	}
	ranOnA := add(models.TaskStatusSuccess, "model-a", 5)
	if _, err := db.conn.ExecContext(ctx, `UPDATE tasks SET model = 'model-b' WHERE id = $1`, ranOnA); err != nil {
		t.Fatal(err)
	}
	gone := add(models.TaskStatusSuccess, "model-d", 7)
	if deleted, err := db.DeleteTask(ctx, gone); err != nil || !deleted {
		t.Fatalf("DeleteTask = %v, %v", deleted, err)
	}
	failed := add(models.TaskStatusError, "model-c", 0)

	rows, err := db.TopTaskOutliers(ctx, 25)
	if err != nil {
		t.Fatalf("TopTaskOutliers: %v", err)
	}
	byID := map[string]TaskOutlierRow{}
	for _, r := range rows {
		byID[r.TaskID] = r
	}
	if r := byID[ranOnA.String()]; r.Model != "model-a" || r.OwnerEmail != "olga@example.com" || !almostEqual(r.CostUSD, 5) {
		t.Errorf("top task row = %+v, want model-a / olga@example.com / 5 as its spend is attributed", r)
	}
	if r := byID[failed.String()]; r.Model != "model-c" || r.OwnerEmail != "olga@example.com" {
		t.Errorf("failing task without iterations = %+v, want the task's own model-c / olga@example.com", r)
	}
	if r, ok := byID[gone.String()]; ok {
		t.Errorf("a deleted task is listed: %+v", r)
	}

	usage, err := db.TaskUsage(ctx, ran.Add(-time.Minute), time.Now().UTC().Add(time.Minute), "model")
	if err != nil {
		t.Fatalf("TaskUsage: %v", err)
	}
	m := bucketMap(usage)
	if !almostEqual(m["model-a"].TaskCostUSD, 5) || !almostEqual(m["model-d"].TaskCostUSD, 7) {
		t.Errorf("usage by model = %+v, want model-a 5 and the deleted task's model-d 7", m)
	}
	if _, ok := m["model-b"]; ok {
		t.Errorf("usage by model relabelled spend to the edited model: %+v", m)
	}
}
