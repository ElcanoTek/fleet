package runner

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/telemetry/spantest"
)

// A driver that swallows its cancellation (returns nil after the run's
// context ends) still has its task routed to a failure path — wall-clock
// timeout or interruption. The task.run span must record that outcome, not
// export the run as a success (#2052).
func TestTaskRunSpanRecordsCancelledOutcomeWithNilRunError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wall  time.Duration
		force bool
	}{
		{name: "wall-clock timeout", wall: 100 * time.Millisecond},
		{name: "force-cancelled", wall: -1, force: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sr := spantest.Install(t)
			store := newTestStore(t)
			seedPending(t, store, 1)
			started := make(chan struct{}, 1)
			runner := TaskRunnerFunc(func(ctx context.Context, _ *models.Task) (*models.LogSession, error) {
				started <- struct{}{}
				<-ctx.Done()
				return &models.LogSession{ID: "s"}, nil // swallows the cancellation
			})
			pool := NewPool(store, runner, Config{
				MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour,
				WallClockTimeout: tc.wall,
			})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { pool.Run(ctx); close(done) }()
			<-started
			if tc.force {
				pool.ForceCancel()
			}
			span := sr.EndedNamed(t, "task.run")
			cancel()
			<-done
			spantest.AssertStatus(t, span, codes.Error, "")
		})
	}
}
