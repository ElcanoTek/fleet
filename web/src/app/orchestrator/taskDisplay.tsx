import { t as tr } from "@/app/lib/i18n";
import type { Task, TaskStatus } from "@/app/shared/lib/orchestratorApi";
import { formatTimeFirst, truncate } from "@/app/shared/lib/format";
import { RunStatusChip, StatusChip, type RunTone } from "@/app/shared/ui/StatusChip";
import type { ReactNode } from "react";
import {
  describeCronExpression,
  describeCronExpressionShort,
} from "@/app/shared/lib/cron";

// Shared task-display helpers for the Operations Center. These produce the
// exact labels the Recent Tasks table shows, so the log modal's task summary
// (#TBD) can mirror the row the user clicked without duplicating the logic.

// --- Task status classification -------------------------------------------
// One home for the lifecycle groupings the table, the log modal and the edit
// form act on. The statuses mirror internal/sched/models.TaskStatus; the
// predicates take `string | undefined` because a row from an older server (or
// a stream frame) may carry no status, which every predicate treats as
// "unknown" exactly as the per-file sets it replaces did.

export type { TaskStatus };

// The finished statuses (models.TaskStatus.IsTerminal): editing one resubmits
// a fresh run instead of rewriting history.
export const TERMINAL_TASK_STATUSES: ReadonlySet<string> = new Set<TaskStatus>([
  "success",
  "error",
  "cancelled",
  "dead_lettered",
]);

// Statuses that can be kicked off on demand ("Run now"/Resubmit) and edited:
// the terminal ones plus the not-yet-run ones. In-flight (leased/running) and
// paused runs are excluded — a second concurrent copy of a running job is a
// footgun, not a feature.
const RUNNABLE_TASK_STATUSES: ReadonlySet<string> = new Set<TaskStatus>([
  ...(TERMINAL_TASK_STATUSES as ReadonlySet<TaskStatus>),
  "pending",
  "scheduled",
]);

// Statuses with something left to stop: everything that has not finished,
// including pending/scheduled (a recurring job that has not fired, #1152).
const STOPPABLE_TASK_STATUSES: ReadonlySet<string> = new Set<TaskStatus>([
  "pending",
  "scheduled",
  "leased",
  "running",
  "paused_awaiting_input",
  "paused_awaiting_wake",
]);

// A live run: a worker holds the lease and is writing to the row.
const LIVE_TASK_STATUSES: ReadonlySet<string> = new Set<TaskStatus>(["leased", "running"]);

export function isTerminal(status: string | undefined): boolean {
  return TERMINAL_TASK_STATUSES.has(status ?? "");
}

export function isRunnable(status: string | undefined): boolean {
  return RUNNABLE_TASK_STATUSES.has(status ?? "");
}

export function isStoppable(status: string | undefined): boolean {
  return STOPPABLE_TASK_STATUSES.has(status ?? "");
}

export function isLive(status: string | undefined): boolean {
  return LIVE_TASK_STATUSES.has(status ?? "");
}

// Everything except a live run can be deleted (the server refuses a leased or
// running task), including a row whose status is unknown.
export function isDeletable(status: string | undefined): boolean {
  return !isLive(status);
}

export function createdByLabel(task: Task): string {
  if (task.created_by_username) return task.created_by_username;
  if (!task.created_by) return "—";
  const uuid =
    /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
  if (uuid.test(task.created_by))
    return tr("user: {0}…", [task.created_by.slice(0, 6)]);
  return task.created_by;
}

// taskRunLabel is the short human name for a task — the title when it has one,
// and otherwise the prompt's first non-empty line (which is where operators put
// a title before the field existed). The short ID is the last resort, for an
// untitled task whose prompt starts with something unprintable.
export function taskRunLabel(task: Task, maxLength = 60): string {
  const title = (task.title ?? "").trim();
  if (title) return truncate(title, maxLength);
  const named = promptName(task.prompt ?? "");
  if (named) return truncate(named, maxLength);
  const firstLine = (task.prompt ?? "")
    .split("\n")
    .map((line) => line.trim())
    .find((line) => line !== "");
  return firstLine ? truncate(firstLine, maxLength) : task.id.slice(0, 8);
}

// promptName is the name a workflow prompt gives itself: a top-level YAML
// `name:` line near the start ("name: \"Weekly Health Scan\""). Scheduled
// prompts often open with a shared preamble heading, so their first line is the
// same for every task; the workflow's own name is what tells them apart.
export function promptName(prompt: string): string {
  const head = prompt.split("\n", 200);
  for (const line of head) {
    const m = /^name:\s*(["']?)(.+?)\1\s*$/.exec(line);
    if (m && m[2].trim()) return m[2].trim();
  }
  return "";
}

// blockedReason is why a run finished Blocked — a success whose declared
// completion clause recorded a blocked outcome (run_outcome "blocked") — or
// null for every other run. The detail is the recording call's own outcome
// value and explanation.
export function blockedReason(task: Task): string | null {
  if (task.status !== "success" || task.run_outcome !== "blocked") return null;
  return (
    (task.run_outcome_detail ?? "").trim() ||
    tr("The run completed without publishing: its completion clause recorded a blocked outcome.")
  );
}

// TaskStatusBadge is a task's status pill: its lifecycle status, except that
// a Blocked run reads "blocked" in amber, with the reason on hover, instead of
// a green success that hides a dashboard which stopped updating.
export function TaskStatusBadge({ task }: { task: Task }) {
  const blocked = blockedReason(task);
  if (blocked) {
    return (
      <RunStatusChip state="blocked" title={tr("Blocked: {0}", [blocked])} testId="status-blocked" />
    );
  }
  return <RunStatusChip state={task.status ?? "unknown"}>{task.status ?? "-"}</RunStatusChip>;
}

// scheduleStoppedReason is why a recurring task's schedule stopped at this
// occurrence (the dead-letter breaker parked it), or null when it did not.
// An older parked row may carry no recorded reason.
export function scheduleStoppedReason(task: Task): string | null {
  if (!task.recurrence || !task.recurrence_parked_at) return null;
  return (
    (task.recurrence_parked_reason ?? "").trim() ||
    tr("The schedule was stopped after dead-lettered runs. Replay this run to resume it; if its EXECUTION REQUIREMENTS line is malformed, replay it with a corrected prompt instead.")
  );
}

// scheduleTitle is the hover text of a schedule cell: the stop reason for a
// stopped schedule, else the exact cron expression.
export function scheduleTitle(task: Task): string | undefined {
  const stopped = scheduleStoppedReason(task);
  if (stopped) return tr("Schedule stopped: {0}", [stopped]);
  return task.recurrence || undefined;
}

export function scheduleLabel(task: Task): string {
  if (scheduleStoppedReason(task)) return tr("⏹ Schedule stopped");
  if (task.recurrence) {
    // Compact plain English, not raw cron ("9:00 AM · Sat, Sun", not
    // "0 9 * * 6,0"), falling back to the verbose description and then the
    // raw expression; the exact cron stays in the cell's title.
    const described =
      describeCronExpressionShort(task.recurrence) ||
      describeCronExpression(task.recurrence);
    return `🔄 ${described || task.recurrence}`;
  }
  if (task.scheduled_for) return `⏰ ${formatTimeFirst(task.scheduled_for)}`;
  return "-";
}

// slaBadge returns {label, tone} when a task carries SLA state worth surfacing
// (#274), or null when the task has no SLA / no breach. tone is the CSS class
// suffix appended to `sla-badge-` (amber for warn, red for fail).
export function slaBadge(task: Task): { label: string; tone: string } | null {
  if (!task.expected_duration_minutes) return null;
  if (task.sla_breached) return { label: tr("SLA breached"), tone: "fail" };
  // Without a live elapsed probe on the client, the in-progress warn state is
  // surfaced only when the server has already latched a breach. A running task
  // that has merely CROSSED the warn threshold (but not fail) is not latched,
  // so we don't render a spurious warn here; the SLA tab carries the live view.
  return null;
}

// TaskSlaBadge renders the SLA cell: breach badge, ok ratio, or a muted dash.
export function TaskSlaBadge({ task }: { task: Task }) {
  const badge = slaBadge(task);
  if (badge) {
    return (
      <SlaChip tone={badge.tone} title={badge.label}>
        {badge.label}
      </SlaChip>
    );
  }
  if (task.expected_duration_minutes) {
    return (
      <SlaChip tone="ok" title={tr("Expected {0}m", [task.expected_duration_minutes])}>
        {task.actual_duration_seconds != null
          ? tr("{0}m / {1}m", [Math.round(task.actual_duration_seconds / 60), task.expected_duration_minutes])
          : `${task.expected_duration_minutes}m`}
      </SlaChip>
    );
  }
  return <span className="sla-badge sla-badge-none text-[var(--color-text-muted)]">—</span>;
}

// SlaChip is an SLA state (ok / warn / fail) on the shared status chip. The
// `sla-badge sla-badge-<tone>` classes are unstyled test hooks.
const SLA_TONE: Record<string, RunTone> = {
  ok: "run-success",
  warn: "run-warning",
  fail: "run-error",
};
export function SlaChip({
  tone,
  title,
  children,
}: {
  tone: string;
  title?: string;
  children: ReactNode;
}) {
  return (
    <StatusChip
      tone={SLA_TONE[tone] ?? "neutral"}
      title={title}
      className={`sla-badge sla-badge-${tone}`}
    >
      {children}
    </StatusChip>
  );
}
