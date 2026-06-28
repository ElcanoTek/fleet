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

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/ElcanoTek/fleet/internal/agentcore"
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

// cmdSchedTask dispatches `fleet-admin sched task export|import|set-model|set-credentials|set-description`.
func cmdSchedTask(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet-admin sched task export|import|set-model|set-credentials|set-description|tag|estimate")
	}
	switch argv[0] {
	case "export":
		return schedTaskExport(argv[1:])
	case "import":
		return schedTaskImport(argv[1:])
	case "set-model":
		return schedTaskSetModel(argv[1:])
	case "set-credentials":
		return schedTaskSetCredentials(argv[1:])
	case "set-description":
		return schedTaskSetDescription(argv[1:])
	case "tag":
		return schedTaskTag(argv[1:])
	case "estimate":
		return schedTaskEstimate(argv[1:])
	default:
		return errf(1, "unknown sched task subcommand %q (want export|import|set-model|set-credentials|set-description|tag|estimate)", argv[0])
	}
}

// schedTaskEstimate prints a pre-submission cost forecast for a would-be task
// without creating it (#233):
//
//	fleet-admin sched task estimate --model anthropic/claude-sonnet-4-5 \
//	    --max-iter 20 --prompt "Summarize all issues opened in the last 7 days"
//
// It is pure local computation over the same agentcore forecast the
// POST /tasks/estimate endpoint uses — no server call and no model call — so it
// needs neither a database nor network access. It exits non-zero on a usage
// error and 0 once it prints the forecast. Because it runs outside the server it
// cannot load the client-config bundle's system prompt; pass --system-prompt
// (or --mcp-tools for the tool-schema budget) to make the estimate reflect that
// input. The endpoint, which has the bundle, includes the system prompt
// automatically.
func schedTaskEstimate(argv []string) int {
	fs := flag.NewFlagSet("sched task estimate", flag.ContinueOnError)
	model := fs.String("model", "", "OpenRouter model slug (required)")
	prompt := fs.String("prompt", "", "task prompt (required)")
	maxIter := fs.Int("max-iter", 1, "loop iteration cap")
	maxCost := fs.Float64("max-cost", 0, "per-task cost ceiling in USD (0 disables the would-hit-ceiling check)")
	mcpTools := fs.Int("mcp-tools", 0, "number of MCP tool definitions in scope")
	systemPrompt := fs.String("system-prompt", "", "system prompt text to include in the token estimate")
	asJSON := fs.Bool("json", false, "emit the forecast as JSON")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if strings.TrimSpace(*model) == "" {
		return errf(1, "--model is required")
	}
	if strings.TrimSpace(*prompt) == "" {
		return errf(1, "--prompt is required")
	}
	if *mcpTools < 0 {
		return errf(1, "--mcp-tools cannot be negative")
	}

	systemToks, toolToks, promptToks := agentcore.EstimateTokens(*systemPrompt, *prompt, *mcpTools)
	fc := agentcore.ForecastCost(*model, systemToks, toolToks, promptToks, *maxIter, *maxCost)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(fc); err != nil {
			return errf(1, "encode forecast: %v", err)
		}
		return 0
	}

	printForecast(fc)
	return 0
}

// printForecast renders a CostForecast as a human-readable block. Cost lines are
// shown only when the model's pricing is known; otherwise the unknown-pricing
// note is printed so the operator isn't handed a fabricated number.
func printForecast(fc agentcore.CostForecast) {
	fmt.Printf("Model:                %s\n", fc.Model)
	fmt.Printf("Estimated prompt:     %d tokens (system %d + tools %d + task)\n",
		fc.EstimatedPromptTokens, fc.SystemPromptTokens, fc.ToolDefinitionsTokens)
	fmt.Printf("Avg output / iter:    %d tokens\n", fc.AvgOutputTokens)
	fmt.Printf("Max iterations:       %d\n", fc.MaxIterations)
	if fc.PricingKnown && fc.PerIterationCostUSD != nil && fc.EstimatedTotalCostUSD != nil && fc.EstimatedTotalRange != nil {
		fmt.Printf("Per-iteration cost:   $%.4f\n", *fc.PerIterationCostUSD)
		fmt.Printf("Estimated total:      $%.4f (range $%.4f - $%.4f)\n",
			*fc.EstimatedTotalCostUSD, fc.EstimatedTotalRange.MinUSD, fc.EstimatedTotalRange.MaxUSD)
		if fc.MaxCostCeilingUSD > 0 {
			fmt.Printf("Cost ceiling:         $%.2f\n", fc.MaxCostCeilingUSD)
			if fc.WouldHitCeiling {
				fmt.Printf("WARNING:              estimated total exceeds the cost ceiling\n")
			}
		}
	}
	fmt.Printf("Note:                 %s\n", fc.Note)
}

// schedTaskTag adds and/or removes tags on a task (#212):
//
//	fleet-admin sched task tag <task_id> --add nightly --add prod --remove staging
func schedTaskTag(argv []string) int {
	fs := flag.NewFlagSet("sched task tag", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	var add, remove allowFlag
	fs.Var(&add, "add", "tag to add (repeatable)")
	fs.Var(&remove, "remove", "tag to remove (repeatable)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errf(1, "usage: fleet-admin sched task tag <task_id> --add <tag> ... --remove <tag> ...")
	}
	taskID, err := uuid.Parse(strings.TrimSpace(rest[0]))
	if err != nil {
		return errf(1, "invalid task id %q: %v", rest[0], err)
	}
	if len(add) == 0 && len(remove) == 0 {
		return errf(1, "provide at least one --add or --remove tag")
	}
	addTags, err := models.NormalizeAndValidateTags(add)
	if err != nil {
		return errf(1, "--add: %v", err)
	}
	removeTags, err := models.NormalizeAndValidateTags(remove)
	if err != nil {
		return errf(1, "--remove: %v", err)
	}

	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()

	updated, err := st.UpdateTaskTags(context.Background(), taskID, addTags, removeTags)
	if err != nil {
		return errf(5, "update tags: %v", err)
	}
	fmt.Fprintf(os.Stderr, "task %s now has %d tag(s): %s\n", updated.ID, len(updated.Tags), strings.Join(updated.Tags, ", "))
	return 0
}

// schedTaskSetDescription sets a task's operator-documentation field (#281).
// The text is the second positional arg, or "-" to read the full text from stdin
// (so `set-description <id> - < TASK_README.md` works). An empty string clears it.
func schedTaskSetDescription(argv []string) int {
	fs := flag.NewFlagSet("sched task set-description", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return errf(1, "usage: fleet-admin sched task set-description <task_id> <text>|-   (- reads from stdin)")
	}
	taskID, err := uuid.Parse(strings.TrimSpace(rest[0]))
	if err != nil {
		return errf(1, "invalid task id %q: %v", rest[0], err)
	}
	description := rest[1]
	if description == "-" {
		b, rerr := io.ReadAll(os.Stdin)
		if rerr != nil {
			return errf(5, "read description from stdin: %v", rerr)
		}
		description = string(b)
	}

	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()

	updated, err := st.UpdateTaskDescription(context.Background(), taskID, description)
	if err != nil {
		if errors.Is(err, storage.ErrTaskNotEditable) {
			return errf(4, "task %s is no longer editable (must be pending or scheduled)", taskID)
		}
		return errf(5, "set description: %v", err)
	}
	if updated.Description == "" {
		fmt.Fprintf(os.Stderr, "cleared description on task %s\n", updated.ID)
	} else {
		fmt.Fprintf(os.Stderr, "set description on task %s (%d chars)\n", updated.ID, len([]rune(updated.Description)))
	}
	return 0
}

// allowFlag collects repeatable --allow values (server or server:account).
type allowFlag []string

func (a *allowFlag) String() string { return strings.Join(*a, ",") }
func (a *allowFlag) Set(v string) error {
	*a = append(*a, v)
	return nil
}

// schedTaskSetCredentials sets (or clears) a task's per-task credential allowlist
// (#184): which (server[:account]) MCP pairs the task may call. Each --allow is
// `server` (default seat only) or `server:account`. --clear reverts the task to
// global inherit (NULL). The task must still be editable (pending/scheduled).
func schedTaskSetCredentials(argv []string) int {
	fs := flag.NewFlagSet("sched task set-credentials", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	doClear := fs.Bool("clear", false, "clear the allowlist (revert to global inherit)")
	var allows allowFlag
	fs.Var(&allows, "allow", "permit a server[:account] pair (repeatable)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errf(1, "usage: fleet-admin sched task set-credentials <task_id> --allow server[:account] ... | --clear")
	}
	taskID, err := uuid.Parse(strings.TrimSpace(rest[0]))
	if err != nil {
		return errf(1, "invalid task id %q: %v", rest[0], err)
	}
	if *doClear && len(allows) > 0 {
		return errf(1, "--clear and --allow are mutually exclusive")
	}
	if !*doClear && len(allows) == 0 {
		return errf(1, "provide at least one --allow server[:account], or --clear")
	}

	// --clear → nil (inherit global); otherwise a non-nil list of parsed entries.
	var allowlist models.CredentialAllowlist
	if !*doClear {
		allowlist = models.CredentialAllowlist{}
		for _, a := range allows {
			server, account, _ := strings.Cut(a, ":")
			server = strings.TrimSpace(server)
			account = strings.TrimSpace(account)
			if server == "" {
				return errf(1, "--allow %q has no server", a)
			}
			allowlist = append(allowlist, models.CredentialAllowlistEntry{Server: server, Account: account})
		}
	}

	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()

	updated, err := st.UpdateTaskCredentialAllowlist(context.Background(), taskID, allowlist)
	if err != nil {
		if errors.Is(err, storage.ErrTaskNotEditable) {
			return errf(4, "task %s is no longer editable (must be pending or scheduled)", taskID)
		}
		return errf(5, "set credentials: %v", err)
	}
	if *doClear {
		fmt.Fprintf(os.Stderr, "cleared credential allowlist on task %s (now inherits global)\n", updated.ID)
	} else {
		fmt.Fprintf(os.Stderr, "set credential allowlist on task %s (%d permitted pair(s))\n", updated.ID, len(updated.CredentialAllowlist))
	}
	return 0
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
