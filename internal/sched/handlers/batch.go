// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

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
// authorized the call (for spend attribution), and whether the caller is an
// admin (admin key or a scoped key carrying PermissionCreateTask). It is the
// extracted form of the auth block CreateTask inlines, lifted here so the batch
// path cannot drift from the single-task auth contract.
type taskCreator struct {
	isAdmin    bool
	creatorID  *uuid.UUID
	creatorKey *string
	// creatorKeyMaxPriority is the authorizing scoped key's task-urgency ceiling
	// (#230), copied by value. nil = admin/user submission or an uncapped key.
	creatorKeyMaxPriority *int
}

// priorityCapError returns a non-nil error when a task's (post-default) priority
// is MORE urgent (lower integer) than the authorizing key's ceiling (#230); nil
// cap = no limit. Shared by the single-task and batch create paths so the two
// cannot drift on how the per-key ceiling is enforced.
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
	kt, hasCreate, ok := h.apiKeys.LookupKeyType(apiKey)
	return ok && kt != apikeys.KeyTypeLegacy && !hasCreate
}

// authorizeTaskCreator is the SAME authorization as CreateTask: an admin API
// key, a scoped key carrying PermissionCreateTask, a user bearer token, or the
// Elcano scoped-tier cookie (which must resolve to a provisioned member). On
// failure it writes the appropriate error response and returns ok=false; the
// caller must stop. It is read-only with respect to authorization state — it
// calls ValidateKey (which DOES consume one rate-limit token, matching
// CreateTask), but creates nothing, so the batch path cannot become a weaker
// route to task creation.
func (h *Handlers) authorizeTaskCreator(w http.ResponseWriter, r *http.Request) (taskCreator, bool) {
	if h.verifyAdminKey(r) {
		return taskCreator{isAdmin: true}, true
	}
	if h.scopedKeyCannotCreate(r) {
		writeError(w, http.StatusForbidden, "insufficient key scope: this key type cannot create tasks")
		return taskCreator{}, false
	}

	var creator taskCreator
	apiKey := r.Header.Get("X-API-Key")
	if apiKey != "" {
		perm := models.PermissionCreateTask
		valid, key, _ := h.apiKeys.ValidateKey(apiKey, &perm, nil, nil, nil)
		if valid && key != nil {
			creator.isAdmin = true
			keyID := key.KeyID
			creator.creatorKey = &keyID
			if key.MaxPriority != nil {
				capVal := *key.MaxPriority
				creator.creatorKeyMaxPriority = &capVal
			}
			return creator, true
		}
	}

	var user *models.User
	if authHeader := r.Header.Get("Authorization"); authHeader != "" {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if u, err := h.storage.GetUserByToken(token); err == nil && u != nil {
			user = u
		}
	}
	if user == nil {
		if sess := h.elcanoSessionFromRequest(r); sess != nil {
			u, err := h.lookupMember(r.Context(), sess.Email)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusInternalServerError, "Membership check failed")
				return taskCreator{}, false
			}
			if u == nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "not_a_member"})
				return taskCreator{}, false
			}
			user = u
		}
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return taskCreator{}, false
	}
	creator.creatorID = &user.ID
	return creator, true
}

// CreateTaskBatch handles POST /tasks/batch (#227): it accepts a slice of
// TaskCreate recipes and creates them in a single API call.
//
// In atomic mode (default false), the whole slice is validated up front; if ANY
// task fails validation NONE are created and the response is 422 with every
// per-task error. In non-atomic mode, valid tasks are inserted while invalid
// ones are skipped, yielding a 207 Multi-Status with per-task results.
//
// Rate-limit accounting: the SchedRateLimitMiddleware already charges one token
// for the HTTP request; the handler additionally charges (len(tasks)-1) tokens
// against the scoped API key's hourly cap so a batch of N tasks costs N tokens
// total — matching the semantic that N tasks, not 1, are being created. The
// admin key and cookie/bearer callers are not hourly-keyed, so there is nothing
// extra to charge there.
func (h *Handlers) CreateTaskBatch(w http.ResponseWriter, r *http.Request) {
	creator, ok := h.authorizeTaskCreator(w, r)
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

	// Spending-cap pre-flight for the scoped key path: refuse a key that has
	// already hit its daily/monthly LLM budget before doing any work, mirroring
	// CreateTask. A batch is N potential runs, so the cap is checked up front
	// (per-task cost is only known at completion; this gates on accumulated spend).
	if creator.creatorKey != nil {
		if err := h.apiKeys.CheckBudget(*creator.creatorKey); err != nil {
			w.Header().Set("Retry-After", "3600")
			writeError(w, http.StatusTooManyRequests, err.Error())
			return
		}
	}

	// Lineage is server-authoritative (#277), identical to the single-task path:
	// clear any forged created_by_task_id on the public create path.
	for i := range req.Tasks {
		req.Tasks[i].CreatedByTaskID = nil
	}

	// Rate-limit accounting: charge the remaining (N-1) tokens so a batch of N
	// costs N total. ValidateKey already charged 1; the admin/cookie paths have
	// no per-key counter to charge.
	if creator.creatorKey != nil {
		if !h.apiKeys.ConsumeN(*creator.creatorKey, len(req.Tasks)-1) {
			writeError(w, http.StatusTooManyRequests, "Rate limit exceeded")
			return
		}
	}

	var (
		toInsert         []*models.Task
		createdList      []models.BatchCreated
		failedList       []models.BatchFailed
		validationFailed bool
	)

	for i := range req.Tasks {
		tc := &req.Tasks[i]
		if err := h.validateTaskCreate(tc); err != nil {
			failedList = append(failedList, models.BatchFailed{Index: i, Error: err.Error()})
			validationFailed = true
			continue
		}
		t := models.NewTask(*tc)
		// Per-key priority ceiling (#230): enforce the SAME cap as the single-task
		// path, treating an over-cap task as a per-task failure (atomic → whole
		// batch aborts; non-atomic → just this one is skipped) so /tasks/batch
		// can't be a weaker route around the key's max_priority.
		if err := priorityCapError(creator.creatorKeyMaxPriority, t.Priority); err != nil {
			failedList = append(failedList, models.BatchFailed{Index: i, Error: err.Error()})
			validationFailed = true
			continue
		}
		t.CreatedBy = creator.creatorID
		t.CreatedByKeyID = creator.creatorKey
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
		writeJSON(w, http.StatusUnprocessableEntity, models.BatchTaskResult{
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
			// Atomic failure: nothing was committed — surface every would-be
			// created task as failed so the caller can retry the whole batch.
			if req.Atomic {
				for _, c := range createdList {
					failedList = append(failedList, models.BatchFailed{
						Index: c.Index, Error: "atomic batch rolled back: " + err.Error(),
					})
				}
				writeJSON(w, http.StatusUnprocessableEntity, models.BatchTaskResult{
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
					Index: c.Index, Error: "failed to create task: " + err.Error(),
				})
			}
			writeJSON(w, http.StatusUnprocessableEntity, models.BatchTaskResult{
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
	writeJSON(w, status, models.BatchTaskResult{
		Created: createdList,
		Failed:  failedList,
		Count:   len(createdList),
	})
}
