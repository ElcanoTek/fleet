package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// cmdTask dispatches `fleet-admin task export|import` (#238). These are the
// definition-only export/import verbs: a portable JSON or YAML envelope of task
// configuration (NOT runtime state) for backup, cross-box migration, and
// cloning. They are distinct from `fleet-admin sched task export|import`, which
// is a full-Task backup that preserves IDs and runtime state.
func cmdTask(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet-admin task export|import|memories")
	}
	switch argv[0] {
	case "export":
		return taskExport(argv[1:])
	case "import":
		return taskImport(argv[1:])
	case "memories", "memory":
		return taskMemories(argv[1:])
	default:
		return errf(1, "unknown task subcommand %q (want export|import|memories)", argv[0])
	}
}

// definitionStore is the storage subset the definition export/import verbs need
// — narrow so the seam is testable and the verbs stay thin. *storage.Storage
// satisfies it.
type definitionStore interface {
	ListTasksForExport(ctx context.Context, ids []uuid.UUID, recurrenceOnly bool) ([]*models.Task, error)
	FindTaskIDsByName(ctx context.Context, names []string) (map[string]uuid.UUID, error)
	GetTaskByName(ctx context.Context, name string) (*models.Task, error)
	AddTaskWithContext(ctx context.Context, task *models.Task) (*models.Task, error)
	UpdateTask(task *models.Task) (*models.Task, error)
}

// taskExport implements `fleet-admin task export` (#238). It writes a versioned
// JSON (default) or YAML envelope of task definitions to stdout.
//
//	fleet-admin task export --format yaml > tasks.yaml
//	fleet-admin task export --ids uuid1,uuid2 --format json > subset.json
//	fleet-admin task export --recurrence-only > cron-tasks.json
func taskExport(argv []string) int {
	fs := flag.NewFlagSet("task export", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	format := fs.String("format", "json", "output format: json|yaml")
	idsStr := fs.String("ids", "", "comma-separated task UUIDs to export (omit for all)")
	recurrenceOnly := fs.Bool("recurrence-only", false, "export only tasks with a non-empty recurrence (cron tasks)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	*format = strings.ToLower(strings.TrimSpace(*format))
	if *format != "json" && *format != "yaml" {
		return errf(1, "--format must be json or yaml (got %q)", *format)
	}
	var ids []uuid.UUID
	if raw := strings.TrimSpace(*idsStr); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			id, err := uuid.Parse(s)
			if err != nil {
				return errf(1, "invalid id %q: %v", s, err)
			}
			ids = append(ids, id)
		}
	}

	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()

	n, err := exportTaskDefinitions(context.Background(), st, os.Stdout, *format, ids, *recurrenceOnly)
	if err != nil {
		return errf(5, "export: %v", err)
	}
	fmt.Fprintf(os.Stderr, "exported %d task definition(s) as %s\n", n, *format)
	return 0
}

// taskImport implements `fleet-admin task import` (#238). It reads a JSON or
// YAML envelope from --from <file> (or stdin when --from is "-" or omitted) and
// creates/replaces tasks with per-record conflict handling.
//
//	fleet-admin task import --from tasks.yaml --dry-run
//	fleet-admin task import --from tasks.yaml --conflict replace
func taskImport(argv []string) int {
	fs := flag.NewFlagSet("task import", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	from := fs.String("from", "-", "input file (- or empty = stdin)")
	format := fs.String("format", "", "input format: json|yaml (empty = sniff from content)")
	dryRun := fs.Bool("dry-run", false, "validate and print the plan without writing")
	conflict := fs.String("conflict", "error", "name-collision handling: error|skip|replace")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	*conflict = strings.ToLower(strings.TrimSpace(*conflict))
	switch models.TaskImportConflict(*conflict) {
	case models.TaskImportConflictError, models.TaskImportConflictSkip, models.TaskImportConflictReplace:
	default:
		return errf(1, "--conflict must be error, skip, or replace (got %q)", *conflict)
	}

	var r io.Reader = os.Stdin
	if f := strings.TrimSpace(*from); f != "" && f != "-" {
		fh, err := os.Open(f) //nolint:gosec // G304: f is an operator-supplied path from --from, not untrusted input.
		if err != nil {
			return errf(1, "open %s: %v", f, err)
		}
		defer fh.Close()
		r = fh
	}

	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()

	resp, err := importTaskDefinitions(context.Background(), st, r, *format, *dryRun, models.TaskImportConflict(*conflict))
	if err != nil {
		return errf(5, "import: %v", err)
	}
	if err := printImportResponse(os.Stdout, resp); err != nil {
		return errf(5, "print result: %v", err)
	}
	prefix := ""
	if *dryRun {
		prefix = "dry-run: "
	}
	fmt.Fprintf(os.Stderr, "%s%d created, %d skipped, %d replaced, %d error(s) (total %d)\n",
		prefix, resp.Created, resp.Skipped, resp.Replaced, resp.Errors, resp.Total)
	if resp.Errors > 0 {
		return 6
	}
	return 0
}

// exportTaskDefinitions fetches task definitions from the store and writes a
// versioned envelope to w in the requested format. Returns the record count.
func exportTaskDefinitions(ctx context.Context, st definitionStore, w io.Writer, format string, ids []uuid.UUID, recurrenceOnly bool) (int, error) {
	tasks, err := st.ListTasksForExport(ctx, ids, recurrenceOnly)
	if err != nil {
		return 0, fmt.Errorf("list tasks: %w", err)
	}
	records := make([]models.TaskExportRecord, 0, len(tasks))
	for _, t := range tasks {
		records = append(records, models.TaskToExportRecord(t))
	}
	envelope := models.TaskExportEnvelope{
		Version:    models.TaskExportVersion,
		ExportedAt: time.Now().UTC(),
		Tasks:      records,
	}
	switch format {
	case "yaml":
		if err := yaml.NewEncoder(w, yaml.Indent(2)).Encode(envelope); err != nil {
			return 0, fmt.Errorf("yaml encode: %w", err)
		}
	default:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(envelope); err != nil {
			return 0, fmt.Errorf("json encode: %w", err)
		}
	}
	return len(records), nil
}

// importTaskDefinitions reads an envelope from r and creates/replaces tasks per
// the conflict policy. It mirrors the HTTP handler's logic so the CLI path and
// the API path enforce the same up-front validation and conflict semantics.
func importTaskDefinitions(ctx context.Context, st definitionStore, r io.Reader, format string, dryRun bool, conflict models.TaskImportConflict) (*models.TaskImportResponse, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}
	var envelope models.TaskExportEnvelope
	switch format {
	case "json":
		if err := json.Unmarshal(body, &envelope); err != nil {
			return nil, fmt.Errorf("parse json: %w", err)
		}
	case "yaml":
		if err := yaml.Unmarshal(body, &envelope); err != nil {
			return nil, fmt.Errorf("parse yaml: %w", err)
		}
	default:
		// Sniff: try JSON first, fall back to YAML.
		if err := json.Unmarshal(body, &envelope); err != nil {
			if yerr := yaml.Unmarshal(body, &envelope); yerr != nil {
				return nil, fmt.Errorf("parse input (tried json then yaml): json err and yaml err occurred")
			}
		}
	}
	if envelope.Version != models.TaskExportVersion {
		return nil, fmt.Errorf("unsupported export version %q (this build imports version %s)", envelope.Version, models.TaskExportVersion)
	}
	if len(envelope.Tasks) > 100 {
		return nil, fmt.Errorf("import accepts at most 100 records (got %d)", len(envelope.Tasks))
	}
	for i, rec := range envelope.Tasks {
		if err := validateExportRecordCLI(rec); err != nil {
			return nil, fmt.Errorf("task[%d] (name=%s): %w", i, rec.Name, err)
		}
	}
	// Intra-batch duplicate names are an error.
	seen := make(map[string]int)
	for i, rec := range envelope.Tasks {
		name := strings.TrimSpace(rec.Name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("duplicate name %q in import payload (record %d)", name, i)
		}
		seen[name] = i
	}

	names := make([]string, 0, len(envelope.Tasks))
	for _, rec := range envelope.Tasks {
		if n := strings.TrimSpace(rec.Name); n != "" {
			names = append(names, n)
		}
	}
	existing, err := st.FindTaskIDsByName(ctx, names)
	if err != nil {
		return nil, fmt.Errorf("resolve conflicts: %w", err)
	}
	if conflict == models.TaskImportConflictError && len(existing) > 0 {
		colliding := make([]string, 0, len(existing))
		for n := range existing {
			colliding = append(colliding, n)
		}
		return nil, fmt.Errorf("%d task(s) already exist: %s", len(existing), strings.Join(colliding, ", "))
	}

	resp := &models.TaskImportResponse{DryRun: dryRun, Total: len(envelope.Tasks), Results: []models.TaskImportResult{}}
	for _, rec := range envelope.Tasks {
		result := models.TaskImportResult{Name: rec.Name}
		name := strings.TrimSpace(rec.Name)
		_, collision := existing[name]
		switch {
		case collision && conflict == models.TaskImportConflictSkip:
			result.Status = models.TaskImportSkipped
			result.Reason = "conflict=skip"
			resp.Skipped++
		case collision && conflict == models.TaskImportConflictReplace:
			if !dryRun {
				id, rerr := replaceTaskDefinitionCLI(ctx, st, rec)
				if rerr != nil {
					result.Status = models.TaskImportErrored
					result.Error = rerr.Error()
					resp.Errors++
					resp.Results = append(resp.Results, result)
					continue
				}
				result.ID = &id
			}
			result.Status = models.TaskImportReplaced
			resp.Replaced++
		default:
			if !dryRun {
				task := models.NewTask(models.ExportRecordToTaskCreate(rec))
				if _, cerr := st.AddTaskWithContext(ctx, task); cerr != nil {
					result.Status = models.TaskImportErrored
					result.Error = cerr.Error()
					resp.Errors++
					resp.Results = append(resp.Results, result)
					continue
				}
				id := task.ID
				result.ID = &id
			}
			result.Status = models.TaskImportCreated
			resp.Created++
		}
		resp.Results = append(resp.Results, result)
	}
	return resp, nil
}

// replaceTaskDefinitionCLI overlays rec's definition onto the existing task
// (matched by name) and re-saves it, preserving runtime state.
func replaceTaskDefinitionCLI(ctx context.Context, st definitionStore, rec models.TaskExportRecord) (uuid.UUID, error) {
	existing, err := st.GetTaskByName(ctx, rec.Name)
	if err != nil {
		return uuid.Nil, err
	}
	if existing == nil {
		// Raced: deleted between pre-flight and now. Fall back to a fresh create.
		task := models.NewTask(models.ExportRecordToTaskCreate(rec))
		if _, cerr := st.AddTaskWithContext(ctx, task); cerr != nil {
			return uuid.Nil, cerr
		}
		return task.ID, nil
	}
	tc := models.ExportRecordToTaskCreate(rec)
	existing.Name = tc.Name
	existing.Prompt = tc.Prompt
	existing.Model = tc.Model
	existing.FallbackModel = tc.FallbackModel
	existing.MaxIterations = tc.MaxIterations
	existing.MCPSelection = tc.MCPSelection
	existing.CredentialAllowlist = tc.CredentialAllowlist
	existing.LoopConfig = tc.LoopConfig
	existing.WorktreeConfig = tc.WorktreeConfig
	existing.Priority = tc.Priority
	existing.InstructionSelfImprove = tc.InstructionSelfImprove
	existing.AllowNetwork = tc.AllowNetwork
	existing.AllowDelegation = tc.AllowDelegation
	existing.Persona = tc.Persona
	existing.Description = tc.Description
	existing.ScheduledFor = tc.ScheduledFor
	existing.Recurrence = tc.Recurrence
	existing.Timezone = tc.Timezone
	existing.Files = tc.Files
	existing.Tags = tc.Tags
	if tc.MaxRetries != nil {
		existing.MaxRetries = *tc.MaxRetries
	} else {
		existing.MaxRetries = 0
	}
	existing.RetryPolicy = tc.RetryPolicy
	existing.TriggerType = tc.TriggerType
	existing.AllowTaskCreation = tc.AllowTaskCreation
	existing.AllowRecurringTaskCreation = tc.AllowRecurringTaskCreation
	if _, err := st.UpdateTask(existing); err != nil {
		return uuid.Nil, err
	}
	return existing.ID, nil
}

// validateExportRecordCLI is the CLI mirror of the handler's
// validateExportRecord. Kept local so the CLI doesn't depend on the handlers
// package (which pulls in net/http + the full mux); the structural checks are
// identical and small.
func validateExportRecordCLI(rec models.TaskExportRecord) error {
	if strings.TrimSpace(rec.Prompt) == "" {
		return errors.New("prompt is required")
	}
	for i, c := range rec.MCPSelection {
		if strings.TrimSpace(c.Server) == "" {
			return fmt.Errorf("mcp_selection[%d] has no server", i)
		}
	}
	for i, e := range rec.CredentialAllowlist {
		if strings.TrimSpace(e.Server) == "" {
			return fmt.Errorf("credential_allowlist[%d] has no server", i)
		}
	}
	if rec.Name != "" && len(rec.Name) > 255 {
		return fmt.Errorf("name exceeds 255 characters")
	}
	return nil
}

// printImportResponse writes the import result envelope as JSON to w.
func printImportResponse(w io.Writer, resp *models.TaskImportResponse) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(resp)
}

var _ definitionStore = (*storage.Storage)(nil)
