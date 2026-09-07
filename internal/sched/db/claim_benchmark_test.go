package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// BenchmarkClaimNextPendingTask measures scheduler claim throughput — the
// FOR UPDATE SKIP LOCKED transaction each worker runs to lease the next pending
// task (#296). It seeds b.N claimable pending tasks BEFORE the timer starts,
// then times b.N claims. Skips without DATABASE_URL (integration benchmark).
//
//	DATABASE_URL=... go test -run '^$' -bench BenchmarkClaimNextPendingTask ./internal/sched/db/
func BenchmarkClaimNextPendingTask(b *testing.B) {
	db := setupTestDB(b)
	ctx := context.Background()

	tasks := make([]*models.Task, b.N)
	now := time.Now().UTC()
	for i := range tasks {
		tasks[i] = &models.Task{
			ID:        uuid.New(),
			Prompt:    "bench claim",
			Status:    models.TaskStatusPending,
			CreatedAt: now,
		}
	}
	// Seed in chunks of MaxTaskBatchRows — the registry-derived ceiling on
	// rows per multi-row INSERT — so the seed can never trip PostgreSQL's
	// 65535-bind-parameter limit. This used to be a hard-coded 1000, which was
	// correct at ~57 insert columns and silently wrong once the registry grew
	// past 65 (68 columns × 1000 rows = 68000 parameters): the weekly run then
	// failed only on machines where b.N happened to land above 963.
	chunk := MaxTaskBatchRows()
	for i := 0; i < len(tasks); i += chunk {
		end := i + chunk
		if end > len(tasks) {
			end = len(tasks)
		}
		if err := db.AddTaskBatch(ctx, tasks[i:end]); err != nil {
			b.Fatalf("seed batch [%d:%d]: %v", i, end, err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.ClaimNextPendingTask(ctx, "bench-worker", time.Minute); err != nil {
			b.Fatalf("claim %d: %v", i, err)
		}
	}
}
