// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/pgtext"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// taskExportMaxRecords caps the number of records an import accepts in one
// request, mirroring the bulk-API 100-ID limit the conversation-operations
// endpoints use. It keeps a single import body bounded so a runaway payload
// can't pin a DB transaction or exhaust the request body budget.
const taskExportMaxRecords = 100

// HandleTaskExport handles GET /tasks/export (#238). It returns a versioned
// envelope of task DEFINITIONS (not runs or logs — only configuration) as JSON
// (default) or YAML, with optional ?ids= and ?recurrence_only= filters. The
// response carries Content-Disposition so browsers save the file directly. It
// requires view_tasks (the same gate as GET /tasks); the route is registered in
// the admin-or-user group.
func (h *Handlers) HandleTaskExport(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	// ?ids=id1,id2 — comma-separated UUIDs; omit for all tasks.
	var ids []uuid.UUID
	if raw := strings.TrimSpace(r.URL.Query().Get("ids")); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			id, err := uuid.Parse(s)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid id: "+s)
				return
			}
			ids = append(ids, id)
		}
	}

	recurrenceOnly := r.URL.Query().Get("recurrence_only") == "true"

	tasks, err := h.storage.ListTasksForExport(r.Context(), ids, recurrenceOnly)
	if err != nil {
		logging.From(r.Context()).Error("handleTaskExport: list failed", "err", err)
		writeError(w, http.StatusInternalServerError, "export failed")
		return
	}
	// Own-rows visibility (#1082): the export bundle carries full task
	// definitions (prompts included), so it is scoped like GET /tasks — a
	// principal without the fleet-wide grant exports only its own tasks.
	tasks = visibleTasks(p, tasks)

	records := make([]models.TaskExportRecord, 0, len(tasks))
	for _, t := range tasks {
		records = append(records, models.TaskToExportRecord(t))
	}
	envelope := models.TaskExportEnvelope{
		Version:    models.TaskExportVersion,
		ExportedAt: time.Now().UTC(),
		Tasks:      records,
	}

	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "json"
	}
	stamp := time.Now().UTC().Format("2006-01-02")
	switch format {
	case "json":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="fleet-tasks-`+stamp+`.json"`)
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(envelope); err != nil {
			logging.From(r.Context()).Error("handleTaskExport: json encode failed", "err", err)
		}
	case "yaml":
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Content-Disposition", `attachment; filename="fleet-tasks-`+stamp+`.yaml"`)
		w.WriteHeader(http.StatusOK)
		if err := yaml.NewEncoder(w, yaml.Indent(2)).Encode(envelope); err != nil {
			logging.From(r.Context()).Error("handleTaskExport: yaml encode failed", "err", err)
		}
	default:
		writeError(w, http.StatusBadRequest, "unsupported format: "+format+" (want json or yaml)")
	}
}

// HandleTaskImport handles POST /tasks/import (#238). It accepts the same JSON
// or YAML envelope as GET /tasks/export and creates tasks in a single batch,
// returning per-task results. ?dry_run=true validates and returns the plan
// without writing. ?conflict=skip|replace|error (default error) controls
// name-collision handling; conflict=replace additionally requires admin.
//
// Permission: create_task (the same gate as POST /tasks). conflict=replace is a
// fleet-wide mutation of existing tasks, so it additionally requires admin.
func (h *Handlers) HandleTaskImport(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionCreateTask) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	dryRun := r.URL.Query().Get("dry_run") == "true"
	conflict := models.TaskImportConflict(strings.ToLower(strings.TrimSpace(r.URL.Query().Get("conflict"))))
	if conflict == "" {
		conflict = models.TaskImportConflictError
	}
	switch conflict {
	case models.TaskImportConflictError, models.TaskImportConflictSkip, models.TaskImportConflictReplace:
	default:
		writeError(w, http.StatusBadRequest, "invalid conflict: "+string(conflict)+" (want error, skip, or replace)")
		return
	}
	// conflict=replace mutates existing tasks fleet-wide — admin-only, matching
	// the BulkSetTaskModel gate.
	if conflict == models.TaskImportConflictReplace && !p.hasPermission(models.PermissionAdmin) {
		writeError(w, http.StatusForbidden, "conflict=replace requires admin permission")
		return
	}

	var envelope models.TaskExportEnvelope
	if err := decodeImportEnvelope(r, &envelope); err != nil {
		writeError(w, http.StatusBadRequest, "invalid import body: "+err.Error())
		return
	}
	if envelope.Version != models.TaskExportVersion {
		writeError(w, http.StatusBadRequest, "unsupported export version: "+envelope.Version+" (this build imports version "+models.TaskExportVersion+")")
		return
	}
	if len(envelope.Tasks) > taskExportMaxRecords {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("import accepts at most %d records (got %d)", taskExportMaxRecords, len(envelope.Tasks)))
		return
	}

	// Validate every record up front so a bad payload writes nothing, mirroring
	// the existing sched task import path. Empty prompts and unparseable cron
	// expressions are rejected; runtime fields are ignored (they never appear in
	// TaskExportRecord).
	for i, rec := range envelope.Tasks {
		if err := validateExportRecord(rec); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("task[%d] (name=%s): %v", i, rec.Name, err))
			return
		}
	}

	// Detect duplicate names WITHIN the import payload itself — two records
	// sharing a name can't both be created (the second would collide with the
	// first), and conflict=error/skip/replace all treat intra-batch dupes as an
	// error rather than a silent overwrite.
	seen := make(map[string]int)
	for i, rec := range envelope.Tasks {
		name := strings.TrimSpace(rec.Name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("duplicate name %q in import payload (record %d)", name, i))
			return
		}
		seen[name] = i
	}

	// Pre-flight: resolve existing tasks by name so conflict handling can decide
	// per-record without N round-trips.
	names := make([]string, 0, len(envelope.Tasks))
	for _, rec := range envelope.Tasks {
		if n := strings.TrimSpace(rec.Name); n != "" {
			names = append(names, n)
		}
	}
	existing, err := h.storage.FindTaskIDsByName(r.Context(), names)
	if err != nil {
		logging.From(r.Context()).Error("handleTaskImport: find by name failed", "err", err)
		writeError(w, http.StatusInternalServerError, "import failed")
		return
	}

	// conflict=error aborts the whole import before any write when ANY name
	// collides.
	if conflict == models.TaskImportConflictError && len(existing) > 0 {
		colliding := make([]string, 0, len(existing))
		for n := range existing {
			colliding = append(colliding, n)
		}
		writeError(w, http.StatusConflict, fmt.Sprintf("%d task(s) already exist: %s", len(existing), strings.Join(colliding, ", ")))
		return
	}

	// Every record this import writes (created or replaced) runs the SAME
	// gates as POST /tasks: admitCreate per created record, admitEdit per
	// replaced one. A record conflict=skip leaves alone writes nothing, so it
	// is neither checked nor charged.
	plan := planImport(envelope.Tasks, existing, conflict)
	gates := taskGatesFor(creatorFromPrincipal(p))
	if err := h.importContentRefusal(gates.creator.hasAdminPermission, envelope.Tasks, plan); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	// The uploads each record to create names (importFileRefusals), checked
	// before any write, dry runs included: a refused name fails that record,
	// and a failed lookup fails the import, like the name lookup above.
	fileRefusals, err := h.importFileRefusals(r.Context(), gates.caller, envelope.Tasks, plan)
	if err != nil {
		logging.From(r.Context()).Error("handleTaskImport: upload lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "import failed")
		return
	}
	gates.uploadsChecked = true

	// The per-principal gates, once for the import. The rolling budget
	// (#601 part 2) bounds the principal's new tasks, not any one record, so it
	// runs when the import creates at least one: an exhausted window refuses
	// the import before any row is written. A replace edits an existing task,
	// and PUT /tasks/{id} has no budget gate either; a replace that finds its
	// row gone and creates instead meets the budget in admitCreate. The
	// importing key's hourly counter is charged one request per record the
	// import creates or replaces, as on /tasks/batch.
	//
	// A dry run runs neither: it writes no row (every create and replace below
	// is behind `if !dryRun`), so refusing it on an exhausted window would deny
	// the plan precisely when an operator most needs to see what an import
	// would do.
	if !dryRun {
		if plan.creates > 0 {
			if err := h.checkBudget(r.Context(), gates); err != nil {
				writeBudgetRefusal(w, err)
				return
			}
		}
		if plan.creates+plan.replaces > 0 && !h.chargeRecords(gates, plan.creates+plan.replaces) {
			writeError(w, http.StatusTooManyRequests, "Rate limit exceeded")
			return
		}
	}

	resp := models.TaskImportResponse{DryRun: dryRun, Total: len(envelope.Tasks), Results: []models.TaskImportResult{}}
	for i, rec := range envelope.Tasks {
		result := h.importRecord(r, rec, plan.actions[i], conflict, gates, dryRun, fileRefusals[i])
		switch result.Status {
		case models.TaskImportSkipped:
			resp.Skipped++
		case models.TaskImportReplaced:
			resp.Replaced++
		case models.TaskImportCreated:
			resp.Created++
		case models.TaskImportErrored:
			resp.Errors++
		}
		resp.Results = append(resp.Results, result)
	}

	// 207 multi-status when any record errored but others succeeded; 200
	// otherwise (incl. dry_run, which never writes).
	status := http.StatusOK
	if resp.Errors > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, r, status, resp)
}

// importAction is what an import does with one record.
type importAction int

const (
	importCreate importAction = iota
	importReplace
	importSkip
)

// importPlan is each record's action, and how many records the import
// creates and replaces.
type importPlan struct {
	actions           []importAction
	creates, replaces int
}

// planImport decides each record's action from the names that already exist.
// A colliding record is replaced under conflict=replace and left alone under
// conflict=skip (conflict=error refused the whole import before this); every
// other record, unnamed ones included, is created.
func planImport(records []models.TaskExportRecord, existing map[string]uuid.UUID, conflict models.TaskImportConflict) importPlan {
	plan := importPlan{actions: make([]importAction, len(records))}
	for i, rec := range records {
		_, collision := existing[strings.TrimSpace(rec.Name)]
		switch {
		case collision && conflict == models.TaskImportConflictSkip:
			plan.actions[i] = importSkip
		case collision && conflict == models.TaskImportConflictReplace:
			plan.actions[i] = importReplace
			plan.replaces++
		default:
			plan.actions[i] = importCreate
			plan.creates++
		}
	}
	return plan
}

// importRecord carries out one record's planned action (nothing on a dry
// run) and reports it. A planned create whose name another request took after
// the plan was made answers like the plan would have: skipped under
// conflict=skip, otherwise an error naming the conflict. filesRefused is the
// record's upload refusal (importFileRefusals): a dry run reports it for a
// planned create, and a write hands it to admitCreate (taskGates
// uploadRefusal), which answers it after validation, as POST /tasks does.
func (h *Handlers) importRecord(r *http.Request, rec models.TaskExportRecord, action importAction, conflict models.TaskImportConflict, gates *taskGates, dryRun bool, filesRefused error) models.TaskImportResult {
	result := models.TaskImportResult{Name: rec.Name}
	gates.uploadRefusal = filesRefused
	skipped := func() models.TaskImportResult {
		result.Status = models.TaskImportSkipped
		result.Reason = "conflict=skip"
		return result
	}
	switch action {
	case importSkip:
		return skipped()
	case importReplace:
		result.Status = models.TaskImportReplaced
		if dryRun {
			return result
		}
		id, err := h.replaceTaskByName(r, rec, gates)
		if err != nil {
			result.Status, result.Error = models.TaskImportErrored, err.Error()
			return result
		}
		result.ID = &id
		return result
	default:
		if dryRun && filesRefused != nil {
			result.Status, result.Error = models.TaskImportErrored, filesRefused.Error()
			return result
		}
		result.Status = models.TaskImportCreated
		if dryRun {
			return result
		}
		id, err := h.createTaskFromRecord(r, rec, gates)
		if err != nil {
			var refusal *createRefusalError
			if conflict == models.TaskImportConflictSkip && errors.As(err, &refusal) && refusal.status == http.StatusConflict {
				return skipped()
			}
			result.Status, result.Error = models.TaskImportErrored, err.Error()
			return result
		}
		result.ID = &id
		return result
	}
}

// importFileRefusals looks up the uploads each record the import writes names
// (uploadRefusals), before any write, dry runs included: the check only
// reads, so a dry run reports the refusal the import would. A replace is
// checked too, since it creates the task when its row is gone. A skipped
// record is not checked.
func (h *Handlers) importFileRefusals(ctx context.Context, caller taskCreator, records []models.TaskExportRecord, plan importPlan) (map[int]error, error) {
	files := make([][]string, len(records))
	for i, rec := range records {
		if plan.actions[i] != importSkip {
			files[i] = rec.Files
		}
	}
	return h.uploadRefusals(ctx, caller, files)
}

// importContentRefusal checks the two admin-only rules on a record's content
// for every record the import writes, before any write, so a dry run reports
// the same refusal the import would. admitCreate applies both again per
// record. It returns the refusal naming the first offending record, or nil.
//
// run_if runs on the host as the fleet user, so importing one needs admin, as
// authoring one does. A per-task max_cost_usd / max_total_tokens above the
// deployment ceiling needs admin, as on POST /tasks (#1533), with POST /tasks's
// text.
func (h *Handlers) importContentRefusal(isAdmin bool, records []models.TaskExportRecord, plan importPlan) error {
	if isAdmin {
		return nil
	}
	for i, rec := range records {
		if plan.actions[i] == importSkip {
			continue
		}
		if rec.RunIf != nil {
			return fmt.Errorf("task[%d] (name=%s): run_if: a host-side pre-run gate can only be imported by an admin", i, rec.Name)
		}
		tc := models.ExportRecordToTaskCreate(rec)
		if msg := h.requireAdminForCeilingsAboveDeployment(isAdmin, &tc); msg != "" {
			return fmt.Errorf("task[%d] (name=%s): %v", i, rec.Name, msg)
		}
	}
	return nil
}

// createTaskFromRecord mints a fresh task from a portable record (the import
// "create" path) and persists it. The record goes through the SAME gates as
// POST /tasks (Handlers.admitCreate) — import cannot mint a task the create
// path would reject: not one that fails its validation, not one with a ceiling
// above the deployment's unless the importer is an admin, not one more urgent
// than the importing key's priority ceiling, not one whose connectors do not
// resolve for the importer. The creator is the importing principal
// (re-attributed on import — user AND key, via taskCreator.attribute, so a
// key-imported task is visible to that key).
func (h *Handlers) createTaskFromRecord(r *http.Request, rec models.TaskExportRecord, gates *taskGates) (uuid.UUID, error) {
	tc := models.ExportRecordToTaskCreate(rec)
	task, err := h.admitCreate(r.Context(), gates, &tc, nil)
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := h.storage.AddTaskWithContext(r.Context(), task); err != nil {
		// Another request took the name after the import's name lookup: the
		// same 409 refusal POST /tasks gives, and the only 409 this returns.
		if isTaskNameConflict(err) {
			return uuid.Nil, taskNameTaken(task.Name)
		}
		return uuid.Nil, err
	}
	return task.ID, nil
}

// replaceTaskByName updates an existing task's DEFINITION in place (the import
// "replace" path). The record goes through the same edit gates as
// PUT /tasks/{id} (Handlers.admitEdit; a replace keeps the row's owner, so
// the selection must resolve for it), then is handed to
// storage.ReplaceTaskDefinition, which re-locks the row, re-checks it is
// still pending/scheduled, overlays ONLY definition fields (the shared
// models.OverlayTaskDefinition — execution state is never copied from the
// record), and recomputes the dispatch state (#1104). A task that was claimed
// or finished since the pre-flight is therefore refused per-record instead of
// having its status/lease silently rewound into a second execution.
func (h *Handlers) replaceTaskByName(r *http.Request, rec models.TaskExportRecord, gates *taskGates) (uuid.UUID, error) {
	existing, err := h.storage.GetTaskByName(r.Context(), rec.Name)
	if err != nil {
		return uuid.Nil, err
	}
	if existing == nil {
		// Raced: the task was deleted between the pre-flight and now. Treat as a
		// create so the import still lands the record.
		return h.createTaskFromRecord(r, rec, gates)
	}
	tc := models.ExportRecordToTaskCreate(rec)
	if err := h.admitEdit(r.Context(), gates, existing, &tc, taskEditScope{replace: true}); err != nil {
		return uuid.Nil, err
	}
	updated, err := h.storage.ReplaceTaskDefinition(r.Context(), existing.ID, tc)
	if err != nil {
		// Deleted between the name lookup and the row lock: the same race as the
		// nil branch above, with the same answer — land the record as a create.
		if errors.Is(err, sql.ErrNoRows) {
			return h.createTaskFromRecord(r, rec, gates)
		}
		return uuid.Nil, err
	}
	return updated.ID, nil
}

// validateExportRecord enforces the load-bearing, PORTABLE create-time checks
// on an import record: a non-empty prompt, every MCP selection naming a server,
// and a parseable cron recurrence. It deliberately does NOT re-run the
// host/runtime-specific checks (file existence, scheduled-in-the-past) here —
// those are enforced by validateTaskCreate when the record is materialized into
// a TaskCreate, which is the same path POST /tasks uses. Doing the cheap
// structural checks up front keeps a bad payload from producing a partial
// import plan.
func validateExportRecord(rec models.TaskExportRecord) error {
	if strings.TrimSpace(pgtext.Content(rec.Prompt)) == "" { // as it will be stored
		return fmt.Errorf("prompt is required")
	}
	// Structural like the rest of this pass, so a dry run reports a malformed
	// EXECUTION REQUIREMENTS line and a real import refuses it before writing
	// any earlier record (#1601).
	if err := models.ValidateExecutionRequirements(rec.Prompt); err != nil {
		return err
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

// decodeImportEnvelope parses the import body as JSON or YAML. JSON is the
// default (and what the exporter emits for ?format=json); YAML is accepted when
// the Content-Type is application/yaml or the body fails JSON parsing with a
// YAML-looking leading character. Keeping JSON the primary path means the
// common round-trip (export → import) needs no content-type negotiation.
func decodeImportEnvelope(r *http.Request, env *models.TaskExportEnvelope) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "yaml") {
		return yaml.Unmarshal(body, env)
	}
	// Try JSON first; fall back to YAML for a body that isn't valid JSON. This
	// makes `fleet task import --from tasks.yaml | curl -T` work without
	// the caller having to set Content-Type.
	if err := json.Unmarshal(body, env); err == nil {
		return nil
	}
	return yaml.Unmarshal(body, env)
}
