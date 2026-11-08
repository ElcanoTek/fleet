package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The records a run writes after it starts — here the reportStatus failure
// when the run's lease is not held — carry the run's correlation, run_id
// included, so a retry of the same task is told apart from the attempt
// before it (#2052).
func TestReportStatusFailureRecordCarriesRunID(t *testing.T) {
	var logs lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	store := newTestStore(t)
	task := seedPending(t, store, 1)[0]
	runner := TaskRunnerFunc(func(context.Context, *models.Task) (*models.LogSession, error) {
		return nil, errors.New("not reached for the assertion")
	})
	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: time.Hour, LeaseRenewInterval: time.Hour})

	// A token that does not hold the task's lease: reporting "running" fails.
	token := uuid.New()
	pool.executeTask(context.Background(), task, token)

	var found bool
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, "runner: failed to report running for task") {
			continue
		}
		found = true
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("record is not JSON: %v: %s", err, line)
		}
		if rec["run_id"] != token.String() || rec["task_id"] != task.ID.String() {
			t.Fatalf("reportStatus-failure record run_id=%v task_id=%v, want %s / %s: %s", rec["run_id"], rec["task_id"], token, task.ID, line)
		}
	}
	if !found {
		t.Fatalf("no reportStatus-failure record; log:\n%s", logs.String())
	}
}
