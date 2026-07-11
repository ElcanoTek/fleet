package db

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestTaskInsertColumnsCount pins taskInsertColumnsCount to the actual column
// list and argument builder. The three MUST agree or AddTaskBatch/AddTaskTx
// build a placeholder list that disagrees with the bound arguments and every
// batch/tx insert fails at execute time — exactly what happened when
// file_names (#710) grew the list to 61 while the constant stayed at 60. No DB
// required, so the drift is caught even where the Postgres-gated suites skip.
func TestTaskInsertColumnsCount(t *testing.T) {
	cols := strings.Split(taskInsertColumns, ",")
	if got := len(cols); got != taskInsertColumnsCount {
		t.Errorf("taskInsertColumns has %d columns but taskInsertColumnsCount = %d", got, taskInsertColumnsCount)
	}
	if got := len(taskInsertArgs(&models.Task{})); got != taskInsertColumnsCount {
		t.Errorf("taskInsertArgs returns %d values but taskInsertColumnsCount = %d", got, taskInsertColumnsCount)
	}
}
