// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package models

import (
	"encoding/json"
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

// TestDeriveDispatchState pins the shared dispatch-state derivation on the
// edit-shaped inputs NewTask never sees: the task-edit recompute
// (storage.UpdateEditableTask) feeds it a task's post-edit fields, where a
// nil scheduled_for is the natural client echo for a parked gated task and a
// past one can arrive from storage. Before the recompute shared this rule, an
// edit flipped both shapes to pending — a gate bypass and a template turned
// one-shot.
func TestDeriveDispatchState(t *testing.T) {
	gate := &RunIf{Command: "true", TimeoutSeconds: 30}
	past := time.Now().UTC().Add(-time.Minute)
	future := time.Now().UTC().Add(time.Hour)

	t.Run("gated cron with nil scheduled_for parks scheduled-for-now", func(t *testing.T) {
		before := time.Now().UTC()
		status, when := DeriveDispatchState(TriggerTypeCron, gate, nil)
		if status != TaskStatusScheduled {
			t.Errorf("status = %q, want scheduled", status)
		}
		if when == nil || when.Before(before) || when.After(time.Now().UTC()) {
			t.Errorf("scheduled_for = %v, want defaulted to now", when)
		}
	})

	t.Run("gated cron with past scheduled_for stays scheduled and due", func(t *testing.T) {
		status, when := DeriveDispatchState(TriggerTypeCron, gate, &past)
		if status != TaskStatusScheduled {
			t.Errorf("status = %q, want scheduled", status)
		}
		if when == nil || !when.Equal(past) {
			t.Errorf("scheduled_for = %v, want the given %v kept", when, past)
		}
	})

	t.Run("empty trigger type is treated as cron", func(t *testing.T) {
		status, when := DeriveDispatchState("", gate, nil)
		if status != TaskStatusScheduled || when == nil {
			t.Errorf("status=%q scheduled_for=%v, want a parked gated task", status, when)
		}
	})

	t.Run("webhook template keeps nil scheduled_for and stays scheduled", func(t *testing.T) {
		status, when := DeriveDispatchState(TriggerTypeWebhook, gate, nil)
		if status != TaskStatusScheduled {
			t.Errorf("status = %q, want scheduled (inert)", status)
		}
		if when != nil {
			t.Errorf("scheduled_for = %v, want nil (defaulting it would surface the template as due)", when)
		}
	})

	t.Run("ungated cron follows the plain rule", func(t *testing.T) {
		if status, _ := DeriveDispatchState(TriggerTypeCron, nil, nil); status != TaskStatusPending {
			t.Errorf("nil scheduled_for: status = %q, want pending", status)
		}
		if status, _ := DeriveDispatchState(TriggerTypeCron, nil, &past); status != TaskStatusPending {
			t.Errorf("past scheduled_for: status = %q, want pending", status)
		}
		if status, _ := DeriveDispatchState(TriggerTypeCron, nil, &future); status != TaskStatusScheduled {
			t.Errorf("future scheduled_for: status = %q, want scheduled", status)
		}
	})
}

// TestExportImport_RunIfRoundTrip proves the pre-run gate (#269) is part of the
// portable task definition: before the fix TaskExportRecord had no run_if
// field, so a box migration or backup-restore silently converted every gated
// task into an unconditional one. The recreated task must also land on the
// scheduler path (RunIf's enforcement contract), never pending.
func TestExportImport_RunIfRoundTrip(t *testing.T) {
	src := &Task{
		Prompt: "gated work",
		RunIf:  &RunIf{Command: "test -f /tmp/ready", ExitCodeIs: 2, TimeoutSeconds: 60, OnError: RunIfOnErrorSkip},
	}
	rec, imported := roundTripDefinition(t, src, json.Marshal, json.Unmarshal)
	if rec.RunIf == nil {
		t.Fatal("export record dropped run_if — the gate would silently vanish on migration")
	}
	if imported.RunIf == nil ||
		imported.RunIf.Command != src.RunIf.Command ||
		imported.RunIf.ExitCodeIs != src.RunIf.ExitCodeIs ||
		imported.RunIf.TimeoutSeconds != src.RunIf.TimeoutSeconds ||
		imported.RunIf.OnError != src.RunIf.OnError {
		t.Errorf("run_if did not round-trip: %+v", imported.RunIf)
	}
	if imported.Status != TaskStatusScheduled || imported.ScheduledFor == nil {
		t.Errorf("imported gated task must be parked on the scheduler path, got status=%s scheduled_for=%v",
			imported.Status, imported.ScheduledFor)
	}
}
