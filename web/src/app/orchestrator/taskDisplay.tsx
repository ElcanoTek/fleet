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
    "The run completed without publishing: its completion clause recorded a blocked outcome."
  );
}

// TaskStatusBadge is a task's status pill: its lifecycle status, except that
// a Blocked run reads "blocked" in amber, with the reason on hover, instead of
// a green success that hides a dashboard which stopped updating.
export function TaskStatusBadge({ task }: { task: Task }) {
  const blocked = blockedReason(task);
  if (blocked) {
    return (
      <span
        className="status-badge status-blocked"
        title={`Blocked: ${blocked}`}
        data-testid="status-blocked"
      >
        blocked
      </span>
    );
  }
  return (
    <span className={`status-badge status-${task.status ?? "unknown"}`}>
      {task.status ?? "-"}
    </span>
  );
}

// scheduleStoppedReason is why a recurring task's schedule stopped at this
// occurrence (the dead-letter breaker parked it), or null when it did not.
// An older parked row may carry no recorded reason.
export function scheduleStoppedReason(task: Task): string | null {
  if (!task.recurrence || !task.recurrence_parked_at) return null;
  return (
    (task.recurrence_parked_reason ?? "").trim() ||
    "The schedule was stopped after dead-lettered runs. Replay this run to resume it; if its EXECUTION REQUIREMENTS line is malformed, replay it with a corrected prompt instead."
  );
}

// scheduleTitle is the hover text of a schedule cell: the stop reason for a
// stopped schedule, else the exact cron expression.
export function scheduleTitle(task: Task): string | undefined {
  const stopped = scheduleStoppedReason(task);
  if (stopped) return `Schedule stopped: ${stopped}`;
  return task.recurrence || undefined;
}

export function scheduleLabel(task: Task): string {
  if (scheduleStoppedReason(task)) return "⏹ Schedule stopped";
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
