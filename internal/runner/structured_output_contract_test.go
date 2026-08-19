package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agentcore"
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

func compactRunnerJSON(raw []byte) string {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return ""
	}
	return out.String()
}

func retainedTerminalStatus(t *testing.T, pool *Pool, taskID uuid.UUID) string {
	t.Helper()
	stream, ok := pool.StreamRegistry().Lookup(taskID)
	if !ok {
		t.Fatal("task stream was not retained")
	}
	buf, ok := stream.(*taskStreamBuffer)
	if !ok {
		t.Fatalf("task stream type = %T", stream)
	}
	buf.mu.Lock()
	if len(buf.events) == 0 {
		buf.mu.Unlock()
		t.Fatal("task emitted no lifecycle events")
	}
	last := append([]byte(nil), buf.events[len(buf.events)-1].Data...)
	buf.mu.Unlock()
	var terminal map[string]any
	if err := json.Unmarshal(last, &terminal); err != nil {
		t.Fatalf("decode terminal stream frame %q: %v", last, err)
	}
	status, _ := terminal["status"].(string)
	return status
}

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
		if err == nil && (task.Status != models.TaskStatusSuccess || compactRunnerJSON(task.OutputJSON) != `{"answer":42}`) {
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
	if got.Status != models.TaskStatusSuccess || compactRunnerJSON(got.OutputJSON) != `{"answer":42}` {
		t.Fatalf("status=%s output=%s", got.Status, got.OutputJSON)
	}
	if status := retainedTerminalStatus(t, pool, task.ID); status != "succeeded" {
		t.Fatalf("committed terminal stream status = %q", status)
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
	if status := retainedTerminalStatus(t, pool, task.ID); status != "failed" {
		t.Fatalf("lease-lost terminal stream status = %q, want failed", status)
	}
}

func TestStructuredSamePoolReclaimCannotABACommitOrNotify(t *testing.T) {
	store := newTestStore(t)
	task := seedStructuredPending(t, store)
	oldAtFence := make(chan struct{})
	releaseOld := make(chan struct{})
	var calls atomic.Int32
	runner := TaskRunnerFunc(func(_ context.Context, claimed *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		answer := claimed.AttemptCount + 1
		return &models.LogSession{Messages: []models.LogMessage{{
			Role: "assistant", Content: fmt.Sprintf(`{"answer":%d}`, answer),
		}}}, nil
	})
	notifier := &fakeNotifier{}
	pool := NewPool(store, runner, Config{
		MaxConcurrentAgents: 2, PollInterval: 10 * time.Millisecond, LeaseRenewInterval: time.Hour, Notifier: notifier,
	})
	pool.beforeSuccessCommit = func(claimed *models.Task, _ uuid.UUID) {
		if claimed.AttemptCount != 0 {
			return
		}
		close(oldAtFence)
		<-releaseOld // A has passed stillOwns but has not begun reportSuccess.
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	releaseOldRun := func() {
		select {
		case <-releaseOld:
		default:
			close(releaseOld)
		}
	}
	t.Cleanup(func() {
		releaseOldRun()
		cancel()
		<-done
	})

	select {
	case <-oldAtFence:
	case <-time.After(3 * time.Second):
		t.Fatal("old claim did not reach the post-stillOwns fence")
	}
	stale, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale.MaxRetries = 1 // recovery re-queues only below the retry budget (#1116)
	stale.LeaseExpiresAt = ptrTime(time.Now().UTC().Add(-time.Minute))
	if _, err := store.UpdateTask(stale); err != nil {
		t.Fatal(err)
	}
	if recovered, err := pool.RecoverExpiredLeases(); err != nil || recovered != 1 {
		t.Fatalf("RecoverExpiredLeases = %d, %v", recovered, err)
	}

	// The same pool reclaims with a fresh persisted UUID owner. B completes
	// while A remains suspended after its local precheck.
	waitFor(t, 3*time.Second, func() bool {
		got, err := store.GetTask(task.ID)
		return err == nil && got.Status == models.TaskStatusSuccess && compactRunnerJSON(got.OutputJSON) == `{"answer":2}`
	})
	waitFor(t, 3*time.Second, func() bool { return len(notifier.drain()) == 1 })
	releaseOldRun()
	cancel()
	<-done // drains stale A's rejected terminal path before assertions
	if calls.Load() != 2 {
		t.Fatalf("runner calls = %d, want old and reclaimed claims", calls.Load())
	}

	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.TaskStatusSuccess || compactRunnerJSON(got.OutputJSON) != `{"answer":2}` {
		t.Fatalf("stale claim clobbered fresh success: status=%s output=%s", got.Status, got.OutputJSON)
	}
	events := notifier.drain()
	if len(events) != 1 || events[0].Status != notify.StatusSuccess {
		t.Fatalf("same-pool ABA emitted duplicate/wrong notifications: %+v", events)
	}
}

func TestStructuredPersistenceFailureIsClassifiedBeforeSideEffects(t *testing.T) {
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
	releaseRun := func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}
	t.Cleanup(func() {
		releaseRun()
		cancel()
		<-done
	})
	<-started

	// Reject only the success UPDATE. The validated output transaction rolls
	// back, then the runner must re-read, classify structured persistence, and
	// land the normal failure/DLQ path under the same still-held lease.
	const constraint = "runner_test_reject_structured_success"
	if _, err := store.DB().Conn().ExecContext(t.Context(),
		"ALTER TABLE tasks DROP CONSTRAINT IF EXISTS "+constraint); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Conn().ExecContext(t.Context(),
		"ALTER TABLE tasks ADD CONSTRAINT "+constraint+" CHECK (status <> 'success')"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.DB().Conn().ExecContext(context.Background(),
			"ALTER TABLE tasks DROP CONSTRAINT IF EXISTS "+constraint)
	})
	releaseRun()

	waitFor(t, 3*time.Second, func() bool {
		got, err := store.GetTask(task.ID)
		return err == nil && got.Status == models.TaskStatusDeadLettered
	})
	waitFor(t, 3*time.Second, func() bool { return len(notifier.drain()) == 1 })
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.OutputJSON) != 0 {
		t.Fatalf("failed success transaction leaked output_json: %s", got.OutputJSON)
	}
	if got.DeadLetterReason == nil || !strings.Contains(*got.DeadLetterReason, models.FailureOutputPersist) {
		t.Fatalf("dead-letter reason = %v, want %s", got.DeadLetterReason, models.FailureOutputPersist)
	}
	events := notifier.drain()
	if len(events) != 1 || events[0].Status != notify.StatusFailure {
		t.Fatalf("persistence failure notifications = %+v", events)
	}
	if status := retainedTerminalStatus(t, pool, task.ID); status != "failed" {
		t.Fatalf("persistence failure terminal stream status = %q, want failed", status)
	}
}

func TestStructuredCommitOutcomeUnknownSameClaimClassifiesPersistence(t *testing.T) {
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
	releaseRun := func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}
	t.Cleanup(func() {
		releaseRun()
		cancel()
		<-done
	})
	<-started

	// A deferred constraint trigger makes the success UPDATE itself appear to
	// succeed and raises only from tx.Commit. PostgreSQL rolls that transaction
	// back, leaving the exact same persisted lease token on a nonterminal row.
	// That reread is the sole outcome-unknown state in which this claim may enter
	// the explicit structured_output_persistence failure path.
	const trigger = "runner_test_defer_structured_success"
	const function = "runner_test_fail_structured_success_commit"
	dropDeferredFailure := func(ctx context.Context) {
		_, _ = store.DB().Conn().ExecContext(ctx, "DROP TRIGGER IF EXISTS "+trigger+" ON tasks")
		_, _ = store.DB().Conn().ExecContext(ctx, "DROP FUNCTION IF EXISTS "+function+"()")
	}
	dropDeferredFailure(t.Context())
	if _, err := store.DB().Conn().ExecContext(t.Context(), `
		CREATE FUNCTION `+function+`() RETURNS trigger AS $$
		BEGIN
			IF NEW.status = 'success' THEN
				RAISE EXCEPTION 'test deferred structured success rejection';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Conn().ExecContext(t.Context(), `
		CREATE CONSTRAINT TRIGGER `+trigger+`
		AFTER UPDATE ON tasks
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION `+function+`()`); err != nil {
		dropDeferredFailure(t.Context())
		t.Fatal(err)
	}
	t.Cleanup(func() { dropDeferredFailure(context.Background()) })
	releaseRun()

	waitFor(t, 3*time.Second, func() bool {
		got, err := store.GetTask(task.ID)
		return err == nil && got.Status == models.TaskStatusDeadLettered
	})
	waitFor(t, 3*time.Second, func() bool { return len(notifier.drain()) == 1 })
	cancel()
	<-done

	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.OutputJSON) != 0 {
		t.Fatalf("commit-failed success leaked output_json: %s", got.OutputJSON)
	}
	if got.DeadLetterReason == nil || !strings.Contains(*got.DeadLetterReason, models.FailureOutputPersist) {
		t.Fatalf("dead-letter reason = %v, want %s", got.DeadLetterReason, models.FailureOutputPersist)
	}
	events := notifier.drain()
	if len(events) != 1 || events[0].Status != notify.StatusFailure {
		t.Fatalf("commit-unknown notifications = %+v", events)
	}
	if status := retainedTerminalStatus(t, pool, task.ID); status != "failed" {
		t.Fatalf("commit-unknown terminal stream status = %q, want failed", status)
	}
}

// TestStructuredOutputPrefersDriverHandoffOverRedactedText pins the #797
// review fix: the runner validates the session's OutputJSON (the exact bytes
// agentcore validated, redacted once at the driver boundary) — never a
// re-parse of the redacted display text, which can corrupt or fail an
// already-valid contract.
func TestStructuredOutputPrefersDriverHandoffOverRedactedText(t *testing.T) {
	task := &models.Task{OutputSchema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}`)}
	session := &models.LogSession{
		OutputJSON: `{"answer":42}`,
		// Display text mangled by redaction: re-parsing it would fail.
		Messages: []models.LogMessage{{Role: "assistant", Content: `{"answer":[REDACTED]}`}},
	}
	out, err := validateStructuredRunOutput(task, session)
	if err != nil {
		t.Fatalf("handoff bytes must win over redacted text: %v", err)
	}
	if string(out) != `{"answer":42}` {
		t.Fatalf("out = %s", out)
	}
}

// TestStructuredOutputRedactionCorruptionFailsLoudly pins the fail-closed
// half: when redaction itself broke the validated JSON (the declared output
// carried secret material), the run fails with an explicit diagnostic instead
// of committing corrupted-but-schema-shaped output.
func TestStructuredOutputRedactionCorruptionFailsLoudly(t *testing.T) {
	task := &models.Task{OutputSchema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}`)}
	session := &models.LogSession{
		OutputJSON: `{"answer":"[REDACTED]"}`,
		Messages:   []models.LogMessage{{Role: "assistant", Content: `{"answer":1}`}},
	}
	_, err := validateStructuredRunOutput(task, session)
	if err == nil {
		t.Fatal("corrupted handoff must fail")
	}
	if !errors.Is(err, agentcore.ErrStructuredOutputInvalid) {
		t.Fatalf("want ErrStructuredOutputInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "redacted secret material") {
		t.Fatalf("diagnostic must name redaction: %v", err)
	}
}
