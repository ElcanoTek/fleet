// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package models

import (
	"testing"
	"time"
)

// TestNewTaskParksGatedTasksOnSchedulerPath pins RunIf's enforcement contract:
// a gated cron task must ALWAYS leave NewTask on the scheduler's promotion
// path (status=scheduled, non-nil scheduled_for), because ProcessScheduledTasks
// is the only place the gate is evaluated. Before the fix a gated task with no
// schedule (an immediate create, a rerun/clone, a trigger spawn) minted as
// pending and the worker claimed it with the gate never run.
func TestNewTaskParksGatedTasksOnSchedulerPath(t *testing.T) {
	gate := &RunIf{Command: "true", TimeoutSeconds: 30}

	t.Run("gated immediate create is parked scheduled-for-now", func(t *testing.T) {
		before := time.Now().UTC()
		task := NewTask(TaskCreate{Prompt: "gated run-now", RunIf: gate})
		after := time.Now().UTC()
		if task.Status != TaskStatusScheduled {
			t.Fatalf("status = %q, want scheduled (a pending gated task would never have its gate evaluated)", task.Status)
		}
		if task.ScheduledFor == nil {
			t.Fatal("scheduled_for = nil; GetScheduledTasks requires it non-nil, so the task would park inert forever")
		}
		if task.ScheduledFor.Before(before) || task.ScheduledFor.After(after) {
			t.Errorf("scheduled_for = %v, want defaulted to now (in [%v, %v])", task.ScheduledFor, before, after)
		}
	})

	t.Run("gated future-scheduled create keeps its schedule", func(t *testing.T) {
		future := time.Now().UTC().Add(time.Hour)
		task := NewTask(TaskCreate{Prompt: "gated scheduled", RunIf: gate, ScheduledFor: &future})
		if task.Status != TaskStatusScheduled {
			t.Errorf("status = %q, want scheduled", task.Status)
		}
		if task.ScheduledFor == nil || !task.ScheduledFor.Equal(future) {
			t.Errorf("scheduled_for = %v, want the explicit %v", task.ScheduledFor, future)
		}
	})

	t.Run("gated webhook template stays inert", func(t *testing.T) {
		task := NewTask(TaskCreate{Prompt: "gated template", RunIf: gate, TriggerType: TriggerTypeWebhook})
		if task.Status != TaskStatusScheduled {
			t.Errorf("status = %q, want scheduled (inert)", task.Status)
		}
		// The template itself is never promoted; its gate applies to spawned
		// runs. Defaulting scheduled_for here would surface the template as due.
		if task.ScheduledFor != nil {
			t.Errorf("scheduled_for = %v, want nil (template must stay inert)", task.ScheduledFor)
		}
	})

	t.Run("ungated immediate create still mints pending", func(t *testing.T) {
		task := NewTask(TaskCreate{Prompt: "plain run-now"})
		if task.Status != TaskStatusPending {
			t.Errorf("status = %q, want pending (parking must apply only to gated tasks)", task.Status)
		}
		if task.ScheduledFor != nil {
			t.Errorf("scheduled_for = %v, want nil", task.ScheduledFor)
		}
	})
}
