// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// MaxBatchSize is the upper bound on tasks accepted by POST /tasks/batch (#227).
// It mirrors the bulk conversation limit so the two bulk endpoints agree on a
// ceiling. A batch larger than this is rejected with 413 before any validation
// or DB work, so a runaway caller can't tie up the connection or the transaction.
const MaxBatchSize = 100

// taskCreator captures the authorization decision shared by CreateTask and
// CreateTaskBatch: the resolved creator (user) ID, the scoped API key ID that
// authorized the call (for spend attribution), and whether the caller carries
// admin privilege. It is the extracted form of the auth block CreateTask
// inlines, lifted here so the batch path cannot drift from the single-task
// auth contract.
type taskCreator struct {
	// hasAdminPermission reports possession of models.PermissionAdmin: the raw
	// admin API key, a scoped key carrying the admin permission, or a user whose
	// role grants it. This is the ONE definition of "may author a run_if gate"
	// (requireAdminForRunIf); UpdateTask enforces the same boundary via
	// principal.hasPermission(models.PermissionAdmin). A key that merely carries
	// PermissionCreateTask (e.g. a CI task key) does NOT qualify — run_if
	// executes on the host as the fleet user.
	hasAdminPermission bool
	creatorID          *uuid.UUID
	creatorKey         *string
	// creatorUsername is the resolved user's username (an email on a standard
	// deployment) — the budget gate's user-scope principal id (#601 part 2),
	// matching the usage report's group_by=user bucket key. Empty for admin-key
	// and scoped-key submissions, except a key's re-run/clone, whose copy keeps
	// the source's owner (rerunCreator).
	creatorUsername string
	// creatorKeyMaxPriority is the authorizing scoped key's task-urgency ceiling
	// (#230), copied by value. nil = admin/user submission or an uncapped key.
	creatorKeyMaxPriority *int
}

// attribute stamps the creator onto a freshly minted task — BOTH the creating
// user and the authorizing key. It is the one place attribution is written so
// every create path (single, batch, rerun/clone, import, A2A) records the same
// pair: a task with neither CreatedBy nor CreatedByKeyID is readable by nobody
// except fleet-wide grant holders (ADR-0043), and a path that set only the
// user id left every API-key-created task invisible to the key that made it.
func (c taskCreator) attribute(t *models.Task) {
	t.CreatedBy = c.creatorID
	t.CreatedByKeyID = c.creatorKey
}

// creatorFromPrincipal projects a middleware-resolved principal (the admin
// key, a scoped key, or a session user) onto the create pipeline's taskCreator,
// preserving spend attribution, the budget gate's user-scope key and the
// per-key priority ceiling exactly as authorizeTaskCreator would for the same
// credential. Used by the create surfaces that sit inside the
// AdminOrUserAuthMiddleware group (rerun/clone, import, A2A) where the
// principal is already resolved and authorizeTaskCreator's inline auth would
// be redundant.
func creatorFromPrincipal(p principal) taskCreator {
	creator := taskCreator{hasAdminPermission: p.hasPermission(models.PermissionAdmin)}
	switch {
	case p.user != nil:
		creator.creatorID = &p.user.ID
		creator.creatorUsername = p.user.Username
	case p.apiKey != nil:
		keyID := p.apiKey.KeyID
		creator.creatorKey = &keyID
		if p.apiKey.MaxPriority != nil {
			capVal := *p.apiKey.MaxPriority
			creator.creatorKeyMaxPriority = &capVal
		}
	}
	return creator
}

// priorityCapError returns a non-nil error when a task's (post-default) priority
// is MORE urgent (lower integer) than the authorizing key's ceiling (#230); nil
// cap = no limit. Applied by admitCreate and admitEdit (task_gates.go), so every
// create and edit path enforces the per-key ceiling the same way.
func priorityCapError(maxPriority *int, priority int) error {
	if maxPriority != nil && priority < *maxPriority {
		return fmt.Errorf("priority %d exceeds this API key's ceiling (the most urgent it may submit is %d)", priority, *maxPriority)
	}
	return nil
}

// scopedKeyCannotCreate reports whether the request carries a VALID, TYPED API
// key that lacks task-create permission (a readonly or webhook key). Such a key
// is a definitive under-scope, so the task-create paths return 403 rather than
// falling through to the 401 "Unauthorized" path (#190 acceptance criterion:
// 403, not 401, when a key is valid but under-scoped). Legacy (untyped sk-) keys
// are exempt — they return false here and keep their historical behavior. The
// lookup is read-only: it does not consume a rate-limit token.
func (h *Handlers) scopedKeyCannotCreate(r *http.Request) bool {
	apiKey := r.Header.Get("X-API-Key")
	if apiKey == "" {
		return false
	}
	kt, hasCreate, ok := h.apiKeys.LookupKeyTypeOn(apikeys.SurfaceOrchestrator, apiKey)
	return ok && kt != apikeys.KeyTypeLegacy && !hasCreate
}

// authorizeTaskCreator is the SAME authorization as CreateTask: an admin API
// key, a scoped key carrying PermissionCreateTask, or a gateway session's
// principal (which must resolve to a provisioned member). On
// failure it writes the appropriate error response and returns ok=false; the
// caller must stop. It is read-only with respect to authorization state — it
// calls ValidateKey (which DOES consume one rate-limit token, matching
// CreateTask), but creates nothing, so the batch path cannot become a weaker
// route to task creation.
//
// These routes sit outside the auth middleware, so it also does the
// middleware's other job: the returned request's context names the resolved
// principal (logging.WithUser — the username, "apikey/<id>", or "admin" for
// the bootstrap key), so the create pipeline's log lines carry user_id. The
// caller must continue with the returned request.
func (h *Handlers) authorizeTaskCreator(w http.ResponseWriter, r *http.Request) (taskCreator, *http.Request, bool) {
	c, id, ok := h.resolveTaskCreator(w, r)
	if !ok {
		return taskCreator{}, r, false
	}
	return c, withAuthIdentity(r, id), true
}

// withAuthIdentity is what the auth middleware does for a request it
// authenticated, for the routes that authenticate inline: the principal's
// log identity on the context.
func withAuthIdentity(r *http.Request, id string) *http.Request {
	if id == "" {
		return r
	}
	return r.WithContext(logging.WithUser(r.Context(), id))
}

// bootstrapAdminIdentity is the log identity of the bootstrap ADMIN_API_KEY.
const bootstrapAdminIdentity = "admin"

// apiKeyIdentity is an API key's log identity: its id, never the key (#2052).
//
// The separator is "/" and not ":" on purpose: "apikey:<value>" is exactly the
// marker=value shape the log redactor scrubs, so a ":" identity logged as
// user_id=apikey:[REDACTED] and named no key at all.
func apiKeyIdentity(keyID string) string { return "apikey/" + keyID }

func (h *Handlers) resolveTaskCreator(w http.ResponseWriter, r *http.Request) (taskCreator, string, bool) {
	if h.verifyAdminKey(r) {
		return taskCreator{hasAdminPermission: true}, bootstrapAdminIdentity, true
	}
	// The user principal (#2050): POST /tasks (and /tasks/batch,
	// /tasks/estimate) sit outside the AdminOrUserAuthMiddleware group, so the
	// gateway session's principal must be honored here too — otherwise the
	// web UI's cookie sign-in could never create tasks. Fail-closed like the
	// middleware: a refused principal is never retried on weaker paths.
	if user, _, handled := h.userPrincipal(w, r); handled {
		if user == nil {
			return taskCreator{}, "", false
		}
		return taskCreator{
			creatorID:          &user.ID,
			creatorUsername:    user.Username,
			hasAdminPermission: userHasPermission(user, models.PermissionAdmin),
		}, user.Username, true
	}
	if h.scopedKeyCannotCreate(r) {
		writeError(w, http.StatusForbidden, "insufficient key scope: this key type cannot create tasks")
		return taskCreator{}, "", false
	}

	var creator taskCreator
	apiKey := r.Header.Get("X-API-Key")
	if apiKey != "" {
		perm := models.PermissionCreateTask
		valid, key, _ := h.apiKeys.ValidateKeyOn(r.Context(), apikeys.SurfaceOrchestrator, apiKey, &perm, nil, nil)
		if valid && key != nil {
			creator.hasAdminPermission = key.HasPermission(models.PermissionAdmin)
			keyID := key.KeyID
			creator.creatorKey = &keyID
			if key.MaxPriority != nil {
				capVal := *key.MaxPriority
				creator.creatorKeyMaxPriority = &capVal
			}
			return creator, apiKeyIdentity(keyID), true
		}
	}

	// The Operations Center password bearer is retired (#2049 M4, ADR-0105):
	// programs use API keys, people the session cookie (the principal above).
	writeError(w, http.StatusUnauthorized, "Unauthorized")
	return taskCreator{}, "", false
}

// CreateTaskBatch handles POST /tasks/batch (#227): it accepts a slice of
// TaskCreate recipes and creates them in a single API call.
//
// Every task goes through the same create gates as POST /tasks
// (Handlers.admitCreate). In atomic mode (default false), the whole slice is
// checked up front; if ANY task is refused NONE are created and the response
// is 422 with every per-task error. In non-atomic mode, admitted tasks are
// inserted while refused ones are skipped, yielding a 207 Multi-Status with
// per-task results.
//
// Rate-limit accounting: the SchedRateLimitMiddleware already charges one token
// for the HTTP request; the handler additionally charges (len(tasks)-1) tokens
// against the scoped API key's hourly cap so a batch of N tasks costs N tokens
// total — matching the semantic that N tasks, not 1, are being created. The
// admin key and cookie/bearer callers are not hourly-keyed, so there is nothing
// extra to charge there.
func (h *Handlers) CreateTaskBatch(w http.ResponseWriter, r *http.Request) {
	creator, r, ok := h.authorizeTaskCreator(w, r)
	if !ok {
		return
	}

	var req models.BatchTaskCreate
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if len(req.Tasks) == 0 {
		writeError(w, http.StatusBadRequest, "tasks must not be empty")
		return
	}
	if len(req.Tasks) > MaxBatchSize {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("batch size %d exceeds limit of %d", len(req.Tasks), MaxBatchSize))
		return
	}

	// The uploads each task names are looked up once, here, before the budget
	// and the key's counter (uploadRefusals): a failed lookup fails the
	// request before the key is charged. A refused name is that task's
	// failure, which admitCreate reports at its place in the gate order,
	// after validation, as POST /tasks does (taskGates.uploadsChecked).
	files := make([][]string, len(req.Tasks))
	for i := range req.Tasks {
		files[i] = req.Tasks[i].Files
	}
	fileRefusals, err := h.uploadRefusals(r.Context(), creator, files)
	if err != nil {
		writeCallerFilesRefusal(w, err)
		return
	}
	gates := taskGatesFor(creator)
	gates.uploadsChecked = true

	// Per-principal rolling budget (#601 part 2): checked up front for the
	// whole batch — the budget bounds the principal, not any one task, so an
	// exhausted window refuses the request before any row is created.
	if err := h.checkBudget(r.Context(), gates); err != nil {
		writeBudgetRefusal(w, err)
		return
	}

	// Rate-limit accounting: a batch of N costs N tokens on the key's hourly
	// counter. The admin/cookie paths have no per-key counter to charge.
	if !h.chargeRecords(gates, len(req.Tasks)) {
		writeError(w, http.StatusTooManyRequests, "Rate limit exceeded")
		return
	}

	var (
		toInsert         []*models.Task
		createdList      []models.BatchCreated
		failedList       []models.BatchFailed
		validationFailed bool
	)

	// Every record runs the SAME gates as POST /tasks (Handlers.admitCreate):
	// validation, the uploads check, the ceiling and run_if admin rules, the
	// mcp_selection check and the key's priority ceiling. A refusal is a
	// per-task failure carrying POST /tasks's message (atomic → the whole batch
	// aborts; non-atomic → just this one is skipped), so a task /tasks/batch
	// accepts is one POST /tasks would accept from the same caller.
	for i := range req.Tasks {
		gates.uploadRefusal = fileRefusals[i]
		t, err := h.admitCreate(r.Context(), gates, &req.Tasks[i], nil)
		if err != nil {
			failedList = append(failedList, models.BatchFailed{Index: i, Error: itemErrorText(r.Context(), err)})
			validationFailed = true
			continue
		}
		toInsert = append(toInsert, t)
		createdList = append(createdList, models.BatchCreated{ID: t.ID, Index: i})
	}

	if req.Atomic && validationFailed {
		var atomicFailedList []models.BatchFailed
		valErrors := make(map[int]string)
		for _, f := range failedList {
			valErrors[f.Index] = f.Error
		}
		for i := range req.Tasks {
			if errMsg, ok := valErrors[i]; ok {
				atomicFailedList = append(atomicFailedList, models.BatchFailed{Index: i, Error: errMsg})
			} else {
				atomicFailedList = append(atomicFailedList, models.BatchFailed{
					Index: i, Error: "batch aborted: another task failed validation",
				})
			}
		}
		writeJSON(w, r, http.StatusUnprocessableEntity, models.BatchTaskResult{
			Created: []models.BatchCreated{},
			Failed:  atomicFailedList,
			Count:   0,
		})
		return
	}

	// Atomic mode with all-valid tasks: insert under a single transaction so a
	// DB failure rolls every row back. Non-atomic: best-effort multi-row insert.
	if len(toInsert) > 0 {
		if _, err := h.storage.AddTaskBatch(r.Context(), toInsert, req.Atomic); err != nil {
			logging.From(r.Context()).Error("task batch insert failed", "atomic", safe.LogSafe(strconv.FormatBool(req.Atomic)), "err", safe.LogSafe(err.Error()))
			// Atomic failure: nothing was committed — surface every would-be
			// created task as failed so the caller can retry the whole batch.
			if req.Atomic {
				for _, c := range createdList {
					failedList = append(failedList, models.BatchFailed{
						Index: c.Index, Error: "atomic batch rolled back: the tasks could not be saved",
					})
				}
				writeJSON(w, r, http.StatusUnprocessableEntity, models.BatchTaskResult{
					Created: []models.BatchCreated{},
					Failed:  failedList,
					Count:   0,
				})
				return
			}
			// Non-atomic DB failure: the multi-row INSERT is all-or-nothing per
			// statement, so none of the valid tasks landed. Surface them as
			// failed and return 422 (total failure), not 207.
			for _, c := range createdList {
				failedList = append(failedList, models.BatchFailed{
					Index: c.Index, Error: "failed to create task: the tasks could not be saved",
				})
			}
			writeJSON(w, r, http.StatusUnprocessableEntity, models.BatchTaskResult{
				Created: []models.BatchCreated{},
				Failed:  failedList,
				Count:   0,
			})
			return
		}
	}

	// All-valid, all-inserted: 200. Partial (non-atomic only): 207. Total
	// failure (every task invalid, or atomic rollback) was returned above as 422.
	status := http.StatusOK
	if len(failedList) > 0 {
		status = http.StatusMultiStatus
	}
	for _, t := range toInsert {
		localizeTask(t)
	}
	writeJSON(w, r, status, models.BatchTaskResult{
		Created: createdList,
		Failed:  failedList,
		Count:   len(createdList),
	})
}
