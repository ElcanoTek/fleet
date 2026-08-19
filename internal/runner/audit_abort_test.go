// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// The production failure (#1151): task 3d767956 built a complete, schema-valid
// 978 KB payload, declined to dispatch it, printed four quality flags and
// ABORTED_WITH_FLAGS, and called confirm_audit(success=false). The task row
// said status: success, result: "Task completed successfully". The agent was
// right and said so; the system discarded that and reported green, which is how
// a client dashboard froze for days with every run "succeeding".
func TestAuditAbortIsNotSuccess(t *testing.T) {
	store := newTestStore(t)
	task := &models.Task{
		ID:         uuid.New(),
		Prompt:     "refresh the dashboard",
		Status:     models.TaskStatusPending,
		Priority:   1,
		CreatedAt:  time.Now().UTC(),
		MaxRetries: 3, // retries available — the abort must NOT use them
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	const summary = "Page unchanged: no file-backed update tool available; live version remains 259"
	runner := TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		return &models.LogSession{ID: "s-" + tk.ID.String()},
			fmt.Errorf("%w: %s", agentcore.ErrAuditAborted, summary)
	})

	pool := NewPool(store, runner, Config{
		MaxConcurrentAgents: 1,
		PollInterval:        20 * time.Millisecond,
		LeaseRenewInterval:  time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	waitFor(t, 5*time.Second, func() bool {
		got, err := store.GetTask(task.ID)
		return err == nil && got != nil && got.Status.IsTerminal()
	})
	cancel()
	<-done

	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != models.TaskStatusError {
		t.Fatalf("status = %s, want %s: an agent's own terminal verdict must reach the record",
			got.Status, models.TaskStatusError)
	}
	// The agent's summary is the single most useful sentence about the run.
	if got.ErrorMessage == nil || !strings.Contains(*got.ErrorMessage, summary) {
		t.Errorf("message = %v, want it to carry the agent's summary %q", got.ErrorMessage, summary)
	}
	if got.ErrorMessage == nil || !strings.Contains(*got.ErrorMessage, "self-audit") {
		t.Errorf("message = %v, want it to name the self-audit as the cause", got.ErrorMessage)
	}
	// Deterministic: the run reached a conclusion about its own work, so a
	// retry would only spend another window reaching the same one.
	if got.AttemptCount != 0 {
		t.Errorf("AttemptCount = %d, want 0 (an abort must not consume retry attempts)", got.AttemptCount)
	}
	if got.Status == models.TaskStatusDeadLettered {
		t.Error("an audit abort must never dead-letter: nothing malfunctioned")
	}
}

// An abort is the machinery working, not an application failure — paging on it
// would train everyone to ignore the page.
func TestAuditAbortIsNotSentryReportable(t *testing.T) {
	err := fmt.Errorf("%w: nothing to publish", agentcore.ErrAuditAborted)
	if reportableRunFailure(err, false, false) {
		t.Error("a deliberate self-audit abort must not be reported as an application failure")
	}
	// A genuine failure still is.
	if !reportableRunFailure(fmt.Errorf("provider exploded"), false, false) {
		t.Error("an ordinary run failure must stay reportable")
	}
}

// The generic constant was written OVER an agent summary that said, in detail,
// that the page was unchanged and why — the one field that would have told an
// operator a dashboard had stopped refreshing while every run showed green.
func TestSuccessMessageKeepsTheAgentsOwnSummary(t *testing.T) {
	const fallback = "Task completed successfully"
	summary := "Source unchanged since 2026-08-13.\n\nPublished nothing; reported source_not_updated."
	session := &models.LogSession{Messages: []models.LogMessage{
		{Role: "user", Content: "refresh"},
		{Role: "assistant", Content: summary},
	}}
	got := successMessage(session)
	if !strings.Contains(got, "source_not_updated") {
		t.Errorf("successMessage = %q, want the agent's own words", got)
	}
	// A task list renders result on one line.
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("successMessage = %q, want no line breaks", got)
	}

	// No closing text: the historical constant, unchanged.
	if got := successMessage(&models.LogSession{}); got != fallback {
		t.Errorf("empty session successMessage = %q, want %q", got, fallback)
	}
	if got := successMessage(nil); got != fallback {
		t.Errorf("nil session successMessage = %q, want %q", got, fallback)
	}

	// A structured run's final text IS its payload, already on the row in
	// output_json; repeating it as prose would only make the list unreadable.
	structured := &models.LogSession{Messages: []models.LogMessage{
		{Role: "assistant", Content: `{"rows": 846, "published": false}`},
	}}
	if got := successMessage(structured); got != fallback {
		t.Errorf("structured successMessage = %q, want %q", got, fallback)
	}

	// Model-authored text is bounded, and bounded by RUNES so a multi-byte
	// character is never cut into invalid UTF-8.
	long := &models.LogSession{Messages: []models.LogMessage{
		{Role: "assistant", Content: strings.Repeat("é", maxTerminalMessageRunes+50)},
	}}
	bounded := successMessage(long)
	if len([]rune(bounded)) != maxTerminalMessageRunes+1 { // +1 for the ellipsis
		t.Errorf("bounded length = %d runes, want %d", len([]rune(bounded)), maxTerminalMessageRunes+1)
	}
	if !strings.HasSuffix(bounded, "…") {
		t.Errorf("bounded = %q, want a truncation marker", bounded)
	}
}
