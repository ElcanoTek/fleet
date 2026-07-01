package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// benchSeedChunk bounds how many tasks are inserted per AddTaskBatch call so a
// large seed can't exceed PostgreSQL's 65535-bind-parameter limit (AddTask
// carries ~57 columns/row).
const benchSeedChunk = 1000

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
	for i := 0; i < len(tasks); i += benchSeedChunk {
		end := i + benchSeedChunk
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
