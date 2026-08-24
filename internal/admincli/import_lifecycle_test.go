package admincli

// Coupling test between the legacy importer's status gate and the task
// lifecycle table (#1127) — the TaskWriterLegacyImport birth rows are
// otherwise the only table rows with no test tied to their writer. DB-free.

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestLegacyImportBirthStatusesMatchLifecycle pins validSchedTaskStatus to
// the lifecycle table's birth edges for the legacy-import writer, both
// directions (STRONG, set-derivation over the real predicate): if the gate
// later admits or drops a status, this fails until the table's birth rows
// move with it — and vice versa.
func TestLegacyImportBirthStatusesMatchLifecycle(t *testing.T) {
	want := map[models.TaskStatus]bool{}
	for _, tr := range models.TaskTransitionsByWriter(models.TaskWriterLegacyImport) {
		if tr.From != models.TaskLifecycleStart {
			t.Errorf("lifecycle table lists a %s edge from %q — the legacy importer only births rows, never transitions them", models.TaskWriterLegacyImport, tr.From)
			continue
		}
		want[tr.To] = true
	}
	for _, s := range models.AllTaskStatuses {
		if got := validSchedTaskStatus(s); got != want[s] {
			t.Errorf("validSchedTaskStatus(%q) = %v, but the lifecycle table lists a legacy-import birth edge for it = %v", s, got, want[s])
		}
	}
	// Retired/unknown statuses must stay rejected: a bundle carrying one is a
	// corrupt or pre-063 export, not a birth edge.
	for _, s := range models.RetiredTaskStatuses {
		if validSchedTaskStatus(s) {
			t.Errorf("validSchedTaskStatus accepts retired status %q", s)
		}
	}
}
