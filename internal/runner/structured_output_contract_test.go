package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

const runnerOutputSchema = `{
  "type":"object",
  "properties":{"answer":{"type":"integer"}},
  "required":["answer"],
  "additionalProperties":false
}`

func seedStructuredPending(t *testing.T, store *storage.Storage) *models.Task {
	t.Helper()
	task := &models.Task{
		ID:           uuid.New(),
		Prompt:       "return a structured answer",
		Status:       models.TaskStatusPending,
		Priority:     1,
		CreatedAt:    time.Now().UTC(),
		OutputSchema: json.RawMessage(runnerOutputSchema),
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatal(err)
	}
	return task
}

type outputOrderingNotifier struct {
	store *storage.Storage
	done  chan error
}

func (n *outputOrderingNotifier) Notify(_ context.Context, ev notify.Event) error {
	id, err := uuid.Parse(ev.TaskID)
	if err == nil {
		var task *models.Task
		task, err = n.store.GetTask(id)
		if err == nil && (task.Status != models.TaskStatusSuccess || string(task.OutputJSON) != `{"answer":42}`) {
			err = fmt.Errorf("notification observed status=%s output=%s", task.Status, task.OutputJSON)
		}
	}
	n.done <- err
	return err
}

func TestStructuredSuccessCommitsOutputBeforeNotification(t *testing.T) {
	store := newTestStore(t)
	task := seedStructuredPending(t, store)
	notifier := &outputOrderingNotifier{store: store, done: make(chan error, 1)}
	runner := TaskRunnerFunc(func(_ context.Context, task *models.Task) (*models.LogSession, error) {
		return &models.LogSession{
			ID:       "s-" + task.ID.String(),
			Messages: []models.LogMessage{{Role: "assistant", Content: `{ "answer": 42 }`}},
		}, nil
	})
	pool := NewPool(store, runner, Config{
		MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour, Notifier: notifier,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	select {
	case err := <-notifier.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for success notification")
	}
	cancel()
	<-done

	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.TaskStatusSuccess || string(got.OutputJSON) != `{"answer":42}` {
		t.Fatalf("status=%s output=%s", got.Status, got.OutputJSON)
	}
}

func TestStructuredRunnerDefenseRejectsInvalidAndMissingOutput(t *testing.T) {
	for name, content := range map[string]string{
		"invalid": `{"answer":"wrong"}`,
		"missing": "",
	} {
		t.Run(name, func(t *testing.T) {
			store := newTestStore(t)
			task := seedStructuredPending(t, store)
			runner := TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
				session := &models.LogSession{ID: "bad"}
				if content != "" {
					session.Messages = []models.LogMessage{{Role: "assistant", Content: content}}
				}
				return session, nil
			})
			pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { pool.Run(ctx); close(done) }()

			waitFor(t, 3*time.Second, func() bool {
				dlq, _ := store.GetTasksByStatus(models.TaskStatusDeadLettered)
				return len(dlq) == 1
			})
			cancel()
			<-done
			got, _ := store.GetTask(task.ID)
			if got.Status != models.TaskStatusDeadLettered || len(got.OutputJSON) != 0 {
				t.Fatalf("status=%s output=%s", got.Status, got.OutputJSON)
			}
			if got.DeadLetterReason == nil || !strings.Contains(*got.DeadLetterReason, models.FailureOutputFormat) {
				t.Fatalf("dead-letter reason = %v", got.DeadLetterReason)
			}
		})
	}
}

func TestStructuredOutputLeaseLossCannotLandSuccess(t *testing.T) {
	store := newTestStore(t)
	task := seedStructuredPending(t, store)
	started := make(chan struct{})
	release := make(chan struct{})
	runner := TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		close(started)
		<-release
		return &models.LogSession{Messages: []models.LogMessage{{Role: "assistant", Content: `{"answer":42}`}}}, nil
	})
	notifier := &fakeNotifier{}
	pool := NewPool(store, runner, Config{
		MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour, Notifier: notifier,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	<-started
	if _, err := store.CancelTaskAtomic(task.ID, "lease taken by test"); err != nil {
		t.Fatal(err)
	}
	close(release)
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done

	got, _ := store.GetTask(task.ID)
	if got.Status == models.TaskStatusSuccess || len(got.OutputJSON) != 0 {
		t.Fatalf("lease-lost task landed success/output: status=%s output=%s", got.Status, got.OutputJSON)
	}
	if events := notifier.drain(); len(events) != 0 {
		t.Fatalf("lease-lost task fired notifications: %+v", events)
	}
}
