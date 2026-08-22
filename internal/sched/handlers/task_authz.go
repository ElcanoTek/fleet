// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// Per-task row visibility (#1082).
//
// Typed task keys are sold as scoped — a fleet_task_* key is minted for one
// automation — but after node-routing removal every task-read surface
// authorized on PermissionViewTasks alone, so any such key (and any non-admin
// user) could enumerate and read EVERY task on the box: list, get, structured
// output, error analysis, the export bundle, and the upcoming-runs projection
// (which carries prompts). Run logs had already drawn the per-task boundary
// (#980, log_authz.go); this extends the same ownership model to the task rows
// themselves, so the metadata gate is no looser than the transcript gate.
//
// The model mirrors #980 exactly: a principal sees its own tasks
// (taskCreatedByPrincipal — the creating user or the creating API key), and
// seeing everyone else's requires a fleet-wide grant. Fleet-wide is
// PermissionViewAllLogs — which hasPermission also reports true for the
// bootstrap admin key and any principal carrying PermissionAdmin (typed admin
// keys, admin-role users). The designated transcript auditor (#980) is
// included on purpose: an auditor who may read every transcript must be able
// to find the tasks those transcripts belong to.

import "github.com/ElcanoTek/fleet/internal/sched/models"

// fleetWideTaskVisibility reports whether the principal may see every task's
// row regardless of creator: the bootstrap admin key, PermissionAdmin
// carriers, and the explicit PermissionViewAllLogs auditor grant.
func (p principal) fleetWideTaskVisibility() bool {
	return p.hasPermission(models.PermissionViewAllLogs)
}

// taskVisibleToPrincipal reports whether the principal may read the given
// task's row. PermissionViewTasks (checked by the caller) admits it to the
// surface; this decides WHICH tasks — own rows unless the fleet-wide grant
// applies. Same shape as taskLogsReadable, deliberately.
func taskVisibleToPrincipal(p principal, task *models.Task) bool {
	if p.fleetWideTaskVisibility() {
		return true
	}
	return taskCreatedByPrincipal(p, task)
}

// visibleTasks filters an already-loaded slice down to the tasks the principal
// may see, for the read surfaces that load broad sets outside GetTasksFiltered
// (task export, upcoming-runs projection). The paginated list path filters in
// SQL instead (db.TaskFilter.VisibleToUserID / VisibleToKeyID) so pagination
// counts stay correct.
func visibleTasks(p principal, tasks []*models.Task) []*models.Task {
	if p.fleetWideTaskVisibility() {
		return tasks
	}
	out := make([]*models.Task, 0, len(tasks))
	for _, t := range tasks {
		if taskCreatedByPrincipal(p, t) {
			out = append(out, t)
		}
	}
	return out
}

// taskWritableByPrincipal reports whether the principal may MUTATE the given
// task's definition (edit, retag). The mutating permission (PermissionCreateTask,
// checked by the caller) admits it to the surface; this decides WHICH task.
//
// Same own-rows model as taskVisibleToPrincipal, and deliberately the same
// helper pair: a write surface must be no looser than the read surface guarding
// the same row. Before this existed, PUT /tasks/{id} and POST /tasks/{id}/tags
// authorized on PermissionCreateTask alone, so any client-role user or scoped
// task key could rewrite ANY task on the box — prompt, model, mcp_selection and
// credential_allowlist included — while the read path had already been narrowed
// to own rows by #1082. That asymmetry was the hole.
//
// Note this is NOT principal.ownsTask, which resolves ownership through
// ownerID() and therefore returns false for every API-key principal. Using it
// here would deny a scoped intake-app key the right to edit the task it just
// created. taskCreatedByPrincipal matches a creating user OR a creating key
// (task.CreatedByKeyID), which is the model #980/#1082 established.
func taskWritableByPrincipal(p principal, task *models.Task) bool {
	if p.fleetWideTaskVisibility() {
		return true
	}
	return taskCreatedByPrincipal(p, task)
}
