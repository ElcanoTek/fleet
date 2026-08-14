"use client";

import { useCallback } from "react";
import type { Task } from "@/app/shared/lib/orchestratorApi";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { formatTimeFirst } from "@/app/shared/lib/format";
import { taskRunLabel } from "./taskDisplay";

// SleepingTasks — thin list of tasks parked in paused_awaiting_wake
// (docs/SELF-WAKE.md). Fire-event lives on the log viewer; this is just the
// operator queue so sleeping work is visible without a status-filter click.

export type SleepingTasksProps = {
  onOpen: (task: Task) => void;
};

export function SleepingTasks({ onOpen }: SleepingTasksProps) {
  const { data } = useCancellableFetch(
    useCallback(
      () => orchestratorApi.tasks("status=paused_awaiting_wake&limit=20&offset=0"),
      [],
    ),
    [],
  );
  const tasks = data?.data ?? [];
  if (tasks.length === 0) return null;
  return (
    <div className="sleeping-tasks" data-testid="sleeping-tasks" role="region" aria-label="Sleeping tasks">
      <div className="sleeping-tasks-head">
        <h3>Sleeping</h3>
        <span className="sleeping-tasks-count">{tasks.length}</span>
      </div>
      <ul className="sleeping-tasks-list">
        {tasks.map((t) => (
          <li key={t.id}>
            <button
              type="button"
              className="sleeping-task-row"
              data-testid="sleeping-task-row"
              onClick={() => onOpen(t)}
            >
              <span className="sleeping-task-title">{taskRunLabel(t, 60)}</span>
              <span className="sleeping-task-meta">
                {t.wake_event_key
                  ? `waiting for “${t.wake_event_key}”`
                  : "timer"}
                {t.wake_at ? ` · wakes ${formatTimeFirst(t.wake_at)}` : ""}
              </span>
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}
