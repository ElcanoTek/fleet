package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// promptWithRecipients mirrors web/src/app/orchestrator/taskEmailBlock.ts:
// the author's text, a blank line, then the CRITICAL ACTION delivery block.
func promptWithRecipients(body string, recipients ...string) string {
	block := "---\nCRITICAL ACTION\nemail:\n  action: send_report\n  tool: email\n" +
		"  instruction: \"The following action is MANDATORY after completing the core task.\"\n" +
		"  description: \"Send the full report and findings to the listed recipients.\"\n  recipients:\n"
	for _, r := range recipients {
		block += "    - " + r + "\n"
	}
	return body + "\n\n" + block + "---"
}

func addRecipientTask(t *testing.T, store *Storage, prompt string, createdAt time.Time, lineage uuid.UUID, createdBy *uuid.UUID) {
	t.Helper()
	task := &models.Task{
		ID:        uuid.New(),
		Prompt:    prompt,
		Status:    models.TaskStatusSuccess,
		CreatedAt: createdAt,
		LineageID: lineage,
		CreatedBy: createdBy,
	}
	if _, err := store.AddTaskWithContext(context.Background(), task); err != nil {
		t.Fatalf("add task: %v", err)
	}
}

// TestRecipientHistory pins what the task form may suggest: addresses from the
// delivery block only, counted once per job however many runs it has, newest
// first, inside the window, and scoped to the caller's own tasks on request.
func TestRecipientHistory(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	alice := uuid.New()
	bob := uuid.New()
	weekly := uuid.New()

	// Two runs of one recurring job: one lineage, so it counts once.
	addRecipientTask(t, store, promptWithRecipients("Weekly scan", "Kyle@Client.com", "ops@client.com"), now.Add(-48*time.Hour), weekly, &alice)
	addRecipientTask(t, store, promptWithRecipients("Weekly scan", "Kyle@Client.com", "ops@client.com"), now.Add(-24*time.Hour), weekly, &alice)
	// Bob's task, newest, plus an address that only appears in his prompt body:
	// a "    - " list in the body is the author's text, not the delivery list.
	addRecipientTask(t, store, promptWithRecipients("Notes:\n    - body@nowhere.com\n", "ops@client.com", "bob@agency.com"), now.Add(-time.Hour), uuid.New(), &bob)
	// Outside the window.
	addRecipientTask(t, store, promptWithRecipients("Old", "old@client.com"), now.Add(-400*24*time.Hour), uuid.New(), &alice)
	// No delivery block at all.
	addRecipientTask(t, store, "CRITICAL ACTION mentioned in passing, no block", now, uuid.New(), &alice)

	since := now.Add(-180 * 24 * time.Hour)
	all, err := store.ListRecipientHistory(ctx, db.RecipientHistoryScope{}, since, 50)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	got := map[string]int{}
	order := make([]string, 0, len(all))
	for _, ru := range all {
		got[ru.Email] = ru.TaskCount
		order = append(order, ru.Email)
	}
	want := map[string]int{"ops@client.com": 2, "bob@agency.com": 1, "kyle@client.com": 1}
	if len(got) != len(want) {
		t.Fatalf("history = %v, want %v", got, want)
	}
	for email, n := range want {
		if got[email] != n {
			t.Errorf("%s task_count = %d, want %d (all: %v)", email, got[email], n, got)
		}
	}
	// Bob's task is the newest, so its addresses lead; ties break on count.
	if order[0] != "ops@client.com" || order[1] != "bob@agency.com" || order[2] != "kyle@client.com" {
		t.Errorf("order = %v, want newest first", order)
	}

	mine, err := store.ListRecipientHistory(ctx, db.RecipientHistoryScope{VisibleToUserID: &alice}, since, 50)
	if err != nil {
		t.Fatalf("scoped history: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("alice's history = %v, want only her two addresses", mine)
	}
	for _, ru := range mine {
		if ru.Email == "bob@agency.com" {
			t.Errorf("alice must not learn bob's recipients: %v", mine)
		}
	}

	limited, err := store.ListRecipientHistory(ctx, db.RecipientHistoryScope{}, since, 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("limit 1 = %v, %v", limited, err)
	}
}
