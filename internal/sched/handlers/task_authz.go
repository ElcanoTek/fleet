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
//
// Fleet-wide visibility is reading only. Acting on a task (edit, retag,
// re-run, clone, wake, permanent delete, an A2A answer or push config) is
// taskActableByPrincipal: the task's creator, or admin. view_all_logs never
// reaches it.

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

// taskActableByPrincipal reports whether the principal may ACT on the given
// task: change its definition (edit, retag), start a new run from it (re-run,
// clone), feed its parked run (wake, an A2A answer), manage its A2A push
// configs, or permanently delete it. The acting permission (checked by the caller) admits the principal
// to the surface; this decides WHICH task: one it created
// (taskCreatedByPrincipal: the creating user or the creating API key), or any
// task for a principal holding PermissionAdmin (the admin API key, typed admin
// keys, admin-role users).
//
// It is deliberately NOT taskVisibleToPrincipal. A task runs as its owner
// (created_by): the owner's hosted connections, budgets, lineage and per-job
// workspace. A re-run or clone by an API key keeps the source's owner
// (rerunCreator, docs/TASK-OWNERSHIP.md), and an edit or answer changes what the
// owner's next run does, so each of these acts for the owner. Reading a task is
// not acting for its owner: PermissionViewAllLogs (#980) grants reading every
// task's row and transcript, not acting on it, so it never widens this check.
//
// A write surface is still no looser than the read surface guarding the same
// row: every task this admits, taskVisibleToPrincipal admits too.
//
// Note this is NOT principal.ownsTask, which resolves ownership through
// ownerID() and therefore returns false for every API-key principal. Using it
// here would deny a scoped intake-app key the right to edit the task it just
// created. taskCreatedByPrincipal matches a creating user OR a creating key
// (task.CreatedByKeyID), which is the model #980/#1082 established.
func taskActableByPrincipal(p principal, task *models.Task) bool {
	if p.hasPermission(models.PermissionAdmin) {
		return true
	}
	return taskCreatedByPrincipal(p, task)
}

// taskStoppableByPrincipal reports whether the principal may stop (cancel)
// the given task. Two ways in:
//
//   - PermissionCancelTask for a task the principal can see
//     (taskVisibleToPrincipal) — so the operator grant reaches the same rows
//     the principal can read: its own, or every task for the fleet-wide grant
//     (admin, view_all_logs);
//   - having created the task (taskCreatedByPrincipal), as a user or as an API
//     key, so a job author — including a scoped task key — can stop its own
//     run without the operator grant.
//
// Stopping is the one non-read that view_all_logs widens, and only together
// with the separately granted PermissionCancelTask: a stop ends the task and
// runs nothing as its owner. Everything that runs as the owner, feeds the
// owner's run, or permanently deletes the task is taskActableByPrincipal.
//
// This is the REST form of the rule A2A CancelTask already applies
// (a2aCancelTask).
func taskStoppableByPrincipal(p principal, task *models.Task) bool {
	if p.hasPermission(models.PermissionCancelTask) && taskVisibleToPrincipal(p, task) {
		return true
	}
	return taskCreatedByPrincipal(p, task)
}

// taskDeletableByPrincipal reports whether the principal may permanently
// delete the given task (DELETE /tasks/{id}/permanent): the task's creator or
// admin (taskActableByPrincipal). Deleting removes the owner's task row, its
// transcripts and its history for good, so it is the owner's decision, not an
// operator's: cancel_task stops any task the caller can read
// (taskStoppableByPrincipal) but deletes none it did not create.
func taskDeletableByPrincipal(p principal, task *models.Task) bool {
	return taskActableByPrincipal(p, task)
}
