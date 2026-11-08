package runner

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// A TaskRunner that panics is recovered by tryClaim after executeTask has
// returned; the task.run span must still record the failure, not end as a
// success before the recovery runs (#2052).
func TestTaskRunSpanRecordsPanic(t *testing.T) {
	prev := otel.GetTracerProvider()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()); otel.SetTracerProvider(prev) })

	store := newTestStore(t)
	seedPending(t, store, 1)
	runner := TaskRunnerFunc(func(context.Context, *models.Task) (*models.LogSession, error) {
		panic("runner exploded")
	})
	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	var span sdktrace.ReadOnlySpan
	waitFor(t, 5*time.Second, func() bool {
		for _, s := range sr.Ended() {
			if s.Name() == "task.run" {
				span = s
				return true
			}
		}
		return false
	})
	// The existing panic handling still marks the task errored.
	waitFor(t, 5*time.Second, func() bool {
		tasks, _ := store.GetTasksByStatus(models.TaskStatusError)
		return len(tasks) == 1
	})
	cancel()
	<-done

	if span.Status().Code != codes.Error {
		t.Fatalf("task.run status = %v after a runner panic, want Error", span.Status())
	}
}
