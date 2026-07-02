package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

type replyCall struct{ to, subject, body, inReplyTo string }

type fakeReplier struct {
	mu    sync.Mutex
	calls []replyCall
}

func (f *fakeReplier) ReplyToEmailEvent(_ context.Context, to, subject, body, inReplyTo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, replyCall{to, subject, body, inReplyTo})
	return nil
}

func (f *fakeReplier) snapshot() []replyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]replyCall(nil), f.calls...)
}

// runReplyPool runs one pool tick with the given runner + replier and waits for
// `cond`.
func runReplyPool(t *testing.T, store *storage.Storage, runner TaskRunner, replier EmailReplier, cond func() bool) {
	t.Helper()
	pool := NewPool(store, runner, Config{
		MaxConcurrentAgents: 2,
		PollInterval:        20 * time.Millisecond,
		LeaseRenewInterval:  time.Hour,
		EmailReplier:        replier,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	waitFor(t, 3*time.Second, cond)
	cancel()
	<-done
}

// answeringRunner returns a runner that produces a session whose final assistant
// message is `answer`, and a getter reporting whether it ran.
func answeringRunner(answer string) (TaskRunner, func() bool) {
	var (
		mu  sync.Mutex
		ran bool
	)
	r := TaskRunnerFunc(func(_ context.Context, task *models.Task) (*models.LogSession, error) {
		mu.Lock()
		ran = true
		mu.Unlock()
		return &models.LogSession{
			ID: "s-" + task.ID.String(),
			Messages: []models.LogMessage{
				{ID: "u1", Role: "user", Content: task.Prompt},
				{ID: "a1", Role: "assistant", Content: answer},
			},
		}, nil
	})
	return r, func() bool { mu.Lock(); defer mu.Unlock(); return ran }
}

// TestMaybeReplyToEmailEvent_Replies: an email-triggered run's success sends a
// reply to the original sender carrying the run's final answer, threaded to the
// original Message-ID.
func TestMaybeReplyToEmailEvent_Replies(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Template + email trigger, then spawn a run and link an inbound event to it.
	template := &models.Task{ID: uuid.New(), Prompt: "handle mail", Status: models.TaskStatusScheduled, Priority: 1, TriggerType: models.TriggerTypeWebhook}
	if _, err := store.AddTask(template); err != nil {
		t.Fatalf("AddTask template: %v", err)
	}
	trigID := uuid.New()
	if err := store.CreateTrigger(ctx, &models.TaskTrigger{ID: trigID, TaskID: template.ID, Slug: "r1", Secret: "s", Kind: models.TriggerKindEmail, EmailPolicy: &models.EmailTriggerPolicy{}}); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	runID, err := store.SpawnEmailRun(ctx, &models.TaskTrigger{TaskID: template.ID}, "do the work", false)
	if err != nil {
		t.Fatalf("SpawnEmailRun: %v", err)
	}
	ev := &models.TriggerEvent{TriggerID: trigID, IdempotencyKey: "<orig@mail>", Sender: "asker@corp.com", Subject: "Please help", MessageID: "<orig@mail>"}
	if _, err := store.RecordTriggerEvent(ctx, ev); err != nil {
		t.Fatalf("RecordTriggerEvent: %v", err)
	}
	if err := store.SetTriggerEventRunID(ctx, ev.ID, runID); err != nil {
		t.Fatalf("SetTriggerEventRunID: %v", err)
	}

	replier := &fakeReplier{}
	runner, _ := answeringRunner("Here is your result.")
	runReplyPool(t, store, runner, replier, func() bool { return len(replier.snapshot()) > 0 })

	calls := replier.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 reply, got %d", len(calls))
	}
	c := calls[0]
	if c.to != "asker@corp.com" || c.body != "Here is your result." || c.inReplyTo != "<orig@mail>" || c.subject != "Please help" {
		t.Errorf("reply args wrong: %+v", c)
	}
}

// TestMaybeReplyToEmailEvent_NonEmailRunNoReply: a plain pending run (not spawned
// from an email trigger) never triggers a reply.
func TestMaybeReplyToEmailEvent_NonEmailRunNoReply(t *testing.T) {
	store := newTestStore(t)
	task := &models.Task{ID: uuid.New(), Prompt: "plain", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	replier := &fakeReplier{}
	runner, ran := answeringRunner("done")
	runReplyPool(t, store, runner, replier, ran)
	// The run executed; give the (would-be) off-thread reply lookup a beat to run
	// and find nothing before asserting it never fired.
	time.Sleep(300 * time.Millisecond)

	if got := replier.snapshot(); len(got) != 0 {
		t.Errorf("non-email run should not reply, got %d calls", len(got))
	}
}
