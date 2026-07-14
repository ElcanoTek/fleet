"use client";

import { useCallback, useState } from "react";
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

type UpcomingView = "list" | "week";

const VIEWS: Array<{ id: UpcomingView; label: string }> = [
  { id: "list", label: "List" },
  { id: "week", label: "Week" },
];

export function UpcomingPanel() {
  const {
    data,
    loading,
    error,
  } = useCancellableFetch(
    useCallback(() => orchestratorApi.upcomingRuns(50), []),
    [],
  );
  // View toggle: the chronological list (default) or a 7-day week board.
  // Designed so a month grid can slot in as a third view later.
  const [view, setView] = useState<UpcomingView>("list");

  const runs = data?.upcoming ?? [];

  return (
    <div className="section" role="region" aria-labelledby="upcomingHeading">
      <div className="section-header">
        <h2 id="upcomingHeading">Upcoming Runs</h2>
        <div className="task-segment" role="radiogroup" aria-label="Upcoming view">
          {VIEWS.map((v) => (
            <button
              key={v.id}
              type="button"
              role="radio"
              aria-checked={view === v.id}
              tabIndex={view === v.id ? 0 : -1}
              className={`task-segment-btn${view === v.id ? " is-active" : ""}`}
              data-testid={`upcoming-view-${v.id}`}
              onClick={() => setView(v.id)}
            >
              {v.label}
            </button>
          ))}
        </div>
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
      ) : view === "week" ? (
        <UpcomingWeek runs={runs} />
      ) : (
        <UpcomingTimeline runs={runs} />
      )}
    </div>
  );
}

// UpcomingWeek — the next 7 days as columns (today first), each listing that
// day's runs in order. A rolling window rather than a calendar week: it
// matches the "what is fleet about to do?" horizon and never opens on a
// mostly-spent week. Runs beyond the window are summarized under the board.
function UpcomingWeek({ runs }: { runs: UpcomingRun[] }) {
  const today = new Date();
  const days: Array<{ key: string; date: Date; runs: UpcomingRun[] }> = [];
  for (let i = 0; i < 7; i++) {
    const d = new Date(today.getFullYear(), today.getMonth(), today.getDate() + i);
    days.push({ key: dayKey(d), date: d, runs: [] });
  }
  const byKey = new Map(days.map((d) => [d.key, d]));
  let beyond = 0;
  for (const run of runs) {
    const bucket = byKey.get(dayKey(new Date(run.next_run)));
    if (bucket) bucket.runs.push(run);
    else beyond++;
  }

  return (
    <div className="upcoming-week-wrapper" data-testid="upcoming-week">
      <div className="upcoming-week">
        {days.map((day, i) => (
          <div
            key={day.key}
            className={`upcoming-week-day${i === 0 ? " upcoming-week-day--today" : ""}`}
          >
            <div className="upcoming-week-day-header">
              <span className="upcoming-week-day-name">
                {i === 0
                  ? "Today"
                  : day.date.toLocaleDateString(undefined, { weekday: "short" })}
              </span>
              <span className="upcoming-week-day-date">
                {day.date.toLocaleDateString(undefined, { month: "numeric", day: "numeric" })}
              </span>
            </div>
            {day.runs.length === 0 ? (
              <div className="upcoming-week-empty">—</div>
            ) : (
              <ul className="upcoming-week-list">
                {day.runs.map((run, j) => {
                  const when = new Date(run.next_run);
                  return (
                    <li
                      key={`${run.task_id}-${j}`}
                      className={`upcoming-week-run upcoming-week-run--${run.recurring ? "recurring" : "oneshot"}`}
                      title={run.prompt}
                      data-testid="upcoming-week-run"
                    >
                      <span className="upcoming-week-run-time">{timeLabel(when)}</span>
                      <span className="upcoming-week-run-name">
                        {run.name || run.prompt.slice(0, 60) || "(untitled task)"}
                      </span>
                    </li>
                  );
                })}
              </ul>
            )}
          </div>
        ))}
      </div>
      <p className="refresh-note">
        {beyond > 0
          ? `Next 7 days · ${beyond} more scheduled run(s) beyond this window — see List`
          : "Next 7 days"}
      </p>
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
