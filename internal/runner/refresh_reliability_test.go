package runner

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// connectorOutageErr is the dispatch error scheduledrun returns when a
// declared server failed to connect transiently.
func connectorOutageErr() error {
	return fmt.Errorf("execution requirements: unavailable in the task's MCP/native tool roster: server pages; "+
		"server pages failed to connect this run (DNS lookup failed (no such host)) — a transient connector outage: %w", agentcore.ErrConnectorUnavailable)
}

func runPoolUntil(t *testing.T, store *storage.Storage, runner TaskRunner, notifier Notifier, until func() bool) {
	t.Helper()
	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour, Notifier: notifier})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	waitFor(t, 3*time.Second, until)
	cancel()
	<-done
}

// TestConnectorOutageReRunsWithoutSpendingRetries: the requirements miss a
// connector outage explains re-queues the same occurrence a few minutes out
// on the infra budget — attempt_count and max_retries untouched — instead of
// dead-lettering it as "non-retryable failure (terminal)".
func TestConnectorOutageReRunsWithoutSpendingRetries(t *testing.T) {
	store := newTestStore(t)
	seedTask(t, store, 0, 0) // max_retries 0: the task's own budget is empty

	runPoolUntil(t, store, TaskRunnerFunc(func(context.Context, *models.Task) (*models.LogSession, error) {
		return nil, connectorOutageErr()
	}), nil, func() bool {
		s, _ := store.GetTasksByStatus(models.TaskStatusScheduled)
		return len(s) == 1
	})

	scheduled, _ := store.GetTasksByStatus(models.TaskStatusScheduled)
	if len(scheduled) != 1 {
		t.Fatalf("want the occurrence re-queued, got %d scheduled", len(scheduled))
	}
	task := scheduled[0]
	if task.InfraRetryCount != 1 || task.AttemptCount != 0 {
		t.Fatalf("infra=%d attempts=%d, want 1/0 — a connector outage must not spend max_retries", task.InfraRetryCount, task.AttemptCount)
	}
	if task.ScheduledFor == nil || time.Until(*task.ScheduledFor) < 4*time.Minute || time.Until(*task.ScheduledFor) > 6*time.Minute {
		t.Fatalf("re-run at %v, want about five minutes out", task.ScheduledFor)
	}
	if task.ErrorMessage == nil || !strings.Contains(*task.ErrorMessage, "server pages failed to connect this run") {
		t.Fatalf("the re-run message must name the server and the connect error: %v", task.ErrorMessage)
	}
	if dl, _ := store.GetTasksByStatus(models.TaskStatusDeadLettered); len(dl) != 0 {
		t.Fatal("a connector outage with infra re-runs left must not dead-letter")
	}
}

// TestConnectorOutageDeadLettersAsSuchOnceTheInfraBudgetIsSpent: after the
// bounded infra re-runs the occurrence dead-letters with run outcome
// connector_unavailable (which the park breaker skips) and a reason that
// names the outage.
func TestConnectorOutageDeadLettersAsSuchOnceTheInfraBudgetIsSpent(t *testing.T) {
	store := newTestStore(t)
	seedTask(t, store, 0, 0)
	if _, err := store.DB().Conn().ExecContext(t.Context(), `UPDATE tasks SET infra_retry_count = $1`, maxConnectorInfraRetries); err != nil {
		t.Fatal(err)
	}
	runPoolUntil(t, store, TaskRunnerFunc(func(context.Context, *models.Task) (*models.LogSession, error) {
		return nil, connectorOutageErr()
	}), nil, func() bool {
		d, _ := store.GetTasksByStatus(models.TaskStatusDeadLettered)
		return len(d) == 1
	})
	dl, _ := store.GetTasksByStatus(models.TaskStatusDeadLettered)
	task := dl[0]
	if !task.IsRunOutcome(models.RunOutcomeConnectorUnavailable) {
		t.Fatalf("run outcome = %v, want connector_unavailable", task.RunOutcome)
	}
	if task.DeadLetterReason == nil || !strings.HasPrefix(*task.DeadLetterReason, "connector unavailable after 2 infra re-run(s) (connector_unavailable): ") ||
		!strings.Contains(*task.DeadLetterReason, "server pages failed to connect this run") {
		t.Fatalf("dead_letter_reason = %v", task.DeadLetterReason)
	}
}

func TestClassifyConnectorUnavailable(t *testing.T) {
	if got := classifyFailure(connectorOutageErr()); got != models.FailureConnectorUnavailable {
		t.Fatalf("classifyFailure = %q, want %q", got, models.FailureConnectorUnavailable)
	}
	if reportableRunFailure(connectorOutageErr(), false, false) {
		t.Fatal("a connector outage is weather, not a Sentry-worthy failure")
	}
}

func blockedSession(id uuid.UUID) *models.LogSession {
	return &models.LogSession{
		ID:               "s-" + id.String(),
		Messages:         []models.LogMessage{{Role: "assistant", Content: "Source unreachable; the page was not published."}},
		RunOutcome:       models.RunOutcomeBlocked,
		RunOutcomeDetail: "outcome=source_unreachable: mailbox had no report for 2026-10-04",
	}
}

// TestBlockedRunIsRecordedAndTheThirdInARowAlerts: a run whose declared
// completion clause recorded a blocked outcome lands as success with run
// outcome "blocked", its result flagged, and the third consecutive blocked
// occurrence of a recurring lineage notifies as a failure — without parking.
func TestBlockedRunIsRecordedAndTheThirdInARowAlerts(t *testing.T) {
	store := newTestStore(t)
	// Two earlier blocked occurrences of the lineage, then the current one.
	var prev *uuid.UUID
	for i := 0; i < 2; i++ {
		past := &models.Task{
			ID: uuid.New(), Prompt: "refresh", Status: models.TaskStatusSuccess, Priority: 1, Recurrence: "@daily",
			Timezone: "UTC", CreatedAt: time.Now().UTC(), PreviousOccurrenceID: prev,
		}
		if _, err := store.AddTask(past); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Conn().ExecContext(t.Context(), `UPDATE tasks SET run_outcome = 'blocked' WHERE id = $1`, past.ID); err != nil {
			t.Fatal(err)
		}
		id := past.ID
		prev = &id
	}
	current := &models.Task{
		ID: uuid.New(), Prompt: "refresh", Status: models.TaskStatusPending, Priority: 1, Recurrence: "@daily",
		Timezone: "UTC", CreatedAt: time.Now().UTC(), PreviousOccurrenceID: prev,
	}
	if _, err := store.AddTask(current); err != nil {
		t.Fatal(err)
	}

	notif := &fakeNotifier{}
	runPoolUntil(t, store, TaskRunnerFunc(func(_ context.Context, task *models.Task) (*models.LogSession, error) {
		return blockedSession(task.ID), nil
	}), notif, func() bool { return len(notif.drain()) == 1 })

	got, err := store.GetTask(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.TaskStatusSuccess || !got.IsRunOutcome(models.RunOutcomeBlocked) {
		t.Fatalf("status=%s outcome=%v, want success + blocked", got.Status, got.RunOutcome)
	}
	if got.RunOutcomeDetail == nil || !strings.Contains(*got.RunOutcomeDetail, "source_unreachable") {
		t.Fatalf("run_outcome_detail = %v", got.RunOutcomeDetail)
	}
	if got.Result == nil || !strings.HasPrefix(*got.Result, "[blocked] Source unreachable") {
		t.Fatalf("result = %v, want the [blocked] flag in front of the summary", got.Result)
	}
	if got.RecurrenceParkedAt != nil {
		t.Fatal("a blocked streak must not park the schedule")
	}
	ev := notif.drain()[0]
	if ev.Status != notify.StatusFailure || !strings.HasPrefix(ev.Message, "Blocked 3 runs in a row") || !strings.Contains(ev.Message, "source_unreachable") {
		t.Fatalf("third blocked occurrence notified %+v, want a failure-class blocked-streak alert", ev)
	}
}

// TestSingleBlockedRunNotifiesAsBlockedSuccess: a first blocked run is a
// success event whose message says it was blocked.
func TestSingleBlockedRunNotifiesAsBlockedSuccess(t *testing.T) {
	store := newTestStore(t)
	task := seedRecurringTask(t, store)
	notif := &fakeNotifier{}
	runPoolUntil(t, store, TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		return blockedSession(tk.ID), nil
	}), notif, func() bool { return len(notif.drain()) == 1 })
	ev := notif.drain()[0]
	if ev.TaskID != task.ID.String() || ev.Status != notify.StatusSuccess || !strings.HasPrefix(ev.Message, "Blocked: outcome=source_unreachable") {
		t.Fatalf("event = %+v, want a success that says Blocked", ev)
	}
}
