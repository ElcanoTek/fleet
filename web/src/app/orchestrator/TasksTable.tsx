"use client";

import { useEffect, useRef, useState } from "react";
import type { Task } from "@/app/shared/lib/orchestratorApi";
import type { TaskFilters } from "@/app/shared/hooks/useDashboardData";
import { formatDate, truncate } from "@/app/shared/lib/format";

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
};

const STATUS_OPTIONS = [
  "",
  "pending",
  "assigned",
  "running",
  "analyzing",
  "success",
  "error",
  "cancelled",
  "scheduled",
];

function createdByLabel(task: Task): string {
  if (task.created_by_username) return task.created_by_username;
  if (!task.created_by) return "—";
  const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
  if (uuid.test(task.created_by)) return `user: ${task.created_by.slice(0, 6)}…`;
  return task.created_by;
}

function scheduleLabel(task: Task): string {
  if (task.recurrence) return `🔄 ${task.recurrence}`;
  if (task.scheduled_for) return `⏰ ${formatDate(task.scheduled_for)}`;
  return "-";
}

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
          <select
            id="taskStatusFilter"
            className="filter-select"
            aria-label="Filter by status"
            value={filters.status}
            onChange={(e) => onFilters({ status: e.target.value })}
          >
            {STATUS_OPTIONS.map((s) => (
              <option key={s || "all"} value={s}>
                {s ? s[0].toUpperCase() + s.slice(1) : "All"}
              </option>
            ))}
          </select>
        </div>
        <div className="filter-group filter-group-search">
          <label htmlFor="taskSearchFilter" className="filter-label">
            Search
          </label>
          <input
            id="taskSearchFilter"
            type="text"
            className="filter-input"
            placeholder="Search prompt or ID..."
            aria-label="Search tasks"
            value={queryDraft}
            onChange={(e) => setQueryDraft(e.target.value)}
          />
        </div>
        <div className="filter-group filter-group-toggle">
          <label className="filter-checkbox-label">
            <input
              type="checkbox"
              aria-label="Scheduled only"
              checked={filters.scheduledOnly}
              onChange={(e) => onFilters({ scheduledOnly: e.target.checked })}
            />
            <span>Scheduled Only</span>
          </label>
        </div>
        <div className="filter-group">
          <label htmlFor="createdByFilter" className="filter-label">
            Created By
          </label>
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

      <div className="table-wrapper">
        <table id="tasksTable">
          <thead>
            <tr>
              <th scope="col">ID</th>
              <th scope="col">Prompt</th>
              <th scope="col">Status</th>
              <th scope="col">Schedule</th>
              <th scope="col">Created By</th>
              <th scope="col">Created</th>
              <th scope="col">Logs</th>
            </tr>
          </thead>
          <tbody>
            {tasks.length === 0 ? (
              <tr>
                <td colSpan={7} className="table-empty">
                  No tasks created yet
                </td>
              </tr>
            ) : (
              tasks.map((task) => {
                const hasLogs = !!task.agent_session_id;
                return (
                  <tr
                    key={task.id}
                    className="clickable"
                    data-task-id={task.id}
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
                      {truncate((task.prompt ?? "").trim(), 80)}
                    </td>
                    <td>
                      <span className={`status-badge status-${task.status ?? "unknown"}`}>
                        {task.status ?? "-"}
                      </span>
                    </td>
                    <td>{scheduleLabel(task)}</td>
                    <td>{createdByLabel(task)}</td>
                    <td>{formatDate(task.created_at)}</td>
                    <td>
                      <span className={`logs-badge ${hasLogs ? "" : "no-logs"}`}>
                        {hasLogs ? "View" : "None"}
                      </span>
                    </td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </div>

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
