package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// The scheduler's content writers store text holding U+0000 without the
// character (internal/pgtext): a task's title, prompt and description (text)
// and tags (jsonb), a run's result, error and structured output, a run's log
// (jsonb — a model can write a NUL, and the log is how the run is read
// afterwards), a trigger's prompt template and an inbound email's subject.
// Keys are stored as given: a task name (the import key) or a trigger slug
// holding U+0000 still fails its write.
func TestSchedTextWithNULIsStoredWithoutIt(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()
	noNUL := func(t *testing.T, what, got, want string) {
		t.Helper()
		if strings.ContainsRune(got, 0) || got != want {
			t.Errorf("%s = %q, want %q", what, got, want)
		}
	}
	newTask := func(t *testing.T) *models.Task {
		t.Helper()
		task := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC()}
		if err := db.AddTask(ctx, task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		return task
	}

	t.Run("task create", func(t *testing.T) {
		task := &models.Task{
			ID: uuid.New(), Name: "nightly-report", Title: "Nightly\x00 report", Prompt: "Summarize\x00 the day.",
			Description: "Every\x00 night", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC(),
		}
		if err := db.AddTask(ctx, task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		got, err := db.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		noNUL(t, "name", got.Name, "nightly-report")
		noNUL(t, "title", got.Title, "Nightly report")
		noNUL(t, "prompt", got.Prompt, "Summarize the day.")
		noNUL(t, "description", got.Description, "Every night")
	})

	t.Run("task name is a key", func(t *testing.T) {
		task := &models.Task{ID: uuid.New(), Name: "nightly\x00", Prompt: "p", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC()}
		if err := db.AddTask(ctx, task); err == nil {
			t.Fatal("AddTask with a name holding U+0000 succeeded")
		}
	})

	t.Run("run outcome", func(t *testing.T) {
		task := newTask(t)
		result, failure := "answer\x00 text", "tool said\x00 no"
		task.Status = models.TaskStatusSuccess
		task.Result, task.ErrorMessage = &result, &failure
		task.OutputJSON = json.RawMessage(`{"summary":"ok\u0000"}`)
		if err := db.UpdateTask(ctx, task); err != nil {
			t.Fatalf("UpdateTask: %v", err)
		}
		got, err := db.GetTask(ctx, task.ID)
		if err != nil || got.Result == nil || got.ErrorMessage == nil {
			t.Fatalf("GetTask = %+v, %v", got, err)
		}
		noNUL(t, "result", *got.Result, "answer text")
		noNUL(t, "error message", *got.ErrorMessage, "tool said no")
		var out struct{ Summary string }
		if err := json.Unmarshal(got.OutputJSON, &out); err != nil {
			t.Fatalf("output json %q: %v", got.OutputJSON, err)
		}
		noNUL(t, "output summary", out.Summary, "ok")
	})

	t.Run("task tags (jsonb)", func(t *testing.T) {
		task := &models.Task{
			ID: uuid.New(), Prompt: "p", Tags: []string{"ops\x00"},
			Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC(),
		}
		if err := db.AddTask(ctx, task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		got, err := db.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if len(got.Tags) != 1 {
			t.Fatalf("tags = %q", got.Tags)
		}
		noNUL(t, "tag", got.Tags[0], "ops")
	})

	t.Run("run log (jsonb)", func(t *testing.T) {
		task := newTask(t)
		session := &models.LogSession{ID: task.ID.String(), Title: "run", Messages: []models.LogMessage{
			{ID: "m1", Role: "assistant", Content: "done\x00 here"},
		}}
		if err := db.AddLog(ctx, task.ID, session); err != nil {
			t.Fatalf("AddLog: %v", err)
		}
		got, err := db.GetLog(ctx, task.ID)
		if err != nil || got == nil || len(got.Messages) != 1 {
			t.Fatalf("GetLog = %+v, %v", got, err)
		}
		noNUL(t, "log message", got.Messages[0].Content, "done here")
	})

	t.Run("trigger event", func(t *testing.T) {
		task := newTask(t)
		trig := &models.TaskTrigger{ID: uuid.New(), TaskID: task.ID, Slug: "nul-slug", Secret: "s", PromptTemplate: "Handle\x00 {{.subject}}"}
		if err := db.CreateTrigger(ctx, trig); err != nil {
			t.Fatalf("CreateTrigger: %v", err)
		}
		ev := &models.TriggerEvent{TriggerID: trig.ID, IdempotencyKey: "k1", Sender: "a@example.com", Subject: "Re:\x00 invoice", MessageID: "<m1>"}
		if inserted, err := db.RecordTriggerEvent(ctx, ev); err != nil || !inserted {
			t.Fatalf("RecordTriggerEvent = %v, %v", inserted, err)
		}
		got, err := db.GetTrigger(ctx, trig.ID)
		if err != nil {
			t.Fatalf("GetTrigger: %v", err)
		}
		noNUL(t, "prompt template", got.PromptTemplate, "Handle {{.subject}}")
		var subject string
		if err := db.conn.QueryRowContext(ctx, `SELECT subject FROM trigger_events WHERE id = $1`, ev.ID).Scan(&subject); err != nil {
			t.Fatal(err)
		}
		noNUL(t, "subject", subject, "Re: invoice")
		if err := db.CreateTrigger(ctx, &models.TaskTrigger{ID: uuid.New(), TaskID: task.ID, Slug: "slug\x00", Secret: "s"}); err == nil {
			t.Fatal("CreateTrigger with a slug holding U+0000 succeeded")
		}
		// A webhook's slug is taken from its URL: one holding U+0000 matches
		// no trigger.
		if got, err := db.GetTriggerBySlug(ctx, models.TriggerKindWebhook, "nul-slug\x00"); err == nil || got != nil {
			t.Fatalf("GetTriggerBySlug(slug + NUL) = %v, %v; want a failure", got, err)
		}
	})
}
