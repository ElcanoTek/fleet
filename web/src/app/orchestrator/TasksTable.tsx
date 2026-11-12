import { rich } from "@/app/lib/i18n/rich";
import { t as tr } from "@/app/lib/i18n";
import { useEffect, useRef, useState } from "react";
import type { Task } from "@/app/shared/lib/orchestratorApi";
import type { TaskFilters } from "@/app/shared/hooks/useDashboardData";
import { formatTimeFirst, truncate } from "@/app/shared/lib/format";
import { Icon } from "@/app/shared/ui/Icon";
import { StatusChip } from "@/app/shared/ui/StatusChip";
import { labelChipStyle } from "@/app/shared/lib/labelColors";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  createdByLabel,
  scheduleLabel,
  scheduleTitle,
  slaBadge,
  promptName,
  taskRunLabel,
  TaskSlaBadge,
  SlaChip,
  TaskStatusBadge,
  isDeletable,
  isRunnable,
  isStoppable,
} from "./taskDisplay";

// taskControlLabel is the accessible name for a per-row task control. The
// human-readable part comes from taskRunLabel — the shared title →
// prompt-first-line helper, which is the web's documented display precedence
// (the server-side `name` is deliberately never rendered as a display label:
// it is empty on every recurrence and clone, orchestratorApi.ts). The short
// id is appended because titles are non-unique by design and the id is what
// disambiguates two identically-titled jobs — knowing WHICH task a Stop or
// Delete will hit is the whole point of the name.
function taskControlLabel(task: Task, action: string): string {
  const idPart = task.id.slice(0, 8);
  const label = taskRunLabel(task, 60);
  // taskRunLabel's last resort is the short id itself; don't print it twice.
  return label === idPart
    ? tr("{0} task {1}", [action, idPart])
    : tr("{0} task {1} ({2})", [action, label, idPart]);
}

// Stopping a recurring task ends its schedule for good, so its control names
// the schedule rather than reading like a pause.
function stopVerb(task: Task): string {
  return task.recurrence ? tr("Stop the schedule for") : tr("Stop");
}

function stopTitle(task: Task): string {
  return task.recurrence
    ? tr("Stop the schedule for good. It won't run again, and a run in progress stops too.")
    : tr("Stop this task");
}

// TasksTable — the Recent Tasks table + filter bar + pagination. React port of
// moc dashboard.js renderTasks()/buildTaskQueryString()/pagination controls.
// Clicking a row opens the log viewer (parent's onOpenLogs).

export type TasksTableProps = {
  tasks: Task[];
  total: number;
  page: number;
  pageSize: number;
  filters: TaskFilters;
  // Tags the filter offers (useDashboardData.tagOptions). Empty is a valid
  // state — a deployment where nobody tags anything — and hides the control
  // rather than showing an empty dropdown.
  tagOptions?: string[];
  onFilters: (next: Partial<TaskFilters>) => void;
  // Reset every filter at once (useDashboardData.clearFilters). The button
  // only renders while some filter is set, and only when the parent wires it.
  onClearFilters?: () => void;
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
  tagOptions = [],
  onFilters,
  onClearFilters,
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
  // Debounce ONLY the search box. The status/createdBy/tag selects and the
  // stat-card quick filters all call onFilters
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

  const anyFilter =
    filters.status !== "" ||
    filters.query !== "" ||
    filters.completedToday ||
    filters.createdBy !== "" ||
    filters.tags.length > 0;

  // Tags AND together server-side, so adding one always narrows and removing
  // one always widens. Toggling is the whole interaction: the select adds,
  // the chips (in the bar and on the rows) remove or add.
  const toggleTag = (tag: string) => {
    onFilters({
      tags: filters.tags.includes(tag)
        ? filters.tags.filter((t) => t !== tag)
        : [...filters.tags, tag],
    });
  };
  const unselectedTags = tagOptions.filter((t) => !filters.tags.includes(t));

  const totalPages = Math.max(1, Math.ceil(total / pageSize));
  const start = total > 0 ? Math.min((page - 1) * pageSize + 1, total) : 0;
  const end = Math.min(page * pageSize, total);

  return (
    <div className="section" role="region" aria-labelledby="tasksHeading">
      <div className="section-header">
        <h2 id="tasksHeading">{tr("Recent Tasks")}</h2>
      </div>

      <div className="tasks-filter-bar" role="search" aria-label={tr("Filter tasks")}>
        <div className="filter-group">
          <label htmlFor="taskStatusFilter" className="filter-label">
            {tr("Status")}
          </label>
          <NativeSelect
            id="taskStatusFilter"
            className="filter-select"
            aria-label={tr("Filter by status")}
            value={filters.status}
            onChange={(e) => onFilters({ status: e.target.value })}
          >
            {STATUS_OPTIONS.map((s) => (
              <NativeSelectOption key={s || "all"} value={s}>
                {s ? s.replaceAll("_", " ") : tr("All")}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
        <div className="filter-group">
          <label htmlFor="createdByFilter" className="filter-label">
            {tr("Created By")}
          </label>
          <NativeSelect
            id="createdByFilter"
            className="filter-select"
            aria-label={tr("Filter by creator")}
            value={filters.createdBy}
            onChange={(e) => onFilters({ createdBy: e.target.value })}
          >
            <NativeSelectOption value="">{tr("All")}</NativeSelectOption>
            <NativeSelectOption value="me">{tr("My Tasks")}</NativeSelectOption>
          </NativeSelect>
        </div>
        {tagOptions.length > 0 || filters.tags.length > 0 ? (
          <div className="filter-group">
            <label htmlFor="taskTagFilter" className="filter-label">
              {tr("Tags")}
            </label>
            <div className="tag-filter-control">
              <NativeSelect
                id="taskTagFilter"
                className="filter-select"
                aria-label={tr("Filter by tag")}
                // Always "" — this select is an ADD control, not a
                // selection. Holding the last-added tag would claim the
                // board is filtered by that one tag when it is filtered by
                // every chip beside it.
                value=""
                onChange={(e) => {
                  if (e.target.value) toggleTag(e.target.value);
                }}
              >
                <NativeSelectOption value="">{filters.tags.length > 0 ? tr("Add tag…") : tr("All")}</NativeSelectOption>
                {unselectedTags.map((tag) => (
                  <NativeSelectOption key={tag} value={tag}>
                    {tag}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
              {filters.tags.length > 0 ? (
                <span className="tag-filter-chips" data-testid="tag-filter-chips">
                  {filters.tags.map((tag) => (
                    <button
                      key={tag}
                      type="button"
                      className="task-tag-chip task-tag-chip-active"
                      style={labelChipStyle(tag)}
                      aria-label={tr("Stop filtering by tag {0}", [tag])}
                      onClick={() => toggleTag(tag)}
                    >
                      {tag}
                      <Icon name="close" className="size-3" />
                    </button>
                  ))}
                </span>
              ) : null}
            </div>
          </div>
        ) : null}
        <div className="filter-group filter-group-search">
          <label htmlFor="taskSearchFilter" className="filter-label">
            {tr("Search")}
          </label>
          <Input
            id="taskSearchFilter"
            type="text"
            className="filter-input"
            placeholder={tr("Search title, prompt, tag, creator or ID…")}
            aria-label={tr("Search tasks")}
            value={queryDraft}
            onChange={(e) => setQueryDraft(e.target.value)}
          />
        </div>
        {onClearFilters && anyFilter ? (
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="btn btn-secondary btn-small"
            data-testid="tasks-clear-filters"
            onClick={() => {
              // Drop the search draft too: text typed inside the debounce
              // window has not reached filters.query yet, and the re-seed
              // effect below only fires when THAT changes.
              lastPropagated.current = "";
              setQueryDraft("");
              onClearFilters();
            }}
          >
            {tr("Clear filters")}
          </Button>
        ) : null}
      </div>

      <Table id="tasksTable" containerClassName="table-wrapper tasks-table-wrapper">
        <TableHeader>
          <TableRow>
            <TableHead>{tr("ID")}</TableHead>
            <TableHead>{tr("Task")}</TableHead>
            <TableHead>{tr("Status")}</TableHead>
            <TableHead>{tr("SLA")}</TableHead>
            <TableHead>{tr("Schedule")}</TableHead>
            <TableHead>{tr("Created By")}</TableHead>
            <TableHead>{tr("Created")}</TableHead>
            <TableHead>{tr("Logs")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {tasks.length === 0 ? (
            <TableRow>
              <TableCell colSpan={8} className="table-empty whitespace-normal">
                <TasksEmptyState
                  loading={loading}
                  error={error}
                  onRetry={onRetry}
                  anyFilter={anyFilter}
                  onClearFilters={onClearFilters}
                />
              </TableCell>
            </TableRow>
          ) : (
            tasks.map((task) => {
              const hasLogs = !!task.agent_session_id;
              const badge = slaBadge(task);
              return (
                <TableRow
                  key={task.id}
                  interactive
                  className={`clickable${badge ? ` sla-row-${badge.tone}` : ""}`}
                  data-task-id={task.id}
                  data-sla-breached={task.sla_breached ? "true" : undefined}
                  role="button"
                  tabIndex={0}
                  aria-label={taskControlLabel(task, tr("View"))}
                  onClick={() => onOpenLogs(task)}
                  onKeyDown={(e) => {
                    // Only open logs when the row itself is focused, not when
                    // an event bubbles from a nested control (such as a tag
                    // chip or action button). Otherwise pressing Enter/Space
                    // on a focused chip opens the log viewer and preventDefault
                    // blocks the chip's native activation click.
                    if (e.target !== e.currentTarget) return;
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      onOpenLogs(task);
                    }
                  }}
                >
                  <TableCell title={task.id}>
                    <code>{task.id.slice(0, 8)}...</code>
                  </TableCell>
                  <TableCell className="prompt-cell whitespace-normal" title={task.prompt ?? ""}>
                    {task.title?.trim() || promptName(task.prompt ?? "") ? (
                      <>
                        <span className="task-title-line">
                          {task.title?.trim() || promptName(task.prompt ?? "")}
                        </span>
                        <span className="task-prompt-line">
                          {truncate((task.prompt ?? "").trim(), 80)}
                        </span>
                      </>
                    ) : (
                      truncate((task.prompt ?? "").trim(), 80)
                    )}
                    <TaskTagChips
                      tags={task.tags}
                      selected={filters.tags}
                      onToggle={toggleTag}
                    />
                  </TableCell>
                  <TableCell>
                    <TaskStatusBadge task={task} />
                  </TableCell>
                  <TableCell>
                    <TaskSlaBadge task={task} />
                  </TableCell>
                  <TableCell title={scheduleTitle(task)}>{scheduleLabel(task)}</TableCell>
                  <TableCell>{createdByLabel(task)}</TableCell>
                  <TableCell>{formatTimeFirst(task.created_at)}</TableCell>
                  <TableCell>
                    <span className="task-row-actions">
                      <StatusChip tone={hasLogs ? "run-assigned" : "neutral"} className={`logs-badge${hasLogs ? "" : " no-logs"}`}>
                        {hasLogs ? tr("View") : tr("None")}
                      </StatusChip>
                      {onRunNow && isRunnable(task.status) ? (
                        <button
                          type="button"
                          className="icon-action task-run-now-btn"
                          aria-label={tr("{0} now", [taskControlLabel(task, tr("Run"))])}
                          title={tr("Run now")}
                          data-testid="task-run-now-button"
                          onClick={(e) => {
                            e.stopPropagation();
                            onRunNow(task);
                          }}
                        >
                          <Icon name="zap" className="size-3.5" />
                        </button>
                      ) : null}
                      {onEdit && isRunnable(task.status) ? (
                        <button
                          type="button"
                          className="icon-action task-edit-btn"
                          aria-label={taskControlLabel(task, tr("Edit"))}
                          title={tr("Edit task")}
                          data-testid="task-edit-button"
                          onClick={(e) => {
                            e.stopPropagation();
                            onEdit(task);
                          }}
                        >
                          <Icon name="edit" className="size-3.5" />
                        </button>
                      ) : null}
                      {onStop && isStoppable(task.status) ? (
                        <button
                          type="button"
                          className="icon-action task-stop-btn"
                          aria-label={taskControlLabel(task, stopVerb(task))}
                          title={stopTitle(task)}
                          data-testid="task-stop-button"
                          onClick={(e) => {
                            e.stopPropagation();
                            onStop(task);
                          }}
                        >
                          <Icon name="stop" className="size-3.5" />
                        </button>
                      ) : null}
                      {onDelete && isDeletable(task.status) ? (
                        <button
                          type="button"
                          className="icon-action task-delete-btn"
                          aria-label={taskControlLabel(task, tr("Delete"))}
                          title={tr("Delete permanently — frees its name for reuse")}
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
                  </TableCell>
                </TableRow>
              );
            })
          )}
        </TableBody>
      </Table>

      {/* Phone card view: the 8-column table needs sideways scrolling on a
          phone, so <=720px renders each task as a stacked card instead. CSS
          toggles .tasks-table-wrapper/.task-cards; both share onOpenLogs.
          Created time + creator are deliberately omitted here — the log
          modal's task summary now carries them, so the card keeps only what
          identifies the task at a glance. */}
      <ul className="task-cards" data-testid="task-cards">
        {tasks.length === 0 ? (
          <li className="table-empty">
            <TasksEmptyState
              loading={loading}
              error={error}
              onRetry={onRetry}
              anyFilter={anyFilter}
              onClearFilters={onClearFilters}
            />
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
                  aria-label={taskControlLabel(task, tr("View"))}
                  onClick={() => onOpenLogs(task)}
                >
                  <span className="task-card-top">
                    <TaskStatusBadge task={task} />
                    {badge ? (
                      <SlaChip tone={badge.tone}>{badge.label}</SlaChip>
                    ) : task.expected_duration_minutes ? (
                      <SlaChip tone="ok">
                        {task.actual_duration_seconds != null
                          ? tr("{0}m / {1}m", [Math.round(task.actual_duration_seconds / 60), task.expected_duration_minutes])
                          : `${task.expected_duration_minutes}m`}
                      </SlaChip>
                    ) : null}
                  </span>
                  {task.title?.trim() || promptName(task.prompt ?? "") ? (
                    <span className="task-card-title">
                      {task.title?.trim() || promptName(task.prompt ?? "")}
                    </span>
                  ) : null}
                  <span className="task-card-prompt">{truncate((task.prompt ?? "").trim(), 120)}</span>
                  <span className="task-card-meta">
                    <code>{task.id.slice(0, 8)}</code>
                    {scheduleLabel(task) !== "-" ? (
                      <span title={scheduleTitle(task)}>{scheduleLabel(task)}</span>
                    ) : null}
                    <StatusChip tone={hasLogs ? "run-assigned" : "neutral"} className={`logs-badge${hasLogs ? "" : " no-logs"}`}>
                      {hasLogs ? tr("View logs") : tr("No logs")}
                    </StatusChip>
                    {onRunNow && isRunnable(task.status) ? (
                      <span
                        role="button"
                        tabIndex={0}
                        className="icon-action task-run-now-btn"
                        aria-label={tr("{0} now", [taskControlLabel(task, tr("Run"))])}
                        title={tr("Run now")}
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
                    {onEdit && isRunnable(task.status) ? (
                      <span
                        role="button"
                        tabIndex={0}
                        className="icon-action task-edit-btn"
                        aria-label={taskControlLabel(task, tr("Edit"))}
                        title={tr("Edit task")}
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
                    {onStop && isStoppable(task.status) ? (
                      <span
                        role="button"
                        tabIndex={0}
                        className="icon-action task-stop-btn"
                        aria-label={taskControlLabel(task, stopVerb(task))}
                        title={stopTitle(task)}
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
                    {onDelete && isDeletable(task.status) ? (
                      <span
                        role="button"
                        tabIndex={0}
                        className="icon-action task-delete-btn"
                        aria-label={taskControlLabel(task, tr("Delete"))}
                        title={tr("Delete permanently — frees its name for reuse")}
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
                {/* Outside the card button, not inside it: the card is itself a
                    <button>, and a control nested in one has invalid semantics
                    however it is marked up — assistive technology can expose
                    only the outer "View task" button, or make the tag action
                    ambiguous. As a sibling each chip is a real button with its
                    own name. The card's border/background moved to this <li>
                    so the chips still sit inside the visible card. */}
                <TaskTagChips tags={task.tags} selected={filters.tags} onToggle={toggleTag} />
              </li>
            );
          })
        )}
      </ul>

      <div className="tasks-pagination" role="navigation" aria-label={tr("Tasks pagination")}>
        <div className="pagination-info">
          <span>
            {total > 0 ? tr("Showing {0}-{1} of {2} tasks", [start, end, total]) : tr("Showing 0 of 0 tasks")}
          </span>
        </div>
        <div className="pagination-controls">
          <Button
            type="button"
            variant="outline"
            className="btn btn-secondary"
            aria-label={tr("Previous page")}
            disabled={page <= 1}
            onClick={() => onPage(page - 1)}
          >
            {tr("Prev")}
          </Button>
          <span className="page-info">
        {rich("Page {0} of {1}", page, totalPages)}
      </span>
          <Button
            type="button"
            variant="outline"
            className="btn btn-secondary"
            aria-label={tr("Next page")}
            disabled={page >= totalPages}
            onClick={() => onPage(page + 1)}
          >
            {tr("Next")}
          </Button>
        </div>
        <div className="page-size-selector">
          <label htmlFor="pageSizeSelect">{tr("Show")}</label>
          <NativeSelect
            id="pageSizeSelect"
            aria-label={tr("Items per page")}
            value={pageSize}
            onChange={(e) => onPageSize(Number.parseInt(e.target.value, 10))}
          >
            <NativeSelectOption value="10">10</NativeSelectOption>
            <NativeSelectOption value="20">20</NativeSelectOption>
            <NativeSelectOption value="50">50</NativeSelectOption>
          </NativeSelect>
        </div>
      </div>
    </div>
  );
}

export default TasksTable;

// TaskTagChips renders a task's tags (#212) as chips that filter the board.
//
// Tags were write-only before this: the create form accepted them, the API
// stored them, and no surface ever showed one again — so the one thing a tag
// is for, finding the rest of its group, could not be done. Clicking a chip
// toggles that tag into the board's filter, which ANDs them server-side.
//
// Every chip is a real <button>. The phone card renders these OUTSIDE its own
// card button rather than within it: a control nested inside a button has
// invalid accessibility semantics however it is marked up, and dressing a span
// as role="button" to dodge the HTML rule leaves the same problem — assistive
// technology can expose only the outer control. Siblings, not children.
function TaskTagChips({
  tags,
  selected,
  onToggle,
}: {
  tags?: string[];
  selected: string[];
  onToggle: (tag: string) => void;
}) {
  if (!tags || tags.length === 0) return null;
  return (
    <span className="task-tag-row">
      {tags.map((tag) => {
        const active = selected.includes(tag);
        const label = active ? tr("Stop filtering by tag {0}", [tag]) : tr("Filter by tag {0}", [tag]);
        const className = `task-tag-chip${active ? " task-tag-chip-active" : ""}`;
        // The row (and the card) opens the log viewer, so every chip has to
        // stop the event before it gets there.
        const toggle = (e: React.SyntheticEvent) => {
          e.stopPropagation();
          onToggle(tag);
        };
        return (
          <button
            key={tag}
            type="button"
            className={className}
            style={labelChipStyle(tag)}
            aria-pressed={active}
            aria-label={label}
            onClick={toggle}
          >
            {tag}
          </button>
        );
      })}
    </span>
  );
}

// TasksEmptyState is the zero-row body of both the table and the phone cards.
// Four cases, in this order:
//  1. a failed load (with Retry when the parent offers one),
//  2. a load in flight,
//  3. filters set with nothing matching,
//  4. the genuine empty account.
// The ordering is the whole point: an outage must never read as "nothing
// here" (1 and 2 precede everything), and a filter that matched nothing must
// never read as "the account has no tasks" — case 3 precedes case 4 because
// the moment an operator deletes or narrows down to zero rows is the exact
// moment "No tasks created yet" is a false and alarming statement.
function TasksEmptyState({
  loading,
  error,
  onRetry,
  anyFilter,
  onClearFilters,
}: {
  loading: boolean;
  error: string | null;
  onRetry?: () => void;
  // A filter is set but the loaded list is empty — say "no match", not
  // "no tasks" (see the comment above), and offer the same Clear filters
  // affordance the parent wires rather than inventing a second one.
  anyFilter: boolean;
  onClearFilters?: () => void;
}) {
  if (error) {
    return (
      <span role="alert" data-testid="tasks-load-error">
        {rich("Couldn't load tasks: {0}{1}", error, onRetry ? (
          <>
            {" "}
            <Button type="button" variant="outline" size="sm" className="btn btn-small" onClick={onRetry}>
              {tr("Retry")}
            </Button>
          </>
        ) : null)}
      </span>
    );
  }
  if (loading) {
    return <span data-testid="tasks-loading">{tr("Loading tasks…")}</span>;
  }
  if (anyFilter) {
    return (
      <span data-testid="tasks-no-match">
        {rich("No tasks match the current filters.{0}", onClearFilters ? (
          <>
            {" "}
            <Button type="button" variant="outline" size="sm" className="btn btn-small" onClick={onClearFilters}>
              {tr("Clear filters")}
            </Button>
          </>
        ) : null)}
      </span>
    );
  }
  return <>{tr("No tasks created yet")}</>;
}
