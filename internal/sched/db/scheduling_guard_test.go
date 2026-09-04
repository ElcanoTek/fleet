package db

import (
	"context"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/google/uuid"
)

// The scheduler reads due rows before promoting them. An edit that commits
// between those statements must still govern dispatch, even if status stays
// scheduled. Drive that interleaving explicitly instead of relying on timing.
func TestScheduledBatchPromotionRechecksEditedDefinition(t *testing.T) {
	d := setupTestDB(t)
	defer d.Close()
	ctx := context.Background()
	for _, change := range []string{"postponed", "new gate", "webhook template", "unchanged"} {
		t.Run(change, func(t *testing.T) {
			due := time.Now().UTC().Add(-time.Minute)
			task := models.NewTask(models.TaskCreate{Prompt: "promotion race", ScheduledFor: &due})
			task.Status = models.TaskStatusScheduled
			if err := d.AddTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			rows, err := d.GetScheduledTasks(ctx, time.Now(), time.Time{}, uuid.Nil, 100)
			if err != nil {
				t.Fatal(err)
			}
			selected := false
			for _, row := range rows {
				if row.ID == task.ID {
					selected = true
				}
			}
			if !selected {
				t.Fatal("due task was not selected")
			}
			switch change {
			case "postponed":
				future := time.Now().UTC().Add(time.Hour)
				task.ScheduledFor = &future
			case "new gate":
				task.RunIf = &models.RunIf{Command: "false"}
			case "webhook template":
				task.TriggerType = models.TriggerTypeWebhook
			}
			if err := d.UpdateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			n, err := d.UpdateTasksStatusBatch(ctx, []uuid.UUID{task.ID}, models.TaskStatusScheduled, models.TaskStatusPending)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			wantStatus := models.TaskStatusScheduled
			if change == "unchanged" {
				want = 1
				wantStatus = models.TaskStatusPending
			}
			if n != want {
				t.Errorf("promoted %d rows, want %d", n, want)
			}
			got, err := d.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != wantStatus {
				t.Errorf("status = %s, want %s", got.Status, wantStatus)
			}
		})
	}
}
