"use client";

import { useCallback } from "react";
import { orchestratorApi, type UpcomingRun } from "@/app/shared/lib/orchestratorApi";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { describeCronExpression } from "@/app/shared/lib/cron";

// UpcomingPanel — the Operations Center "Upcoming" tab (Scheduler UX 2.0, #504):
// a forward-looking timeline of the next scheduled runs, grouped by calendar
// day. Each recurring task contributes up to its next few cron occurrences and
// each one-shot its single scheduled time, computed server-side by
// GET /tasks/upcoming (cron.Next in the task's timezone). Read-only: it answers
// "what is fleet about to do?" without opening each task.

// dayKey/dayLabel bucket runs into local calendar days for the grouped headers.
function dayKey(d: Date): string {
  return `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}`;
}

function dayLabel(d: Date): string {
  const today = new Date();
  const tomorrow = new Date(today);
  tomorrow.setDate(today.getDate() + 1);
  if (dayKey(d) === dayKey(today)) return "Today";
  if (dayKey(d) === dayKey(tomorrow)) return "Tomorrow";
  return d.toLocaleDateString(undefined, {
    weekday: "long",
    month: "short",
    day: "numeric",
  });
}

function timeLabel(d: Date): string {
  return d.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
}

export function UpcomingPanel() {
  const {
    data,
    loading,
    error,
  } = useCancellableFetch(
    useCallback(() => orchestratorApi.upcomingRuns(50), []),
    [],
  );

  const runs = data?.upcoming ?? [];

  return (
    <div className="section" role="region" aria-labelledby="upcomingHeading">
      <div className="section-header">
        <h2 id="upcomingHeading">Upcoming Runs</h2>
      </div>

      {loading ? (
        <div className="loading">
          <p>Loading upcoming runs…</p>
        </div>
      ) : error ? (
        <div className="table-error">Failed to load upcoming runs: {error}</div>
      ) : runs.length === 0 ? (
        <div className="table-empty">
          No upcoming runs. Recurring tasks and future one-shot schedules appear here.
        </div>
      ) : (
        <UpcomingTimeline runs={runs} />
      )}
    </div>
  );
}

function UpcomingTimeline({ runs }: { runs: UpcomingRun[] }) {
  // The API already returns runs sorted by next_run ascending; group them into
  // consecutive calendar-day buckets, preserving that order.
  const groups: { label: string; key: string; runs: UpcomingRun[] }[] = [];
  for (const run of runs) {
    const when = new Date(run.next_run);
    const key = dayKey(when);
    const last = groups[groups.length - 1];
    if (last && last.key === key) {
      last.runs.push(run);
    } else {
      groups.push({ key, label: dayLabel(when), runs: [run] });
    }
  }

  return (
    <div className="table-wrapper" data-testid="upcoming-timeline">
      {groups.map((group) => (
        <div key={group.key} className="upcoming-day-group">
          <h3 className="upcoming-day-header">{group.label}</h3>
          <ul className="upcoming-run-list">
            {group.runs.map((run, i) => {
              const when = new Date(run.next_run);
              return (
                <li key={`${run.task_id}-${i}`} className="upcoming-run-row" data-testid="upcoming-run-row">
                  <span className="upcoming-run-time">{timeLabel(when)}</span>
                  <span className="upcoming-run-name" title={run.prompt}>
                    {run.name || run.prompt.slice(0, 80) || "(untitled task)"}
                  </span>
                  <span className={`upcoming-run-kind upcoming-run-kind-${run.recurring ? "recurring" : "oneshot"}`}>
                    {run.recurring ? describeCronExpression(run.recurrence) : "One-time"}
                  </span>
                </li>
              );
            })}
          </ul>
        </div>
      ))}
      <p className="refresh-note">Next {runs.length} scheduled run(s)</p>
    </div>
  );
}

export default UpcomingPanel;
