package admincli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
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
// memory UUID / sched username / task UUID / log task UUID) and SKIPPED when
// that identity already exists in fleet — re-running an import never
// duplicates data and never reverts state fleet has since written (#713: a
// task that already ran in fleet must not flip back to pending, a leased task
// must not lose its lease, fleet-side run history must not be replaced).
// --overwrite restores the old upsert behavior for the restore-from-bundle
// use case, replacing already-present sched tasks and run logs in place.

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
	FileNames              []string   `json:"file_names,omitempty"`
	// SerializationKey is the opaque per-task mutual-exclusion token (#709,
	// #712): at most one task per key is active at a time, enforced at fleet's
	// claim gate exactly as the legacy scheduler enforced it. Carried verbatim
	// so a live recurring task keeps its serialization guarantee across the
	// migration instead of silently losing it.
	SerializationKey *string `json:"serialization_key,omitempty"`
}

type bundleLog struct {
	TaskID      uuid.UUID       `json:"task_id"`
	SessionData json.RawMessage `json:"session_data"`
}

// importStats accumulates per-kind outcomes for the final summary.
type importStats struct {
	created map[string]int
	skipped map[string]int
	// filtered counts records excluded by an operator FILTER (--live-only),
	// kept apart from skipped so the summary never mislabels a deliberate
	// exclusion as "already present".
	filtered map[string]int
	errors   int
	// warnings are non-fatal per-record notes (clamped role, dropped
	// conversation reference, unparseable terminal-task cron, …).
	warnings []string
	// tasksWithFiles counts imported tasks referencing attachment files — the
	// operator must copy those from the legacy DATA_DIR (see the runbook).
	tasksWithFiles int
	// liveTasksImported counts live (pending/scheduled) tasks written, which
	// arrive with fleet's runtime defaults (sandbox egress OFF, no MCP
	// selection) rather than the legacy scheduler's — the summary tells the
	// operator to re-scope them (#714).
	liveTasksImported int
}

func newImportStats() *importStats {
	return &importStats{created: map[string]int{}, skipped: map[string]int{}, filtered: map[string]int{}}
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
	overwrite := fs.Bool("overwrite", false, "restore mode: replace sched tasks and run logs whose UUID already exists in fleet (default is to skip them, so a re-run never reverts live state)")
	// The flag set mixes value flags (--*-database-url) and boolean flags, and
	// the bundle path may come before OR after them — splitPositionalMixed is
	// the only splitter that parses both orders correctly (#714).
	file, flagArgs := splitPositionalMixed(argv, map[string]bool{
		"dry-run": true, "live-only": true, "overwrite": true,
	})
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if strings.TrimSpace(file) == "" {
		return errf(1, "usage: fleet import <bundle.json> [--dry-run] [--live-only] [--overwrite]")
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
	// same at serve time). Both DSN resolvers fall back to the generic
	// DATABASE_URL, so a setup with only DATABASE_URL set would silently write
	// both stores' schemas into ONE database — via a dual-section bundle in a
	// single run, or via the documented runbook's two single-section bundles
	// across two runs (#714). Refuse both shapes. For a single-section bundle
	// the guard fires only when that section's DSN came from the generic
	// fallback: an explicit --*-database-url flag or FLEET_*_DATABASE_URL var
	// names the target unambiguously and is honored (a genuinely sched-only
	// deployment may point DATABASE_URL at its one database).
	{
		cdsn, cerr := chatDSN(*chatDB)
		sdsn, serr := schedDSN(*schedDB)
		if cerr == nil && serr == nil && cdsn == sdsn {
			sameDB := (bundle.Chat != nil && bundle.Sched != nil) ||
				(bundle.Chat != nil && dsnFromGenericFallback(*chatDB, "FLEET_CHAT_DATABASE_URL")) ||
				(bundle.Sched != nil && dsnFromGenericFallback(*schedDB, "FLEET_SCHED_DATABASE_URL"))
			if sameDB {
				return errf(1, "chat and sched DSNs resolve to the same database, which would mix the two stores' schemas in one DB — set FLEET_CHAT_DATABASE_URL and FLEET_SCHED_DATABASE_URL (or the --*-database-url flags) to distinct databases")
			}
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
		importSchedSection(ctx, st, bundle.Sched, schedImportOpts{
			dryRun:    *dryRun,
			liveOnly:  *liveOnly,
			overwrite: *overwrite,
		}, stats)
		_ = st.Close()
	}

	printImportSummary(stats, *dryRun)
	if stats.errors > 0 {
		return 3
	}
	return 0
}

// dsnFromGenericFallback reports whether a section's DSN resolution reached the
// generic DATABASE_URL fallback: no explicit --*-database-url flag and no
// section-specific FLEET_*_DATABASE_URL var. Used by the same-DSN guard — a
// fallback-resolved target is ambiguous between the two stores, an explicit
// one is operator intent.
func dsnFromGenericFallback(explicit, sectionEnv string) bool {
	return strings.TrimSpace(explicit) == "" && strings.TrimSpace(os.Getenv(sectionEnv)) == ""
}

// chatCatalog is the loaded client-config bundle's MCP-server and persona name
// sets, used to WARN (never fail) when an imported conversation references a
// name the target deployment doesn't know: an unknown optional_mcp_servers_enabled
// entry silently never matches the runtime catalog (the opt-in goes dead), and
// a non-empty unknown persona hard-errors every turn of that conversation
// (#714). Best-effort — when no bundle loads, validation is skipped with one
// warning rather than blocking the migration.
type chatCatalog struct {
	loaded   bool
	servers  map[string]bool
	personas map[string]bool
}

// loadChatCatalog reads the client-config bundle (FLEET_CLIENT_CONFIG_DIR,
// else the baked-in default dir): server names from the MCP catalog plus the
// curated remote-MCP catalog (the two namespaces conversation opt-ins can
// name), persona names from personas/*.yaml basenames (the namespace
// conversation.persona resolves against at prompt-build time).
func loadChatCatalog(stats *importStats) chatCatalog {
	b, err := clientconfig.Load("")
	if err != nil {
		stats.warnf("client-config bundle not loadable (%v) — skipped MCP opt-in / persona validation; verify imported conversations' opt-ins and personas against the deployed bundle manually", err)
		return chatCatalog{}
	}
	cat := chatCatalog{loaded: true, servers: map[string]bool{}, personas: map[string]bool{}}
	for i := range b.MCPCatalog {
		cat.servers[b.MCPCatalog[i].Name] = true
	}
	for i := range b.RemoteMCPCatalog {
		cat.servers[b.RemoteMCPCatalog[i].Name] = true
	}
	if entries, err := os.ReadDir(b.PersonasDir); err == nil {
		for _, e := range entries {
			name := e.Name()
			lower := strings.ToLower(name)
			if strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml") {
				cat.personas[name[:strings.LastIndex(name, ".")]] = true
			}
		}
	}
	return cat
}

// importChatSection applies users → conversations → memories, in that order
// (memories consult which conversations exist). Order within a kind doesn't
// matter — every record is independent, and failures are per-record
// (accumulated in stats) so one bad row never aborts the section mid-run.
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

	// Aggregate catalog mismatches per NAME (not per conversation) so a legacy
	// deployment with hundreds of conversations opted into one renamed server
	// yields one actionable warning, not hundreds.
	cat := loadChatCatalog(stats)
	unknownServers := map[string]int{}
	unknownPersonas := map[string]int{}
	noteCatalogMismatches := func(c store.ImportedConversation) {
		if !cat.loaded {
			return
		}
		for _, name := range c.OptionalMCPServersEnabled {
			if !cat.servers[name] {
				unknownServers[name]++
			}
		}
		if p := strings.TrimSpace(c.Persona); p != "" && !cat.personas[p] {
			unknownPersonas[p]++
		}
	}

	imported := make(map[string]bool, len(sec.Conversations))
	for _, c := range sec.Conversations {
		noteCatalogMismatches(c)
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
					// Per-record, matching the sched section's posture: one
					// failed lookup must not abort the section after users and
					// conversations were already written (#714).
					stats.errorf("memory %s: check conversation %s: %v", m.ID, m.ConversationID, err)
					continue
				}
			}
			if !convExists {
				stats.warnf("memory %s: conversation %s not present — reference dropped", m.ID, m.ConversationID)
			}
		}
		if dryRun {
			// Existence check so the plan's created/skipped split matches what
			// the real run will do (#714) — previously every memory was counted
			// "created" even when already present.
			exists, err := chatStore.HasMemory(ctx, m.ID)
			if err != nil {
				stats.errorf("memory %s: %v", m.ID, err)
				continue
			}
			bump(stats, "memories", !exists)
			continue
		}
		created, err := chatStore.ImportMemory(ctx, m, convExists)
		if err != nil {
			stats.errorf("%v", err)
			continue
		}
		bump(stats, "memories", created)
	}

	// Catalog-mismatch warnings (#714), one per distinct name (sorted for a
	// stable summary). Opt-ins go silently dead (the stored name never matches
	// the runtime catalog — legacy chat stored un-suffixed names while bundles
	// use *_mcp); an unknown persona is worse, hard-erroring every turn on that
	// conversation until reassigned.
	for _, name := range sortedKeys(unknownServers) {
		stats.warnf("optional MCP server %q (enabled on %d conversation(s)) is not in the loaded catalog — the opt-in will be inert until a catalog server with that exact name exists", name, unknownServers[name])
	}
	for _, name := range sortedKeys(unknownPersonas) {
		stats.warnf("persona %q (set on %d conversation(s)) is not in the loaded bundle's personas/ — every turn on those conversations will fail until the persona is added or reassigned", name, unknownPersonas[name])
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

// schedImportOpts carries the operator switches for the sched section.
type schedImportOpts struct {
	dryRun bool
	// liveOnly imports only live (pending/scheduled) tasks, dropping terminal
	// history and run logs.
	liveOnly bool
	// overwrite replaces tasks/logs whose UUID already exists in fleet (restore
	// mode). Default false: already-present rows are skipped so a re-run can
	// never revert live task state or replace fleet-side run history (#713).
	overwrite bool
}

// importSchedSection applies users → tasks → logs. Users first so task
// created_by remapping can consult them; logs last so they attach to imported
// task ids. Every failure here is per-record (accumulated in stats) — nothing
// aborts the section.
func importSchedSection(ctx context.Context, st *storage.Storage, sec *schedSection, opts schedImportOpts, stats *importStats) {
	dryRun := opts.dryRun
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

	// presentTasks tracks every task UUID that exists (or will exist after this
	// run) in fleet, so the log pass can reject an orphan log — one whose task
	// is neither in the bundle nor in fleet — identically in dry-run and real
	// runs, instead of dry-run passing what the real run's FK would fail (#714).
	presentTasks := make(map[uuid.UUID]bool, len(sec.Tasks))
	for _, bt := range sec.Tasks {
		task, live, err := buildImportedTask(st, bt, remap, stats)
		if err != nil {
			stats.errorf("task %s: %v", bt.ID, err)
			continue
		}
		if opts.liveOnly && !live {
			stats.filtered["tasks"]++
			continue
		}
		exists, err := st.TaskExists(ctx, bt.ID)
		if err != nil {
			stats.errorf("task %s: %v", bt.ID, err)
			continue
		}
		if exists {
			presentTasks[bt.ID] = true
			if !opts.overwrite {
				// Skip-by-default (#713): fleet's row may have progressed (run,
				// been leased, been cancelled) since the last import — writing
				// the bundle's snapshot over it would flip a completed one-shot
				// back to pending (double execution) or null an active lease.
				bump(stats, "tasks", false)
				continue
			}
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
		presentTasks[bt.ID] = true
		if live {
			stats.liveTasksImported++
		}
		bump(stats, "tasks", true)
	}

	if opts.liveOnly {
		if n := len(sec.Logs); n > 0 {
			stats.filtered["run logs"] += n
		}
		return
	}
	for _, l := range sec.Logs {
		if l.TaskID == uuid.Nil || len(l.SessionData) == 0 || !json.Valid(l.SessionData) {
			stats.errorf("log %s: task_id and valid session_data JSON required", l.TaskID)
			continue
		}
		// Orphan check, identical in dry-run and real runs: a log whose task is
		// neither in the bundle nor already in fleet would fail the logs→tasks
		// FK on write (#714).
		present := presentTasks[l.TaskID]
		if !present {
			var err error
			present, err = st.TaskExists(ctx, l.TaskID)
			if err != nil {
				stats.errorf("log %s: %v", l.TaskID, err)
				continue
			}
		}
		if !present {
			stats.errorf("log %s: no matching task in the bundle or in fleet — run log not importable", l.TaskID)
			continue
		}
		exists, err := st.LogExists(ctx, l.TaskID)
		if err != nil {
			stats.errorf("log %s: %v", l.TaskID, err)
			continue
		}
		if exists && !opts.overwrite {
			// Skip-by-default (#713): fleet may have re-run the task since the
			// last import; its run history must survive a re-import.
			bump(stats, "run logs", false)
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
		FileNames:              bt.FileNames,
		// SerializationKey (#712): carried verbatim (NewTask normalizes
		// empty/whitespace to nil) so a live recurring task keeps its ≤1-active
		// per-key guarantee across the migration.
		SerializationKey: bt.SerializationKey,
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

	// Inert-task check (#714): status "scheduled" with no recurrence and no
	// scheduled_for passes every validation but never fires — the scheduler's
	// due query requires a non-NULL scheduled_for. Keep the row (normalizing to
	// pending would surprise the operator by running it immediately) but say so.
	if task.Status == models.TaskStatusScheduled && task.ScheduledFor == nil && bt.Recurrence == "" {
		stats.warnf("task %s: status \"scheduled\" with no recurrence and no scheduled_for — it will never fire; reschedule or cancel it after import", bt.ID)
	}
	return task, live, nil
}

// sortedKeys returns a map's keys in ascending order, so aggregated warnings
// print deterministically.
func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
		c, s, f := stats.created[kind], stats.skipped[kind], stats.filtered[kind]
		if c == 0 && s == 0 && f == 0 {
			continue
		}
		line := fmt.Sprintf("%-12s %s %d, skipped %d (already present)", kind+":", verb, c, s)
		if f > 0 {
			line += fmt.Sprintf(", skipped %d (--live-only)", f)
		}
		fmt.Println(line)
	}
	if stats.tasksWithFiles > 0 {
		fmt.Printf("note: %d task(s) reference attachment files — copy them from the legacy DATA_DIR (docs/LEGACY-IMPORT.md step 4)\n", stats.tasksWithFiles)
	}
	if stats.liveTasksImported > 0 {
		fmt.Printf("note: %d live task(s) imported with fleet's runtime defaults — sandbox egress OFF (allow_network=false) and no MCP server selection. v1 runs had full egress and prompt-driven MCP loading, so re-scope tasks that need network or connectors before their next run (docs/LEGACY-IMPORT.md).\n", stats.liveTasksImported)
	}
	for _, w := range stats.warnings {
		fmt.Printf("warning: %s\n", w)
	}
	if stats.errors > 0 {
		fmt.Printf("%d record(s) failed — fix and re-run; records already present in fleet are skipped on re-import (pass --overwrite to replace them)\n", stats.errors)
	}
}
