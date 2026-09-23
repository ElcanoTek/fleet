package admincli

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Every CLI write path refuses a malformed EXECUTION REQUIREMENTS line (#1601)
// with the same message as the HTTP API: sched task import, task import,
// sched task batch, and the legacy bundle importer.
func TestCLIWritePathsRejectMalformedExecutionRequirements(t *testing.T) {
	bad := "Refresh the page.\n" + models.ExecutionRequirementsMarker + "\n" + `{"mcp_servers":["fast_io + fastio_helpers","pages"]}`
	const want = `invalid server or tool identifier "fast_io + fastio_helpers"`
	check := func(t *testing.T, path string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s accepted a malformed declaration (err=%v)", path, err)
		}
	}
	check(t, "sched task import", validateImportedTask(&models.Task{Prompt: bad, Status: models.TaskStatusScheduled}))
	check(t, "task import", validateExportRecordCLI(models.TaskExportRecord{Prompt: bad}))
	check(t, "sched task batch", validateBatchTaskCreate(&models.TaskCreate{Prompt: bad}))
	_, _, err := buildImportedTask(nil, bundleTask{ID: uuid.New(), Prompt: bad, Status: string(models.TaskStatusScheduled)}, nil, newImportStats())
	check(t, "legacy import", err)

	good := strings.Replace(bad, `"fast_io + fastio_helpers"`, `"fast_io","fastio_helpers"`, 1)
	if err := validateImportedTask(&models.Task{Prompt: good, Status: models.TaskStatusScheduled}); err != nil {
		t.Fatalf("a well-formed declaration was refused: %v", err)
	}
	if err := validateExportRecordCLI(models.TaskExportRecord{Prompt: good}); err != nil {
		t.Fatalf("a well-formed declaration was refused: %v", err)
	}
}

// Terminal history is preserved verbatim (docs/LEGACY-IMPORT.md): a legacy
// malformed declaration on a success/error/cancelled/dead_lettered row cannot
// dispatch, so it must not make a whole cross-box export unrestorable. Only a
// live (pending/scheduled) row is refused (#1601).
func TestImportsPreserveMalformedTerminalHistory(t *testing.T) {
	bad := "Refresh the page.\n" + models.ExecutionRequirementsMarker + "\n" + `{"mcp_servers":["fast_io + fastio_helpers","pages"]}`
	for _, status := range []models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusError, models.TaskStatusCancelled, models.TaskStatusDeadLettered} {
		if err := validateImportedTask(&models.Task{Prompt: bad, Status: status}); err != nil {
			t.Fatalf("sched task import refused %s history with a legacy malformed line: %v", status, err)
		}
		if _, _, err := buildImportedTask(nil, bundleTask{ID: uuid.New(), Prompt: bad, Status: string(status)}, nil, newImportStats()); err != nil && strings.Contains(err.Error(), "fast_io + fastio_helpers") {
			t.Fatalf("legacy import refused %s history with a legacy malformed line: %v", status, err)
		}
	}
	if err := validateImportedTask(&models.Task{Prompt: bad, Status: models.TaskStatusPending}); err == nil {
		t.Fatal("a live pending row with a malformed line must still be refused")
	}
}
