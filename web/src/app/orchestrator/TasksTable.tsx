"use client";

import { useEffect, useRef, useState } from "react";
import type { Task } from "@/app/shared/lib/orchestratorApi";
import type { TaskFilters } from "@/app/shared/hooks/useDashboardData";
import { formatTimeFirst, truncate } from "@/app/shared/lib/format";
import { Icon } from "@/app/shared/ui/Icon";
import { createdByLabel, scheduleLabel, slaBadge, TaskSlaBadge } from "./taskDisplay";

// Statuses whose tasks can be edited: pending/scheduled edit in place;
// terminal ones reopen the form to resubmit with changes. In-flight tasks
// (leased/running) can only be stopped, not edited.
const EDITABLE_STATUSES = new Set([
  "pending",
  "scheduled",
  "success",
  "error",
  "cancelled",
  "dead_lettered",
]);

// Statuses whose tasks can be kicked off on demand ("Run now"): a copy of the
// task runs immediately and the source's schedule is left alone. Deliberately
// the same set as EDITABLE_STATUSES — a task that is already in flight
// (leased/running) is excluded, because a second concurrent
// copy of a running job is a footgun, not a feature. A scheduled/pending task
// IS included: waiting a day for the next cron tick to see whether a new job
// works was the gap this closes.
const RUNNABLE_STATUSES = EDITABLE_STATUSES;

// Statuses whose tasks can be stopped/cancelled from the list. The complement
// of EDITABLE_STATUSES: a task that has already reached a terminal state has
// nothing left to stop, and everything else does — including pending/scheduled,
// which is the case that mattered. Stopping a live run was reachable only from
// inside the Live-activity modal, and a RECURRING job that had not yet fired
// could not be stopped from anywhere in the UI at all, even though the API has
// always supported it (#1152).
const STOPPABLE_STATUSES = new Set([
  "pending",
  "scheduled",
  "leased",
  "running",
  "paused_awaiting_input",
  "paused_awaiting_wake",
]);

// Everything except a live run can be deleted. A leased/running task is refused
// server-side — the worker holds the lease and is still writing to the row — so
// the affordance is hidden rather than offered and then rejected. Stop it first.
const UNDELETABLE_STATUSES = new Set(["leased", "running"]);

// TasksTable — the Recent Tasks table + filter bar + pagination. React port of
// moc dashboard.js renderTasks()/buildTaskQueryString()/pagination controls.
// Clicking a row opens the log viewer (parent's onOpenLogs).

export type TasksTableProps = {
  tasks: Task[];
  total: number;
  page: number;
  pageSize: number;
  filters: TaskFilters;
  onFilters: (next: Partial<TaskFilters>) => void;
  onPage: (page: number) => void;
  onPageSize: (size: number) => void;
  onOpenLogs: (task: Task) => void;
  // First-load and failure state (from useDashboardData). With neither, an
  // empty list rendered "No tasks created yet" during the first fetch AND on a
  // backend outage — an outage looked like an empty account. Both only matter
  // when there are no rows to show; a refresh of a populated list is silent.
  loading?: boolean;
  error?: string | null;
  onRetry?: () => void;
  onEdit?: (task: Task) => void;
  // Kick this task off now. The parent owns the confirm + API call so the
  // table stays presentational (same contract as onEdit/onOpenLogs).
  onRunNow?: (task: Task) => void;
  // Stop/cancel this task. Same presentational contract: the parent owns the
  // confirm and the API call.
  onStop?: (task: Task) => void;
  // Permanently delete this task. Offered on every status EXCEPT a live run,
  // which the server refuses anyway (the worker still holds the lease).
  onDelete?: (task: Task) => void;
};

const STATUS_OPTIONS = [
  "",
  "pending",
  "leased",
  "running",
  "success",
  "error",
  "cancelled",
  "scheduled",
  "paused_awaiting_input",
  "paused_awaiting_wake",
];

export function TasksTable({
  tasks,
  total,
  page,
  pageSize,
  filters,
  onFilters,
  onPage,
  onPageSize,
  onOpenLogs,
  loading = false,
  error = null,
  onRetry,
  onEdit,
  onRunNow,
  onStop,
  onDelete,
}: TasksTableProps) {
  // Debounce ONLY the search box. The status/createdBy selects, the
  // scheduledOnly checkbox, and the stat-card quick filters all call onFilters
  // and must stay instant — so the search input is locally controlled and
  // propagates to onFilters ~300ms after typing settles. queryDraft is the
  // live input value; lastPropagated tracks the value we last pushed up so an
  // EXTERNAL reset (Clear button → filters.query="") re-seeds the box without
  // a mid-typing echo clobbering it.
  const [queryDraft, setQueryDraft] = useState(filters.query);
  const lastPropagated = useRef(filters.query);

  useEffect(() => {
    if (queryDraft === filters.query) return;
    const t = setTimeout(() => {
      lastPropagated.current = queryDraft;
      onFilters({ query: queryDraft });
    }, 300);
    return () => clearTimeout(t);
  }, [queryDraft, filters.query, onFilters]);

  useEffect(() => {
    // Re-seed the draft only when filters.query changed externally (not from our
    // own debounced push), e.g. clearFilters() — never clobber active typing.
    if (filters.query !== lastPropagated.current) {
      lastPropagated.current = filters.query;
      setQueryDraft(filters.query);
    }
  }, [filters.query]);

  const totalPages = Math.max(1, Math.ceil(total / pageSize));
  const start = total > 0 ? Math.min((page - 1) * pageSize + 1, total) : 0;
  const end = Math.min(page * pageSize, total);

  return (
    <div className="section" role="region" aria-labelledby="tasksHeading">
      <div className="section-header">
        <h2 id="tasksHeading">Recent Tasks</h2>
      </div>

      <div className="tasks-filter-bar" role="search" aria-label="Filter tasks">
        <div className="filter-group">
          <label htmlFor="taskStatusFilter" className="filter-label">
            Status
          </label>
          <div className="select-wrap">
            <select
              id="taskStatusFilter"
              className="filter-select"
              aria-label="Filter by status"
              value={filters.status}
              onChange={(e) => onFilters({ status: e.target.value })}
            >
              {STATUS_OPTIONS.map((s) => (
                <option key={s || "all"} value={s}>
                  {s ? s.replaceAll("_", " ") : "All"}
                </option>
              ))}
            </select>
          </div>
        </div>
        <div className="filter-group">
          <label htmlFor="createdByFilter" className="filter-label">
            Created By
          </label>
          <div className="select-wrap">
            <select
              id="createdByFilter"
              className="filter-select"
              aria-label="Filter by creator"
              value={filters.createdBy}
              onChange={(e) => onFilters({ createdBy: e.target.value })}
            >
              <option value="">All</option>
              <option value="me">My Tasks</option>
            </select>
          </div>
        </div>
        <label className="filter-checkbox-label">
          <input
            type="checkbox"
            aria-label="Scheduled only"
            checked={filters.scheduledOnly}
            onChange={(e) => onFilters({ scheduledOnly: e.target.checked })}
          />
          <span>Scheduled Only</span>
        </label>
        <div className="filter-group filter-group-search">
          <label htmlFor="taskSearchFilter" className="filter-label">
            Search
          </label>
          <input
            id="taskSearchFilter"
            type="text"
            className="filter-input"
            placeholder="Search title, prompt, or ID..."
            aria-label="Search tasks"
            value={queryDraft}
            onChange={(e) => setQueryDraft(e.target.value)}
          />
        </div>
      </div>

      <div className="table-wrapper tasks-table-wrapper">
        <table id="tasksTable">
          <thead>
            <tr>
              <th scope="col">ID</th>
              <th scope="col">Task</th>
              <th scope="col">Status</th>
              <th scope="col">SLA</th>
              <th scope="col">Schedule</th>
              <th scope="col">Created By</th>
              <th scope="col">Created</th>
              <th scope="col">Logs</th>
            </tr>
          </thead>
          <tbody>
            {tasks.length === 0 ? (
              <tr>
                <td colSpan={8} className="table-empty">
                  <TasksEmptyState loading={loading} error={error} onRetry={onRetry} />
                </td>
              </tr>
            ) : (
              tasks.map((task) => {
                const hasLogs = !!task.agent_session_id;
                const badge = slaBadge(task);
                return (
                  <tr
                    key={task.id}
                    className={`clickable${badge ? ` sla-row-${badge.tone}` : ""}`}
                    data-task-id={task.id}
                    data-sla-breached={task.sla_breached ? "true" : undefined}
                    role="button"
                    tabIndex={0}
                    aria-label={`View task ${task.id.slice(0, 8)}`}
                    onClick={() => onOpenLogs(task)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter" || e.key === " ") {
                        e.preventDefault();
                        onOpenLogs(task);
                      }
                    }}
                  >
                    <td title={task.id}>
                      <code>{task.id.slice(0, 8)}...</code>
                    </td>
                    <td className="prompt-cell" title={task.prompt ?? ""}>
                      {task.title?.trim() ? (
                        <>
                          <span className="task-title-line">{task.title.trim()}</span>
                          <span className="task-prompt-line">
                            {truncate((task.prompt ?? "").trim(), 80)}
                          </span>
                        </>
                      ) : (
                        truncate((task.prompt ?? "").trim(), 80)
                      )}
                    </td>
                    <td>
                      <span className={`status-badge status-${task.status ?? "unknown"}`}>
                        {task.status ?? "-"}
                      </span>
                    </td>
                    <td>
                      <TaskSlaBadge task={task} />
                    </td>
                    <td title={task.recurrence || undefined}>{scheduleLabel(task)}</td>
                    <td>{createdByLabel(task)}</td>
                    <td>{formatTimeFirst(task.created_at)}</td>
                    <td>
                      <span className="task-row-actions">
                        <span className={`logs-badge ${hasLogs ? "" : "no-logs"}`}>
                          {hasLogs ? "View" : "None"}
                        </span>
                        {onRunNow && RUNNABLE_STATUSES.has(task.status ?? "") ? (
                          <button
                            type="button"
                            className="icon-action task-run-now-btn"
                            aria-label={`Run task ${task.id.slice(0, 8)} now`}
                            title="Run now"
                            data-testid="task-run-now-button"
                            onClick={(e) => {
                              e.stopPropagation();
                              onRunNow(task);
                            }}
                          >
                            <Icon name="zap" className="size-3.5" />
                          </button>
                        ) : null}
                        {onEdit && EDITABLE_STATUSES.has(task.status ?? "") ? (
                          <button
                            type="button"
                            className="icon-action task-edit-btn"
                            aria-label={`Edit task ${task.id.slice(0, 8)}`}
                            title="Edit task"
                            data-testid="task-edit-button"
                            onClick={(e) => {
                              e.stopPropagation();
                              onEdit(task);
                            }}
                          >
                            <Icon name="edit" className="size-3.5" />
                          </button>
                        ) : null}
                        {onStop && STOPPABLE_STATUSES.has(task.status ?? "") ? (
                          <button
                            type="button"
                            className="icon-action task-stop-btn"
                            aria-label={`Stop task ${task.id.slice(0, 8)}`}
                            title={
                              task.recurrence
                                ? "Stop this job — it will not run again"
                                : "Stop this task"
                            }
                            data-testid="task-stop-button"
                            onClick={(e) => {
                              e.stopPropagation();
                              onStop(task);
                            }}
                          >
                            <Icon name="stop" className="size-3.5" />
                          </button>
                        ) : null}
                        {onDelete && !UNDELETABLE_STATUSES.has(task.status ?? "") ? (
                          <button
                            type="button"
                            className="icon-action task-delete-btn"
                            aria-label={`Delete task ${task.id.slice(0, 8)}`}
                            title="Delete permanently — frees its name for reuse"
                            data-testid="task-delete-button"
                            onClick={(e) => {
                              e.stopPropagation();
                              onDelete(task);
                            }}
                          >
                            <Icon name="trash" className="size-3.5" />
                          </button>
                        ) : null}
                      </span>
                    </td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </div>

      {/* Phone card view: the 8-column table needs sideways scrolling on a
          phone, so <=720px renders each task as a stacked card instead. CSS
          toggles .tasks-table-wrapper/.task-cards; both share onOpenLogs.
          Created time + creator are deliberately omitted here — the log
          modal's task summary now carries them, so the card keeps only what
          identifies the task at a glance. */}
      <ul className="task-cards" data-testid="task-cards">
        {tasks.length === 0 ? (
          <li className="table-empty">
            <TasksEmptyState loading={loading} error={error} onRetry={onRetry} />
          </li>
        ) : (
          tasks.map((task) => {
            const hasLogs = !!task.agent_session_id;
            const badge = slaBadge(task);
            return (
              <li key={task.id}>
                <button
                  type="button"
                  className="task-card"
                  data-task-id={task.id}
                  data-sla-breached={task.sla_breached ? "true" : undefined}
                  aria-label={`View task ${task.id.slice(0, 8)}`}
                  onClick={() => onOpenLogs(task)}
                >
                  <span className="task-card-top">
                    <span className={`status-badge status-${task.status ?? "unknown"}`}>
                      {task.status ?? "-"}
                    </span>
                    {badge ? (
                      <span className={`sla-badge sla-badge-${badge.tone}`}>{badge.label}</span>
                    ) : task.expected_duration_minutes ? (
                      <span className="sla-badge sla-badge-ok">
                        {task.actual_duration_seconds != null
                          ? `${Math.round(task.actual_duration_seconds / 60)}m / ${task.expected_duration_minutes}m`
                          : `${task.expected_duration_minutes}m`}
                      </span>
                    ) : null}
                  </span>
                  {task.title?.trim() ? (
                    <span className="task-card-title">{task.title.trim()}</span>
                  ) : null}
                  <span className="task-card-prompt">{truncate((task.prompt ?? "").trim(), 120)}</span>
                  <span className="task-card-meta">
                    <code>{task.id.slice(0, 8)}</code>
                    {scheduleLabel(task) !== "-" ? (
                      <span title={task.recurrence || undefined}>{scheduleLabel(task)}</span>
                    ) : null}
                    <span className={`logs-badge ${hasLogs ? "" : "no-logs"}`}>
                      {hasLogs ? "View logs" : "No logs"}
                    </span>
                    {onRunNow && RUNNABLE_STATUSES.has(task.status ?? "") ? (
                      <span
                        role="button"
                        tabIndex={0}
                        className="icon-action task-run-now-btn"
                        aria-label={`Run task ${task.id.slice(0, 8)} now`}
                        title="Run now"
                        data-testid="task-run-now-button-card"
                        onClick={(e) => {
                          e.stopPropagation();
                          onRunNow(task);
                        }}
                        onKeyDown={(e) => {
                          if (e.key === "Enter" || e.key === " ") {
                            e.preventDefault();
                            e.stopPropagation();
                            onRunNow(task);
                          }
                        }}
                      >
                        <Icon name="zap" className="size-3.5" />
                      </span>
                    ) : null}
                    {onEdit && EDITABLE_STATUSES.has(task.status ?? "") ? (
                      <span
                        role="button"
                        tabIndex={0}
                        className="icon-action task-edit-btn"
                        aria-label={`Edit task ${task.id.slice(0, 8)}`}
                        title="Edit task"
                        data-testid="task-edit-button-card"
                        onClick={(e) => {
                          e.stopPropagation();
                          onEdit(task);
                        }}
                        onKeyDown={(e) => {
                          if (e.key === "Enter" || e.key === " ") {
                            e.preventDefault();
                            e.stopPropagation();
                            onEdit(task);
                          }
                        }}
                      >
                        <Icon name="edit" className="size-3.5" />
                      </span>
                    ) : null}
                    {onStop && STOPPABLE_STATUSES.has(task.status ?? "") ? (
                      <span
                        role="button"
                        tabIndex={0}
                        className="icon-action task-stop-btn"
                        aria-label={`Stop task ${task.id.slice(0, 8)}`}
                        title={
                          task.recurrence ? "Stop this job — it will not run again" : "Stop this task"
                        }
                        data-testid="task-stop-button-card"
                        onClick={(e) => {
                          e.stopPropagation();
                          onStop(task);
                        }}
                        onKeyDown={(e) => {
                          if (e.key === "Enter" || e.key === " ") {
                            e.preventDefault();
                            e.stopPropagation();
                            onStop(task);
                          }
                        }}
                      >
                        <Icon name="stop" className="size-3.5" />
                      </span>
                    ) : null}
                    {onDelete && !UNDELETABLE_STATUSES.has(task.status ?? "") ? (
                      <span
                        role="button"
                        tabIndex={0}
                        className="icon-action task-delete-btn"
                        aria-label={`Delete task ${task.id.slice(0, 8)}`}
                        title="Delete permanently — frees its name for reuse"
                        data-testid="task-delete-button-card"
                        onClick={(e) => {
                          e.stopPropagation();
                          onDelete(task);
                        }}
                        onKeyDown={(e) => {
                          if (e.key === "Enter" || e.key === " ") {
                            e.preventDefault();
                            e.stopPropagation();
                            onDelete(task);
                          }
                        }}
                      >
                        <Icon name="trash" className="size-3.5" />
                      </span>
                    ) : null}
                  </span>
                </button>
              </li>
            );
          })
        )}
      </ul>

      <div className="tasks-pagination" role="navigation" aria-label="Tasks pagination">
        <div className="pagination-info">
          <span>
            {total > 0 ? `Showing ${start}-${end} of ${total} tasks` : "Showing 0 of 0 tasks"}
          </span>
        </div>
        <div className="pagination-controls">
          <button
            type="button"
            className="btn btn-secondary"
            aria-label="Previous page"
            disabled={page <= 1}
            onClick={() => onPage(page - 1)}
          >
            Prev
          </button>
          <span className="page-info">
            Page {page} of {totalPages}
          </span>
          <button
            type="button"
            className="btn btn-secondary"
            aria-label="Next page"
            disabled={page >= totalPages}
            onClick={() => onPage(page + 1)}
          >
            Next
          </button>
        </div>
        <div className="page-size-selector">
          <label htmlFor="pageSizeSelect">Show</label>
          <select
            id="pageSizeSelect"
            aria-label="Items per page"
            value={pageSize}
            onChange={(e) => onPageSize(Number.parseInt(e.target.value, 10))}
          >
            <option value="10">10</option>
            <option value="20">20</option>
            <option value="50">50</option>
          </select>
        </div>
      </div>
    </div>
  );
}

export default TasksTable;

// TasksEmptyState is the zero-row body of both the table and the phone cards:
// a load in flight, a failed load (with Retry when the parent offers one), or
// the genuine empty account — in that order, so an outage never reads as
// "nothing here".
function TasksEmptyState({
  loading,
  error,
  onRetry,
}: {
  loading: boolean;
  error: string | null;
  onRetry?: () => void;
}) {
  if (error) {
    return (
      <span role="alert" data-testid="tasks-load-error">
        Couldn&apos;t load tasks: {error}
        {onRetry ? (
          <>
            {" "}
            <button type="button" className="btn btn-small" onClick={onRetry}>
              Retry
            </button>
          </>
        ) : null}
      </span>
    );
  }
  if (loading) {
    return <span data-testid="tasks-loading">Loading tasks…</span>;
  }
  return <>No tasks created yet</>;
}
