// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package models

import "testing"

// Every dead letter reviewed in September 2026 had max_retries 0 because the
// create default was 0 and nobody set it per task, so the transient retry
// class never fired. NewTask now applies a deployment default when the request
// omits the field; an explicit value — including 0 — always wins (#1538).
func TestNewTaskAppliesDeploymentDefaultMaxRetries(t *testing.T) {
	prev := DefaultMaxRetries()
	t.Cleanup(func() { SetDefaultMaxRetries(prev) })

	SetDefaultMaxRetries(1)
	if got := NewTask(TaskCreate{Prompt: "p"}).MaxRetries; got != 1 {
		t.Fatalf("omitted max_retries must take the deployment default 1, got %d", got)
	}
	zero, three := 0, 3
	if got := NewTask(TaskCreate{Prompt: "p", MaxRetries: &zero}).MaxRetries; got != 0 {
		t.Fatalf("an explicit 0 must stay 0 (no retries), got %d", got)
	}
	if got := NewTask(TaskCreate{Prompt: "p", MaxRetries: &three}).MaxRetries; got != 3 {
		t.Fatalf("an explicit value must win over the default, got %d", got)
	}

	// The historical behaviour survives for a process that never sets it.
	SetDefaultMaxRetries(0)
	if got := NewTask(TaskCreate{Prompt: "p"}).MaxRetries; got != 0 {
		t.Fatalf("default 0 must mean no retries, got %d", got)
	}
	// Out-of-range values are clamped to the per-task bounds.
	SetDefaultMaxRetries(99)
	if got := DefaultMaxRetries(); got != 10 {
		t.Fatalf("default above 10 must clamp to 10, got %d", got)
	}
	SetDefaultMaxRetries(-4)
	if got := DefaultMaxRetries(); got != 0 {
		t.Fatalf("negative default must clamp to 0, got %d", got)
	}
}

// Export drops only the value equal to the deployment default, so "unset"
// round-trips as unset and an explicit 0 pinned under a non-zero default is
// preserved rather than silently becoming one retry on import.
func TestExportPreservesExplicitZeroRetriesUnderNonZeroDefault(t *testing.T) {
	prev := DefaultMaxRetries()
	t.Cleanup(func() { SetDefaultMaxRetries(prev) })
	SetDefaultMaxRetries(1)

	atDefault := NewTask(TaskCreate{Prompt: "p"})
	if rec := TaskToExportRecord(atDefault); rec.MaxRetries != nil {
		t.Fatalf("a task at the deployment default must export max_retries unset, got %d", *rec.MaxRetries)
	}
	zero := 0
	pinned := NewTask(TaskCreate{Prompt: "p", MaxRetries: &zero})
	rec := TaskToExportRecord(pinned)
	if rec.MaxRetries == nil || *rec.MaxRetries != 0 {
		t.Fatalf("an explicit 0 under default 1 must export as 0, got %v", rec.MaxRetries)
	}
	if got := NewTask(ExportRecordToTaskCreate(rec)).MaxRetries; got != 0 {
		t.Fatalf("re-importing the pinned task must keep 0 retries, got %d", got)
	}
}
