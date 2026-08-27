package admincli

// Coupling test between the importers' status gate and the task lifecycle
// table (#1127) — the TaskWriterLegacyImport birth rows are otherwise the only
// table rows with no test tied to their writer. Since #1267 the SAME predicate
// gates the `sched task import` envelope path, so this test covers both.
// DB-free.

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestLegacyImportBirthStatusesMatchLifecycle pins importableTaskStatus to
// the lifecycle table's birth edges for the legacy-import writer, both
// directions (STRONG, set-derivation over the real predicate): if the gate
// later admits or drops a status, this fails until the table's birth rows
// move with it — and vice versa.
func TestLegacyImportBirthStatusesMatchLifecycle(t *testing.T) {
	want := importableStatusesFromLifecycle(t)
	for _, s := range models.AllTaskStatuses {
		if got := importableTaskStatus(s); got != want[s] {
			t.Errorf("importableTaskStatus(%q) = %v, but the lifecycle table lists a legacy-import birth edge for it = %v", s, got, want[s])
		}
	}
	// Retired/unknown statuses must stay rejected: a bundle carrying one is a
	// corrupt or pre-063 export, not a birth edge.
	for _, s := range models.RetiredTaskStatuses {
		if importableTaskStatus(s) {
			t.Errorf("importableTaskStatus accepts retired status %q", s)
		}
	}
}

// TestSchedTaskImportStatusesMatchLifecycle extends the same coupling to the
// envelope path (`sched task import`, #1267): validateImportedTask must accept
// exactly the statuses the lifecycle table lists as import birth edges, and
// reject every transient/retired one. It exercises the REAL validator (not the
// predicate directly), so a future edit that stops consulting the gate — the
// regression #1267 fixed — fails here.
func TestSchedTaskImportStatusesMatchLifecycle(t *testing.T) {
	want := importableStatusesFromLifecycle(t)
	for _, s := range models.AllTaskStatuses {
		task := &models.Task{ID: uuid.New(), Prompt: "do it", Status: s}
		err := validateImportedTask(task)
		if want[s] && err != nil {
			t.Errorf("validateImportedTask rejected importable status %q: %v", s, err)
		}
		if !want[s] && err == nil {
			t.Errorf("validateImportedTask accepted status %q, which the lifecycle table lists no import birth edge for", s)
		}
	}
	for _, s := range models.RetiredTaskStatuses {
		if err := validateImportedTask(&models.Task{ID: uuid.New(), Prompt: "x", Status: s}); err == nil {
			t.Errorf("validateImportedTask accepted retired status %q", s)
		}
	}
	// The birth sentinel is not a storable status: an envelope with no status
	// is corrupt, not a row to insert with an empty status column.
	if err := validateImportedTask(&models.Task{ID: uuid.New(), Prompt: "x"}); err == nil {
		t.Error("validateImportedTask accepted a task with no status")
	}
}

// importableStatusesFromLifecycle derives the expected gate set from the
// lifecycle table's TaskWriterLegacyImport birth rows — the table is the
// definition, so neither import path can drift from it silently.
func importableStatusesFromLifecycle(t *testing.T) map[models.TaskStatus]bool {
	t.Helper()
	want := map[models.TaskStatus]bool{}
	for _, tr := range models.TaskTransitionsByWriter(models.TaskWriterLegacyImport) {
		if tr.From != models.TaskLifecycleStart {
			t.Errorf("lifecycle table lists a %s edge from %q — the legacy importer only births rows, never transitions them", models.TaskWriterLegacyImport, tr.From)
			continue
		}
		want[tr.To] = true
	}
	return want
}
