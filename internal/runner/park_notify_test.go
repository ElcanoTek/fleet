package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// The dead-letter notification of an occurrence whose chain parked says the
// schedule stopped and why (ADR-0073): for a malformed EXECUTION REQUIREMENTS
// line that is the validator's message, naming the identifier. A dead-letter
// that spawned its successor carries no such line.
func TestDeadLetterNotificationSaysTheScheduleStopped(t *testing.T) {
	store := newTestStore(t)
	malformed := &models.Task{
		ID: uuid.New(), Status: models.TaskStatusPending, Priority: 1, Recurrence: "@daily", Timezone: "UTC", CreatedAt: time.Now().UTC(),
		Prompt: "Refresh the page.\n" + models.ExecutionRequirementsMarker + "\n" + `{"mcp_servers":["fast_io + fastio_helpers","pages"]}`,
	}
	if _, err := store.AddTask(malformed); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	healthy := seedRecurringTask(t, store)

	notif := &fakeNotifier{}
	// The dispatch preflight's refusal, as scheduledrun reports it; any other
	// deterministic failure for the well-formed task.
	pool := NewPool(store, TaskRunnerFunc(func(_ context.Context, task *models.Task) (*models.LogSession, error) {
		if err := models.ValidateExecutionRequirements(task.Prompt); err != nil {
			return nil, err
		}
		return nil, errors.New("no model configured") // deterministic, not a transient sentinel
	}), Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour, Notifier: notif})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	waitFor(t, 3*time.Second, func() bool { return len(notif.drain()) == 2 })
	cancel()
	<-done

	byTask := map[string]notify.Event{}
	for _, ev := range notif.drain() {
		byTask[ev.TaskID] = ev
	}
	stopped := byTask[malformed.ID.String()]
	if stopped.Status != notify.StatusFailure || !strings.HasPrefix(stopped.Message, "Schedule stopped: ") ||
		!strings.Contains(stopped.Message, `invalid server or tool identifier "fast_io + fastio_helpers" in mcp_servers[0]`) {
		t.Fatalf("parked chain's notification = %+v, want a failure saying the schedule stopped and why", stopped)
	}
	if ev := byTask[healthy.ID.String()]; ev.Status != notify.StatusFailure || ev.Message != "" {
		t.Fatalf("a dead-letter that spawned its successor must not say the schedule stopped: %+v", ev)
	}
	got, err := store.GetTask(malformed.ID)
	if err != nil || got.RecurrenceParkedReason == nil || !strings.HasSuffix(stopped.Message, strings.Join(strings.Fields(*got.RecurrenceParkedReason), " ")) {
		t.Fatalf("the notification must carry the persisted reason: %+v %v", got, err)
	}
}
