package admincli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/robfig/cron/v3"
)

// batchTaskStore is the storage subset batch-create needs — narrow so the seam
// is testable without a live DB. *storage.Storage satisfies it.
type batchTaskStore interface {
	AddTaskBatch(ctx context.Context, tasks []*models.Task, atomic bool) (int, error)
}

var _ batchTaskStore = (*storage.Storage)(nil)

// batchCreateTasks reads an array of TaskCreate recipes from r, validates them
// the same lightweight way sched task import does (non-empty prompt, parseable
// cron, every MCP selection naming a server), mints a Task per recipe via
// models.NewTask, and persists the batch through storage.AddTaskBatch.
//
// In atomic mode a single validation failure or DB error aborts the whole
// batch (returns the error; nothing is committed). In non-atomic mode valid
// tasks are inserted while invalid ones are skipped; the returned result lists
// per-task successes and failures so the CLI can print a summary.
//
// This mirrors sched task import's DB-direct seam (the fleet-admin CLI talks to
// the sched DB directly, not over HTTP), so the batch-create path exercises the
// same storage.AddTaskBatch the POST /tasks/batch handler uses.
func batchCreateTasks(ctx context.Context, st batchTaskStore, r io.Reader, atomic bool) (models.BatchTaskResult, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return models.BatchTaskResult{}, fmt.Errorf("read input: %w", err)
	}
	var tasks []models.TaskCreate
	if err := json.Unmarshal(raw, &tasks); err != nil {
		return models.BatchTaskResult{}, fmt.Errorf("parse input JSON: %w", err)
	}
	if len(tasks) == 0 {
		return models.BatchTaskResult{}, errors.New("tasks array is empty")
	}
	if len(tasks) > MaxBatchSize {
		return models.BatchTaskResult{}, fmt.Errorf("batch size %d exceeds limit of %d", len(tasks), MaxBatchSize)
	}

	var (
		toInsert         []*models.Task
		createdList      []models.BatchCreated
		failedList       []models.BatchFailed
		validationFailed bool
	)
	for i := range tasks {
		tc := &tasks[i]
		if err := validateBatchTaskCreate(tc); err != nil {
			failedList = append(failedList, models.BatchFailed{Index: i, Error: err.Error()})
			validationFailed = true
		} else {
			t := models.NewTask(*tc)
			toInsert = append(toInsert, t)
			createdList = append(createdList, models.BatchCreated{ID: t.ID, Index: i})
		}
	}

	if atomic && validationFailed {
		var atomicFailedList []models.BatchFailed
		valErrors := make(map[int]string)
		for _, f := range failedList {
			valErrors[f.Index] = f.Error
		}
		for i := range tasks {
			if errMsg, ok := valErrors[i]; ok {
				atomicFailedList = append(atomicFailedList, models.BatchFailed{Index: i, Error: errMsg})
			} else {
				atomicFailedList = append(atomicFailedList, models.BatchFailed{
					Index: i, Error: "batch aborted: another task failed validation",
				})
			}
		}
		return models.BatchTaskResult{Created: []models.BatchCreated{}, Failed: atomicFailedList, Count: 0}, nil
	}

	if len(toInsert) == 0 {
		return models.BatchTaskResult{Created: []models.BatchCreated{}, Failed: failedList, Count: 0}, nil
	}

	if _, err := st.AddTaskBatch(ctx, toInsert, atomic); err != nil {
		if atomic {
			for _, c := range createdList {
				failedList = append(failedList, models.BatchFailed{
					Index: c.Index, Error: "atomic batch rolled back: " + err.Error(),
				})
			}
			return models.BatchTaskResult{Created: []models.BatchCreated{}, Failed: failedList, Count: 0}, nil
		}
		for _, c := range createdList {
			failedList = append(failedList, models.BatchFailed{
				Index: c.Index, Error: "failed to create task: " + err.Error(),
			})
		}
		return models.BatchTaskResult{Created: []models.BatchCreated{}, Failed: failedList, Count: 0}, nil
	}

	return models.BatchTaskResult{Created: createdList, Failed: failedList, Count: len(createdList)}, nil
}

// validateBatchTaskCreate runs the portable create-time checks the CLI can apply
// without a live fleet process: a non-empty prompt, every MCP selection naming a
// server, and a parseable cron recurrence. It deliberately does NOT re-run the
// host/runtime-specific checks (file existence, scheduled-in-the-past, model
// catalog) the HTTP handler enforces — those are runtime concerns the scheduler
// re-evaluates at dispatch, and a CLI operator may legitimately import
// historical/template definitions.
func validateBatchTaskCreate(tc *models.TaskCreate) error {
	tc.Prompt = strings.TrimSpace(tc.Prompt)
	if tc.Prompt == "" {
		return errors.New("prompt is required")
	}
	for i, c := range tc.MCPSelection {
		if strings.TrimSpace(c.Server) == "" {
			return fmt.Errorf("mcp_selection[%d] has no server", i)
		}
	}
	if tc.Recurrence = strings.TrimSpace(tc.Recurrence); tc.Recurrence != "" {
		if _, err := cron.ParseStandard(tc.Recurrence); err != nil {
			return fmt.Errorf("invalid recurrence %q: %w", tc.Recurrence, err)
		}
	}
	return nil
}

// MaxBatchSize re-exports the handler cap so the CLI enforces the same ceiling
// without importing the handlers package (which would pull in HTTP deps).
const MaxBatchSize = 100

// schedTaskBatchCreate is the `fleet-admin sched task batch-create` subcommand.
// It reads a JSON array of TaskCreate objects from --from-file (or stdin when
// the value is "-" or the flag is omitted), optionally runs in atomic mode, and
// prints a summary table of created (with IDs) and failed (with indices and
// errors) tasks.
func schedTaskBatchCreate(argv []string) int {
	fs := flag.NewFlagSet("sched task batch-create", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	fromFile := fs.String("from-file", "", "path to a JSON file of TaskCreate objects (\"-\" or empty = stdin)")
	atomic := fs.Bool("atomic", false, "create all tasks in a single transaction (any failure rolls back all)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}

	in := os.Stdin
	if v := strings.TrimSpace(*fromFile); v != "" && v != "-" {
		f, err := os.Open(v) //nolint:gosec // G304: operator-supplied --from-file path for an admin CLI, not request/LLM input.
		if err != nil {
			return errf(1, "open %s: %v", v, err)
		}
		defer f.Close()
		in = f
	}

	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()

	result, err := batchCreateTasks(context.Background(), st, in, *atomic)
	if err != nil {
		return errf(5, "batch-create: %v", err)
	}

	printBatchResult(result, *atomic)
	if len(result.Failed) > 0 && len(result.Created) == 0 {
		return 6
	}
	return 0
}

// printBatchResult renders the batch outcome as a human-readable summary on
// stdout, mirroring the issue's example output.
func printBatchResult(r models.BatchTaskResult, atomic bool) {
	fmt.Printf("Submitted %d task(s) (%s mode)\n", len(r.Created)+len(r.Failed), modeLabel(atomic))
	fmt.Printf("Created %d task(s):\n", r.Count)
	for _, c := range r.Created {
		fmt.Printf("  [%d] %s\n", c.Index, c.ID)
	}
	if len(r.Failed) > 0 {
		fmt.Printf("Failed %d task(s):\n", len(r.Failed))
		for _, f := range r.Failed {
			fmt.Printf("  [%d] %s\n", f.Index, f.Error)
		}
	}
}

func modeLabel(atomic bool) string {
	if atomic {
		return "atomic"
	}
	return "best-effort"
}
