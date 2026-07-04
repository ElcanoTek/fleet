package admincli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/store"
)

// cmdImport implements `fleet import <bundle.json>` — the one-time legacy
// migration ingest (docs/LEGACY-IMPORT.md). The bundle is a versioned JSON
// envelope produced by the deprecated chat repo's `chat-admin export` and the
// deprecated moc repo's `moc -export-fleet`; each exporter owns the mapping
// from ITS legacy schema into this format, so fleet stays generic: it only
// knows how to route the `chat` section into the chat store and the `sched`
// section into the sched store.
//
//	fleet import chat-bundle.json --dry-run
//	fleet import chat-bundle.json
//	fleet import moc-bundle.json --live-only
//
// Every write is keyed on the source identity (email / conversation UUID /
// memory UUID / sched username / task UUID / log task UUID) and either skips
// or idempotently upserts — re-running an import never duplicates data.

// migrationBundleFormat / migrationBundleVersion pin the envelope contract.
// Import rejects an unknown format or version rather than guessing.
const (
	migrationBundleFormat  = "fleet-migration-bundle"
	migrationBundleVersion = 1
)

// migrationBundle is the top-level envelope. Sections are optional: a chat
// export carries only Chat, a moc export only Sched.
type migrationBundle struct {
	Format     string        `json:"format"`
	Version    int           `json:"version"`
	ExportedAt string        `json:"exported_at,omitempty"`
	Source     string        `json:"source,omitempty"`
	Chat       *chatSection  `json:"chat,omitempty"`
	Sched      *schedSection `json:"sched,omitempty"`
}

// chatSection reuses the store's import record types directly — they ARE the
// documented wire shape (timestamps in unix seconds, the chat store's native
// representation).
type chatSection struct {
	Users         []store.ImportedUser         `json:"users,omitempty"`
	Conversations []store.ImportedConversation `json:"conversations,omitempty"`
	Memories      []store.ImportedMemory       `json:"memories,omitempty"`
}

// schedSection carries sched users, tasks (definitions PLUS identity and
// runtime state, unlike the definition-only TaskExportRecord), and raw run
// logs. Timestamps are RFC3339, the sched store's native representation.
type schedSection struct {
	Users []bundleSchedUser `json:"users,omitempty"`
	Tasks []bundleTask      `json:"tasks,omitempty"`
	Logs  []bundleLog       `json:"logs,omitempty"`
}

type bundleSchedUser struct {
	ID           uuid.UUID  `json:"id"`
	Username     string     `json:"username"`
	PasswordHash string     `json:"password_hash"`
	Role         string     `json:"role"`
	Scopes       []string   `json:"scopes,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLogin    *time.Time `json:"last_login,omitempty"`
}

type bundleTask struct {
	ID                     uuid.UUID  `json:"id"`
	Prompt                 string     `json:"prompt"`
	Name                   string     `json:"name,omitempty"`
	Description            string     `json:"description,omitempty"`
	Model                  *string    `json:"model,omitempty"`
	FallbackModel          *string    `json:"fallback_model,omitempty"`
	MaxIterations          *int       `json:"max_iterations,omitempty"`
	Priority               int        `json:"priority"`
	InstructionSelfImprove bool       `json:"instruction_self_improve,omitempty"`
	Status                 string     `json:"status"`
	CreatedAt              time.Time  `json:"created_at"`
	StartedAt              *time.Time `json:"started_at,omitempty"`
	CompletedAt            *time.Time `json:"completed_at,omitempty"`
	Result                 *string    `json:"result,omitempty"`
	ErrorMessage           *string    `json:"error_message,omitempty"`
	AgentSessionID         *string    `json:"agent_session_id,omitempty"`
	ScheduledFor           *time.Time `json:"scheduled_for,omitempty"`
	Recurrence             string     `json:"recurrence,omitempty"`
	Timezone               string     `json:"timezone,omitempty"`
	CreatedBy              *uuid.UUID `json:"created_by,omitempty"`
	Files                  []string   `json:"files,omitempty"`
}

type bundleLog struct {
	TaskID      uuid.UUID       `json:"task_id"`
	SessionData json.RawMessage `json:"session_data"`
}

// importStats accumulates per-kind outcomes for the final summary.
type importStats struct {
	created map[string]int
	skipped map[string]int
	errors  int
	// warnings are non-fatal per-record notes (clamped role, dropped
	// conversation reference, unparseable terminal-task cron, …).
	warnings []string
	// tasksWithFiles counts imported tasks referencing attachment files — the
	// operator must copy those from the legacy DATA_DIR (see the runbook).
	tasksWithFiles int
}

func newImportStats() *importStats {
	return &importStats{created: map[string]int{}, skipped: map[string]int{}}
}

func (st *importStats) warnf(format string, a ...any) {
	st.warnings = append(st.warnings, fmt.Sprintf(format, a...))
}

func (st *importStats) errorf(format string, a ...any) {
	st.errors++
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
}

func cmdImport(argv []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	chatDB := fs.String("chat-database-url", "", "chat Postgres DSN (default FLEET_CHAT_DATABASE_URL, then DATABASE_URL)")
	schedDB := fs.String("sched-database-url", "", "sched Postgres DSN (default FLEET_SCHED_DATABASE_URL, then DATABASE_URL)")
	dryRun := fs.Bool("dry-run", false, "validate and print the plan without writing")
	liveOnly := fs.Bool("live-only", false, "sched section: import only live (pending/scheduled) tasks; skip terminal tasks and run logs")
	file, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if strings.TrimSpace(file) == "" {
		return errf(1, "usage: fleet import <bundle.json> [--dry-run] [--live-only]")
	}

	//nolint:gosec // G304: the bundle path is an operator-supplied CLI positional (like backup/restore's dump paths), never request or LLM input.
	body, err := os.ReadFile(file)
	if err != nil {
		return errf(1, "read bundle: %v", err)
	}
	var bundle migrationBundle
	if err := json.Unmarshal(body, &bundle); err != nil {
		return errf(1, "parse bundle: %v", err)
	}
	if bundle.Format != migrationBundleFormat {
		return errf(1, "not a migration bundle: format is %q (want %q)", bundle.Format, migrationBundleFormat)
	}
	if bundle.Version != migrationBundleVersion {
		return errf(1, "unsupported bundle version %d (this build imports version %d)", bundle.Version, migrationBundleVersion)
	}
	if bundle.Chat == nil && bundle.Sched == nil {
		return errf(1, "bundle has neither a chat nor a sched section — nothing to import")
	}

	// The chat and sched stores are DISTINCT databases (cmd/fleet asserts the
	// same at serve time). Both DSN resolvers fall back to DATABASE_URL, so a
	// dual-section bundle with only DATABASE_URL set would silently write both
	// sections into one database — refuse instead.
	if bundle.Chat != nil && bundle.Sched != nil {
		cdsn, cerr := chatDSN(*chatDB)
		sdsn, serr := schedDSN(*schedDB)
		if cerr == nil && serr == nil && cdsn == sdsn {
			return errf(1, "chat and sched DSNs resolve to the same database — set FLEET_CHAT_DATABASE_URL and FLEET_SCHED_DATABASE_URL (or the --*-database-url flags) to distinct databases")
		}
	}

	ctx := context.Background()
	stats := newImportStats()
	if bundle.Source != "" {
		fmt.Printf("bundle source: %s (exported_at %s)\n", bundle.Source, bundle.ExportedAt)
	}
	if *dryRun {
		fmt.Println("dry-run: validating and planning only — nothing will be written")
	}

	if bundle.Chat != nil {
		dsn, err := chatDSN(*chatDB)
		if err != nil {
			return errf(1, "%v", err)
		}
		chatStore, err := store.Open(dsn, store.DefaultPoolConfig())
		if err != nil {
			return errf(1, "open chat DB: %v", err)
		}
		err = importChatSection(ctx, chatStore, bundle.Chat, *dryRun, stats)
		_ = chatStore.Close()
		if err != nil {
			return errf(5, "chat section: %v", err)
		}
	}

	if bundle.Sched != nil {
		st, code := openSchedStorage(*schedDB)
		if st == nil {
			return code
		}
		importSchedSection(ctx, st, bundle.Sched, *dryRun, *liveOnly, stats)
		_ = st.Close()
	}

	printImportSummary(stats, *dryRun)
	if stats.errors > 0 {
		return 3
	}
	return 0
}

// importChatSection applies users → conversations → memories, in that order
// (memories consult which conversations exist). Order within a kind doesn't
// matter — every record is independent.
func importChatSection(ctx context.Context, chatStore *store.Store, sec *chatSection, dryRun bool, stats *importStats) error {
	for _, u := range sec.Users {
		if dryRun {
			exists, err := chatStore.IsUser(ctx, u.Email)
			if err != nil {
				return fmt.Errorf("check user %s: %w", u.Email, err)
			}
			bump(stats, "chat users", !exists)
			continue
		}
		created, err := chatStore.ImportUser(ctx, u)
		if err != nil {
			stats.errorf("%v", err)
			continue
		}
		bump(stats, "chat users", created)
	}

	imported := make(map[string]bool, len(sec.Conversations))
	for _, c := range sec.Conversations {
		if dryRun {
			exists, err := chatStore.HasConversation(ctx, c.ID)
			if err != nil {
				return fmt.Errorf("check conversation %s: %w", c.ID, err)
			}
			imported[c.ID] = true
			bump(stats, "conversations", !exists)
			if !exists {
				stats.created["messages"] += len(c.Messages)
			} else {
				stats.skipped["messages"] += len(c.Messages)
			}
			continue
		}
		created, err := chatStore.ImportConversation(ctx, c)
		if err != nil {
			stats.errorf("%v", err)
			continue
		}
		imported[c.ID] = true
		bump(stats, "conversations", created)
		if created {
			stats.created["messages"] += len(c.Messages)
		} else {
			stats.skipped["messages"] += len(c.Messages)
		}
	}

	for _, m := range sec.Memories {
		convExists := false
		if m.ConversationID != "" {
			convExists = imported[m.ConversationID]
			if !convExists {
				var err error
				convExists, err = chatStore.HasConversation(ctx, m.ConversationID)
				if err != nil {
					return fmt.Errorf("check conversation %s: %w", m.ConversationID, err)
				}
			}
			if !convExists {
				stats.warnf("memory %s: conversation %s not present — reference dropped", m.ID, m.ConversationID)
			}
		}
		if dryRun {
			bump(stats, "memories", true) // exact skip count needs a write attempt; plan optimistically
			continue
		}
		created, err := chatStore.ImportMemory(ctx, m, convExists)
		if err != nil {
			stats.errorf("%v", err)
			continue
		}
		bump(stats, "memories", created)
	}

	// Make migrated history searchable. BackfillSearchContent pages the whole
	// messages table for rows missing from the FTS side-table — idempotent and
	// batched, exactly what the server runs at startup.
	if !dryRun {
		if n, err := chatStore.BackfillSearchContent(ctx); err != nil {
			stats.warnf("FTS backfill: %v (the server re-runs it at startup)", err)
		} else if n > 0 {
			fmt.Printf("indexed %d imported message(s) for search\n", n)
		}
	}
	return nil
}

// validSchedTaskStatus is the set of statuses a bundle task may carry. The
// exporters normalize anything transient (moc's assigned/leased/running) to a
// terminal status before export; import stays strict rather than guessing —
// the transient/paused states are explicitly rejected because they carry
// lease/pending state a migrated row can't honor.
func validSchedTaskStatus(s models.TaskStatus) bool {
	switch s {
	case models.TaskStatusPending, models.TaskStatusScheduled,
		models.TaskStatusSuccess, models.TaskStatusError,
		models.TaskStatusCancelled, models.TaskStatusDeadLettered:
		return true
	case models.TaskStatusLeased, models.TaskStatusRunning,
		models.TaskStatusAnalyzing, models.TaskStatusPausedAwaitingInput:
		return false
	default:
		return false
	}
}

// importSchedSection applies users → tasks → logs. Users first so task
// created_by remapping can consult them; logs last so they attach to imported
// task ids. Unlike the chat section, every failure here is per-record
// (accumulated in stats) — nothing aborts the section.
func importSchedSection(ctx context.Context, st *storage.Storage, sec *schedSection, dryRun, liveOnly bool, stats *importStats) {
	// Users: matched by username. An existing account (e.g. the bootstrap
	// admin) wins and imported tasks are re-attributed to its UUID; a new
	// username is inserted with its source UUID + hash so logins keep working.
	remap := make(map[uuid.UUID]uuid.UUID, len(sec.Users))
	for _, u := range sec.Users {
		username := strings.TrimSpace(u.Username)
		if username == "" || u.ID == uuid.Nil || strings.TrimSpace(u.PasswordHash) == "" {
			stats.errorf("sched user %q (%s): username, id, and password_hash are all required", u.Username, u.ID)
			continue
		}
		existing, err := st.GetUserByUsernameWithContext(ctx, username)
		if err == nil && existing != nil {
			remap[u.ID] = existing.ID
			bump(stats, "sched users", false)
			continue
		}
		role := u.Role
		if !validRole(role) {
			stats.warnf("sched user %q: unknown role %q clamped to readonly", username, role)
			role = "readonly"
		}
		if !dryRun {
			if _, err := st.AddUser(&models.User{
				ID:           u.ID,
				Username:     username,
				PasswordHash: u.PasswordHash,
				Role:         role,
				Scopes:       u.Scopes,
				CreatedAt:    u.CreatedAt,
				LastLogin:    u.LastLogin,
			}); err != nil {
				stats.errorf("sched user %q: %v", username, err)
				continue
			}
		}
		bump(stats, "sched users", true)
	}

	importedTasks := make(map[uuid.UUID]bool, len(sec.Tasks))
	for _, bt := range sec.Tasks {
		task, live, err := buildImportedTask(st, bt, remap, stats)
		if err != nil {
			stats.errorf("task %s: %v", bt.ID, err)
			continue
		}
		if liveOnly && !live {
			bump(stats, "tasks", false)
			continue
		}
		if len(bt.Files) > 0 {
			stats.tasksWithFiles++
		}
		if !dryRun {
			if _, err := st.AddTaskWithContext(ctx, task); err != nil {
				stats.errorf("task %s: %v", bt.ID, err)
				continue
			}
		}
		importedTasks[bt.ID] = true
		bump(stats, "tasks", true)
	}

	if liveOnly {
		if n := len(sec.Logs); n > 0 {
			stats.warnf("--live-only: skipped %d run log(s)", n)
		}
		return
	}
	for _, l := range sec.Logs {
		if l.TaskID == uuid.Nil || len(l.SessionData) == 0 || !json.Valid(l.SessionData) {
			stats.errorf("log %s: task_id and valid session_data JSON required", l.TaskID)
			continue
		}
		if !dryRun {
			if err := st.ImportLogRaw(ctx, l.TaskID, l.SessionData); err != nil {
				stats.errorf("log %s: %v", l.TaskID, err)
				continue
			}
		}
		bump(stats, "run logs", true)
	}
}

// buildImportedTask converts one bundle task into a models.Task: minted through
// models.NewTask so every normalization (priority clamp, SLA defaults, timezone
// fallback, trigger type) matches a task created through the public API, then
// overlaid with the preserved source identity + runtime state. The bool result
// reports whether the task is live (pending/scheduled) as opposed to history.
func buildImportedTask(st *storage.Storage, bt bundleTask, remap map[uuid.UUID]uuid.UUID, stats *importStats) (*models.Task, bool, error) {
	if bt.ID == uuid.Nil {
		return nil, false, fmt.Errorf("missing id")
	}
	if strings.TrimSpace(bt.Prompt) == "" {
		return nil, false, fmt.Errorf("empty prompt")
	}
	status := models.TaskStatus(bt.Status)
	if !validSchedTaskStatus(status) {
		return nil, false, fmt.Errorf("unsupported status %q (exporters must normalize transient statuses)", bt.Status)
	}
	live := status == models.TaskStatusPending || status == models.TaskStatusScheduled

	tz := strings.TrimSpace(bt.Timezone)
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			stats.warnf("task %s: unknown timezone %q — falling back to UTC", bt.ID, tz)
			tz = ""
		}
	}

	if bt.Recurrence != "" {
		if _, err := cron.ParseStandard(bt.Recurrence); err != nil {
			if live {
				return nil, live, fmt.Errorf("invalid recurrence %q: %w", bt.Recurrence, err)
			}
			// Terminal history: keep the row, the cron spec is never evaluated.
			stats.warnf("task %s: terminal task carries unparseable recurrence %q (kept as history)", bt.ID, bt.Recurrence)
		}
	}

	task := models.NewTask(models.TaskCreate{
		Name:                   bt.Name,
		Prompt:                 bt.Prompt,
		Description:            bt.Description,
		Model:                  bt.Model,
		FallbackModel:          bt.FallbackModel,
		MaxIterations:          bt.MaxIterations,
		Priority:               bt.Priority,
		InstructionSelfImprove: bt.InstructionSelfImprove,
		ScheduledFor:           bt.ScheduledFor,
		Recurrence:             bt.Recurrence,
		Timezone:               tz,
		Files:                  bt.Files,
	})

	// Overlay the preserved source identity + runtime state.
	task.ID = bt.ID
	task.Status = status
	task.CreatedAt = bt.CreatedAt.UTC()
	task.StartedAt = bt.StartedAt
	task.CompletedAt = bt.CompletedAt
	task.Result = bt.Result
	task.ErrorMessage = bt.ErrorMessage
	task.AgentSessionID = bt.AgentSessionID
	if bt.CreatedBy != nil {
		owner := *bt.CreatedBy
		if mapped, ok := remap[owner]; ok {
			owner = mapped
		}
		task.CreatedBy = &owner
	}

	// Scheduling normalization for LIVE tasks only (history keeps its
	// timestamps verbatim): a recurring task whose next fire time was consumed
	// or is in the past gets the next tick recomputed in ITS timezone — the
	// same thing moc's own importer did. A one-shot with a past scheduled_for
	// stays due and fires shortly after import.
	if live && bt.Recurrence != "" &&
		(task.ScheduledFor == nil || !task.ScheduledFor.After(time.Now())) {
		next, err := st.ComputeNextRun(task)
		if err != nil {
			return nil, live, fmt.Errorf("compute next run: %w", err)
		}
		task.ScheduledFor = &next
		task.Status = models.TaskStatusScheduled
	}
	return task, live, nil
}

func bump(stats *importStats, kind string, created bool) {
	if created {
		stats.created[kind]++
	} else {
		stats.skipped[kind]++
	}
}

// printImportSummary emits the per-kind outcome table plus accumulated
// warnings, in a stable order.
func printImportSummary(stats *importStats, dryRun bool) {
	verb := "imported"
	if dryRun {
		verb = "would import"
	}
	order := []string{"chat users", "conversations", "messages", "memories", "sched users", "tasks", "run logs"}
	for _, kind := range order {
		c, s := stats.created[kind], stats.skipped[kind]
		if c == 0 && s == 0 {
			continue
		}
		fmt.Printf("%-12s %s %d, skipped %d (already present)\n", kind+":", verb, c, s)
	}
	if stats.tasksWithFiles > 0 {
		fmt.Printf("note: %d task(s) reference attachment files — copy them from the legacy DATA_DIR (docs/LEGACY-IMPORT.md step 4)\n", stats.tasksWithFiles)
	}
	for _, w := range stats.warnings {
		fmt.Printf("warning: %s\n", w)
	}
	if stats.errors > 0 {
		fmt.Printf("%d record(s) failed — fix and re-run; completed records are skipped on re-import\n", stats.errors)
	}
}
