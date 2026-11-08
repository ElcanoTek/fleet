package runner

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/telemetry/spantest"
)

// A run whose lease was recovered out from under it (the pool no longer
// holds its token) skips its terminal write. Its task.run span records that
// loss instead of ending as a success (#2052).
func TestTaskRunSpanRecordsLeaseLossOnStaleRun(t *testing.T) {
	sr := spantest.Install(t)
	store := newTestStore(t)
	task := seedPending(t, store, 1)[0]
	runner := TaskRunnerFunc(func(context.Context, *models.Task) (*models.LogSession, error) {
		return &models.LogSession{ID: "s"}, nil // the run itself "succeeded"
	})
	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: time.Hour, LeaseRenewInterval: time.Hour})

	// Not registered in the pool's active map: stillOwns is false, as after a
	// lease recovery re-claimed the task under a new token.
	pool.executeTask(context.Background(), task, uuid.New())

	spantest.AssertStatus(t, sr.EndedNamed(t, "task.run"), codes.Error, "")
	sr.AssertAllEnded(t)
}
