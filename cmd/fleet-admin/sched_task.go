package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// taskExportVersion is the envelope schema version. Bump only on an incompatible
// shape change; import rejects an unknown version rather than guessing.
const taskExportVersion = 1

// taskExportEnvelope is the versioned JSON wrapper for `sched task export|import`.
// It carries full Task definitions so a round-trip preserves every field
// (including ID/CreatedAt/Status) — the basis for backup / version-control /
// cross-box migration.
type taskExportEnvelope struct {
	Version int            `json:"version"`
	Tasks   []*models.Task `json:"tasks"`
}

// cmdSchedTask dispatches `fleet-admin sched task export|import|set-model`.
func cmdSchedTask(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet-admin sched task export|import|set-model")
	}
	switch argv[0] {
	case "export":
		return schedTaskExport(argv[1:])
	case "import":
		return schedTaskImport(argv[1:])
	case "set-model":
		return schedTaskSetModel(argv[1:])
	default:
		return errf(1, "unknown sched task subcommand %q (want export|import|set-model)", argv[0])
	}
}

// schedTaskSetModel bulk re-assigns the pinned model (and optional fallback)
// across SCHEDULED tasks, optionally limited to those pinned to --from-model.
// --dry-run prints the matched tasks without writing. Fleet-wide, operator-only.
func schedTaskSetModel(argv []string) int {
	fs := flag.NewFlagSet("sched task set-model", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	model := fs.String("model", "", "the model slug to set (required)")
	fallback := fs.String("fallback-model", "", "optional fallback model slug ('' clears it to NULL)")
	fromModel := fs.String("from-model", "", "only re-assign tasks currently pinned to this slug")
	dryRun := fs.Bool("dry-run", false, "print the tasks that would change without writing")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if strings.TrimSpace(*model) == "" {
		return errf(1, "--model is required")
	}
	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	ctx := context.Background()

	if *dryRun {
		tasks, err := st.ListScheduledTasks(ctx)
		if err != nil {
			return errf(5, "list scheduled tasks: %v", err)
		}
		n := 0
		for _, t := range tasks {
			if *fromModel != "" && (t.Model == nil || *t.Model != *fromModel) {
				continue
			}
			cur := "<none>"
			if t.Model != nil {
				cur = *t.Model
			}
			fmt.Printf("%s\t%s → %s\n", t.ID, cur, *model)
			n++
		}
		fmt.Fprintf(os.Stderr, "dry-run: %d scheduled task(s) would be re-assigned\n", n)
		return 0
	}

	updated, err := st.BulkUpdateScheduledTaskModel(ctx, *model, *fallback, *fromModel)
	if err != nil {
		return errf(5, "re-assign model: %v", err)
	}
	fmt.Fprintf(os.Stderr, "re-assigned model on %d scheduled task(s)\n", updated)
	return 0
}

func schedTaskExport(argv []string) int {
	fs := flag.NewFlagSet("sched task export", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()

	n, err := exportTasks(context.Background(), st, os.Stdout)
	if err != nil {
		return errf(5, "export: %v", err)
	}
	fmt.Fprintf(os.Stderr, "exported %d scheduled task(s)\n", n)
	return 0
}

func schedTaskImport(argv []string) int {
	fs := flag.NewFlagSet("sched task import", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()

	n, err := importTasks(context.Background(), st, os.Stdin)
	if err != nil {
		return errf(5, "import: %v", err)
	}
	fmt.Fprintf(os.Stderr, "imported %d scheduled task(s)\n", n)
	return 0
}

// taskStore is the storage subset export/import needs — narrow so the seam is
// testable and the verbs stay thin. *storage.Storage satisfies it.
type taskStore interface {
	ListScheduledTasks(ctx context.Context) ([]*models.Task, error)
	AddTaskWithContext(ctx context.Context, task *models.Task) (*models.Task, error)
	GetUsersByIDsWithContext(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// exportTasks writes a versioned JSON envelope of all SCHEDULED tasks to w. Scope
// is status='scheduled' (ListScheduledTasks) — momentarily pending/running rows
// are excluded, matching the "scheduled tasks" wording. Returns the task count.
func exportTasks(ctx context.Context, st taskStore, w io.Writer) (int, error) {
	tasks, err := st.ListScheduledTasks(ctx)
	if err != nil {
		return 0, fmt.Errorf("list scheduled tasks: %w", err)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(taskExportEnvelope{Version: taskExportVersion, Tasks: tasks}); err != nil {
		return 0, err
	}
	return len(tasks), nil
}

// importTasks reads a versioned JSON envelope from r and recreates every task via
// AddTask (a full-column upsert on id, so re-import is idempotent / preserves IDs
// and all fields — NOT NewTask, which would mint new IDs and recompute Status).
//
// It is validate-all-THEN-insert: the entire batch is validated up-front, so a
// bad payload inserts nothing. CreatedBy is a FK to the sched users table; on a
// fresh target box the referencing user may be absent, which would fail the FK
// and abort the whole import — so a CreatedBy that names a user NOT present on the
// target is nulled (with a stderr notice) rather than failing the migration.
func importTasks(ctx context.Context, st taskStore, r io.Reader) (int, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return 0, fmt.Errorf("read import: %w", err)
	}
	var env taskExportEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, fmt.Errorf("parse import JSON: %w", err)
	}
	if env.Version != taskExportVersion {
		return 0, fmt.Errorf("unsupported export version %d (this build imports version %d)", env.Version, taskExportVersion)
	}

	// Validate the ENTIRE batch before inserting anything, so a bad payload leaves
	// the DB untouched.
	for i, t := range env.Tasks {
		if t == nil {
			return 0, fmt.Errorf("task[%d] is null", i)
		}
		if err := validateImportedTask(t); err != nil {
			return 0, fmt.Errorf("task[%d] (id=%s): %w", i, t.ID, err)
		}
	}

	// Null out CreatedBy for any user absent on the target box (cross-box migration
	// FK safety). Resolve all referenced users in one query.
	if err := nullMissingCreatedBy(ctx, st, env.Tasks); err != nil {
		return 0, err
	}

	for _, t := range env.Tasks {
		if _, err := st.AddTaskWithContext(ctx, t); err != nil {
			return 0, fmt.Errorf("add task %s: %w", t.ID, err)
		}
	}
	return len(env.Tasks), nil
}

// validateImportedTask enforces the load-bearing, PORTABLE create-time checks:
// a non-empty prompt, every MCP selection naming a server, and a parseable cron
// recurrence. It deliberately does NOT re-run the host/runtime-specific checks
// (file existence, scheduled-in-the-past) — import accepts historical definitions
// verbatim, and those are runtime concerns the scheduler re-evaluates at dispatch.
func validateImportedTask(t *models.Task) error {
	if strings.TrimSpace(t.Prompt) == "" {
		return fmt.Errorf("prompt is required")
	}
	for i, c := range t.MCPSelection {
		if strings.TrimSpace(c.Server) == "" {
			return fmt.Errorf("mcp_selection[%d] has no server", i)
		}
	}
	if strings.TrimSpace(t.Recurrence) != "" {
		if _, err := cron.ParseStandard(t.Recurrence); err != nil {
			return fmt.Errorf("invalid recurrence %q: %w", t.Recurrence, err)
		}
	}
	return nil
}

// nullMissingCreatedBy clears CreatedBy on any task whose referenced user is not
// present on the target box, so the FK insert can't abort a cross-box migration.
func nullMissingCreatedBy(ctx context.Context, st taskStore, tasks []*models.Task) error {
	var ids []uuid.UUID
	for _, t := range tasks {
		if t.CreatedBy != nil {
			ids = append(ids, *t.CreatedBy)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	present, err := st.GetUsersByIDsWithContext(ctx, ids)
	if err != nil {
		return fmt.Errorf("resolve created_by users: %w", err)
	}
	for _, t := range tasks {
		if t.CreatedBy != nil {
			if _, ok := present[*t.CreatedBy]; !ok {
				fmt.Fprintf(os.Stderr, "note: task %s created_by user %s not present on this box; importing with created_by unset\n", t.ID, *t.CreatedBy)
				t.CreatedBy = nil
			}
		}
	}
	return nil
}

var _ taskStore = (*storage.Storage)(nil)
