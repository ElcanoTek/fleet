import type { Task } from "@/app/shared/lib/orchestratorApi";
import { formatTimeFirst, truncate } from "@/app/shared/lib/format";
import {
  describeCronExpression,
  describeCronExpressionShort,
} from "@/app/shared/lib/cron";

// Shared task-display helpers for the Operations Center. These produce the
// exact labels the Recent Tasks table shows, so the log modal's task summary
// (#TBD) can mirror the row the user clicked without duplicating the logic.

export function createdByLabel(task: Task): string {
  if (task.created_by_username) return task.created_by_username;
  if (!task.created_by) return "—";
  const uuid =
    /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
  if (uuid.test(task.created_by))
    return `user: ${task.created_by.slice(0, 6)}…`;
  return task.created_by;
}

// taskRunLabel is the short human name for a task in prose — confirm dialogs,
// toasts. Operators put a title line at the head of the prompt because there is
// nowhere else to put one, so the prompt's first non-empty line is the closest
// thing to a name a task currently has; the short ID is the fallback for a task
// whose prompt starts with something unprintable.
export function taskRunLabel(task: Task, maxLength = 60): string {
  const firstLine = (task.prompt ?? "")
    .split("\n")
    .map((line) => line.trim())
    .find((line) => line !== "");
  return firstLine ? truncate(firstLine, maxLength) : task.id.slice(0, 8);
}

export function scheduleLabel(task: Task): string {
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
  if (task.sla_breached) return { label: "SLA breached", tone: "fail" };
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
      <span className={`sla-badge sla-badge-${badge.tone}`} title={badge.label}>
        {badge.label}
      </span>
    );
  }
  if (task.expected_duration_minutes) {
    return (
      <span
        className="sla-badge sla-badge-ok"
        title={`Expected ${task.expected_duration_minutes}m`}
      >
        {task.actual_duration_seconds != null
          ? `${Math.round(task.actual_duration_seconds / 60)}m / ${task.expected_duration_minutes}m`
          : `${task.expected_duration_minutes}m`}
      </span>
    );
  }
  return <span className="sla-badge sla-badge-none">—</span>;
}
