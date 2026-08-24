package models

// Tests for the task-lifecycle transition table (#1127). The table encodes
// the CURRENT lifecycle as data; these tests are the DB-free half of the
// coupling: structural validity, completeness (reachability + no dead ends),
// and agreement with the Go predicates and the create-path derivation. The
// DB-backed halves — per-writer transition matrices and the SQL-literal
// drift scan — live in sched/db/lifecycle_test.go and
// sched/storage/lifecycle_test.go, keyed on the same writer constants.

import (
	"testing"
	"time"
)

// TestTaskLifecycleValid re-runs the init-time structural gate as a test so a
// violation is a readable failure, not a boot panic: known statuses only, no
// duplicate edges, every status reachable from birth, no non-terminal dead
// ends, terminal exits limited to operator recovery, declared sets consistent
// with IsTerminal / IsValidReportedStatus.
func TestTaskLifecycleValid(t *testing.T) {
	if err := validateTaskLifecycle(); err != nil {
		t.Fatalf("lifecycle table invalid: %v", err)
	}
}

// TestTaskLifecycleDerivedSets pins the table-derived sets to their expected
// values — these are load-bearing (db.taskActiveStatuses feeds the
// serialization gate's SQL from ActiveTaskStatuses), so a table edit that
// changes them must be deliberate.
func TestTaskLifecycleDerivedSets(t *testing.T) {
	assertSet := func(name string, got, want []TaskStatus) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s = %v, want %v", name, got, want)
			}
		}
	}
	assertSet("ClaimableTaskStatuses", ClaimableTaskStatuses, []TaskStatus{TaskStatusPending})
	assertSet("ActiveTaskStatuses", ActiveTaskStatuses, []TaskStatus{TaskStatusLeased, TaskStatusRunning})
	assertSet("PausedTaskStatuses", PausedTaskStatuses,
		[]TaskStatus{TaskStatusPausedAwaitingInput, TaskStatusPausedAwaitingWake})
}

// TestDeriveDispatchStateMatchesBirthEdges sweeps DeriveDispatchState across
// every input combination class and asserts each produced status is a birth
// edge the table lists for TaskWriterDeriveDispatchState — and, conversely,
// that every listed birth status is actually producible. A new dispatch rule
// (or a new birth status) fails here until the table learns it.
func TestDeriveDispatchStateMatchesBirthEdges(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	gate := &RunIf{Command: "true"}

	cases := []struct {
		name         string
		trigger      TriggerType
		runIf        *RunIf
		scheduledFor *time.Time
	}{
		{"immediate", TriggerTypeCron, nil, nil},
		{"immediate past schedule", TriggerTypeCron, nil, &past},
		{"future schedule", TriggerTypeCron, nil, &future},
		{"gated immediate", TriggerTypeCron, gate, nil},
		{"gated future", TriggerTypeCron, gate, &future},
		{"webhook template", TriggerTypeWebhook, nil, nil},
		{"webhook gated", TriggerTypeWebhook, gate, nil},
		{"empty trigger defaults cron", "", nil, nil},
	}

	produced := map[TaskStatus]bool{}
	for _, tc := range cases {
		status, _ := DeriveDispatchState(tc.trigger, tc.runIf, tc.scheduledFor)
		produced[status] = true
		if !TaskTransitionExists(TaskLifecycleStart, status, TaskWriterDeriveDispatchState) {
			t.Errorf("%s: DeriveDispatchState produced %q but the table has no such birth edge", tc.name, status)
		}
	}
	for _, tr := range TaskTransitionsByWriter(TaskWriterDeriveDispatchState) {
		if !produced[tr.To] {
			t.Errorf("table lists birth edge →%q for DeriveDispatchState but no input combination produced it", tr.To)
		}
	}
}

// TestLifecycleRedCaseGuard documents (and exercises) the drift-catch: a
// status whose table rows are removed becomes unreachable, and the validator
// refuses the table. This is the loud failure #1127 asked for when a future
// status is added without lifecycle rows.
func TestLifecycleRedCaseGuard(t *testing.T) {
	// Rebuild the validator's input with every edge touching one real status
	// stripped, using a scratch copy — the package-level table is shared.
	orig := TaskLifecycle
	var stripped []TaskTransition
	for _, tr := range orig {
		if tr.From == TaskStatusPausedAwaitingWake || tr.To == TaskStatusPausedAwaitingWake {
			continue
		}
		stripped = append(stripped, tr)
	}
	TaskLifecycle = stripped
	defer func() { TaskLifecycle = orig }()

	if err := validateTaskLifecycle(); err == nil {
		t.Fatal("validator accepted a table with a status that has no edges — a new status added without lifecycle rows would go unnoticed")
	}
}
