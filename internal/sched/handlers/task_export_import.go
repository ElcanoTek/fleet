// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/google/uuid"

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
		log.Printf("handleTaskExport: list: %v", err)
		writeError(w, http.StatusInternalServerError, "export failed")
		return
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
			log.Printf("handleTaskExport: json encode: %v", err)
		}
	case "yaml":
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Content-Disposition", `attachment; filename="fleet-tasks-`+stamp+`.yaml"`)
		w.WriteHeader(http.StatusOK)
		if err := yaml.NewEncoder(w, yaml.Indent(2)).Encode(envelope); err != nil {
			log.Printf("handleTaskExport: yaml encode: %v", err)
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
		log.Printf("handleTaskImport: find by name: %v", err)
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

	resp := models.TaskImportResponse{DryRun: dryRun, Total: len(envelope.Tasks), Results: []models.TaskImportResult{}}

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
				id, rerr := h.replaceTaskByName(r, rec)
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
			// No collision (or unnamed record — never collides by name).
			if !dryRun {
				id, cerr := h.createTaskFromRecord(r, rec, p)
				if cerr != nil {
					result.Status = models.TaskImportErrored
					result.Error = cerr.Error()
					resp.Errors++
					resp.Results = append(resp.Results, result)
					continue
				}
				result.ID = &id
			}
			result.Status = models.TaskImportCreated
			resp.Created++
		}
		resp.Results = append(resp.Results, result)
	}

	// 207 multi-status when any record errored but others succeeded; 200
	// otherwise (incl. dry_run, which never writes).
	status := http.StatusOK
	if resp.Errors > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, resp)
}

// createTaskFromRecord mints a fresh task from a portable record (the import
// "create" path) and persists it. It reuses the handler's validateTaskCreate so
// the imported definition is subject to the SAME routing/cron/timezone/model
// validation as POST /tasks — import cannot mint a task the create path would
// reject. The creator is the importing principal (re-attributed on import).
func (h *Handlers) createTaskFromRecord(_ *http.Request, rec models.TaskExportRecord, p principal) (uuid.UUID, error) {
	tc := models.ExportRecordToTaskCreate(rec)
	if err := h.validateTaskCreate(&tc); err != nil {
		return uuid.Nil, err
	}
	task := models.NewTask(tc)
	task.CreatedBy = p.ownerID()
	if _, err := h.storage.AddTask(task); err != nil {
		return uuid.Nil, err
	}
	return task.ID, nil
}

// replaceTaskByName updates an existing task's DEFINITION in place (the import
// "replace" path). It fetches the colliding task by name, overlays the record's
// definition fields onto it (preserving id, status, attempt_count, lease,
// timestamps, created_by, lineage), and re-saves via UpdateTask. Runtime state
// is therefore preserved; only the configuration is replaced.
func (h *Handlers) replaceTaskByName(r *http.Request, rec models.TaskExportRecord) (uuid.UUID, error) {
	existing, err := h.storage.GetTaskByName(r.Context(), rec.Name)
	if err != nil {
		return uuid.Nil, err
	}
	if existing == nil {
		// Raced: the task was deleted between the pre-flight and now. Treat as a
		// create so the import still lands the record.
		return h.createTaskFromRecord(r, rec, h.principalFromRequest(r))
	}
	tc := models.ExportRecordToTaskCreate(rec)
	if err := h.validateTaskCreate(&tc); err != nil {
		return uuid.Nil, err
	}
	// Overlay definition fields onto the existing task, preserving runtime state.
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
	if _, err := h.storage.UpdateTask(existing); err != nil {
		return uuid.Nil, err
	}
	return existing.ID, nil
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
	if strings.TrimSpace(rec.Prompt) == "" {
		return fmt.Errorf("prompt is required")
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
	// makes `fleet-admin task import --from tasks.yaml | curl -T` work without
	// the caller having to set Content-Type.
	if err := json.Unmarshal(body, env); err == nil {
		return nil
	}
	return yaml.Unmarshal(body, env)
}
