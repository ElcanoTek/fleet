package runner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/telemetry"
)

// An early failure (worktree, sandbox or MCP-scope prep) returns no session,
// so the pool stores a synthetic one. It still names the run and its trace,
// like a session the agent loop built (#2052).
func TestSyntheticLogCarriesRunAndTraceIDs(t *testing.T) {
	prev := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()); otel.SetTracerProvider(prev) })

	store := newTestStore(t)
	seeded := seedPending(t, store, 1)

	var mu sync.Mutex
	var runID, traceID string
	runner := TaskRunnerFunc(func(ctx context.Context, _ *models.Task) (*models.LogSession, error) {
		mu.Lock()
		runID, traceID = logging.Value(ctx, logging.KeyRunID), telemetry.TraceID(ctx)
		mu.Unlock()
		return nil, errors.New("sandbox prep failed")
	})

	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	var session *models.LogSession
	waitFor(t, 5*time.Second, func() bool {
		logs, _ := allLogs(store)
		session = logs[seeded[0].ID]
		return session != nil
	})
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if runID == "" || traceID == "" {
		t.Fatalf("the run's context carried run_id=%q trace_id=%q; want both", runID, traceID)
	}
	if session.ID != "session-synthetic-"+seeded[0].ID.String() {
		t.Fatalf("stored session %q, want the synthetic one", session.ID)
	}
	if session.RunID != runID || session.TraceID != traceID {
		t.Fatalf("synthetic session run_id=%q trace_id=%q, want %q / %q", session.RunID, session.TraceID, runID, traceID)
	}
}
